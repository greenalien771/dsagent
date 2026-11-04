# Development

## The checks

```sh
go vet ./...
go test ./...
```

`go vet` type-checks only the target it is built for, so a clean run on Windows
says nothing about darwin or linux - an untagged `SysProcAttr.CmdLine` once passed
`go vet` on Windows and failed on the first unix runner it reached. Vet the other
targets too:

```sh
GOOS=darwin go vet ./...
GOOS=windows go vet ./...
```

`go vet` builds nothing, links nothing and runs nothing, so `go build` and
`go test` are the steps that actually prove the tree compiles and works.

## Rebuilding on Windows

The running `dsagent.exe` is locked by the OS, so `go build -o dsagent.exe .` fails
with access-denied while the agent is running. Either stop dsagent first, or build
somewhere else to prove compilation:

```sh
go build -o "%TEMP%\check.exe" .
```

Never move, rename or delete the executable to "release the lock" - whoever is
holding it is holding it for a reason.

## The test suite

The Go tests are plain `_test.go` files beside the code. What they are worth
knowing for:

- **End-to-end, on a loopback port** - `abort_e2e_test.go` runs a real server so the
  abort path is tested through HTTP rather than through a mock.
- **Real windows, on Windows** - `window_e2e_test.go` launches Notepad, waits for it,
  captures it while minimised and closes it. Gated on `GOOS` and skipped elsewhere,
  because the desktop tools are.
- **Notifications** - `notify_windows_test.go` and `notify_e2e_test.go`.
- **Thresholds rather than exact strings** where a message is prose:
  `schema_test.go`, `errors_test.go`, `finish_test.go`.
- **The platform boundary** - `dropDesktopTools` takes `goos` as an argument instead
  of reading `runtime.GOOS`, so the non-Windows path can be tested from the one
  platform where the mistake it prevents is invisible.

`testdata/` holds fixtures, including images for the vision tests.

## CI

`.github/workflows/ci.yml` runs on `ubuntu-latest`, `windows-latest` and
`macos-15-intel` (the arm64 pool is the capacity-constrained one), pinned to the Go
version in `go.mod` so the stated minimum is the version actually exercised:

1. `gofmt -l .` - must print nothing,
2. `go vet ./...` plus the cross-vet of darwin and windows from Linux,
3. `go build ./...`,
4. `go test ./...`.

## The vendored front end

`ui/vendor/` holds marked 18.1.1, DOMPurify 3.4.17, highlight.js 11.12.0 and its
GitHub Dark theme, with SHA-256 hashes and byte-for-byte license copies recorded in
`ui/vendor/README.md`.

To update one:

1. Replace the artifact, **keeping the local filename** - `ui/index.html`
   hard-codes all four paths, and the theme is renamed locally
   (`highlight-github-dark.css`); change that name and the stylesheet silently
   stops loading.
2. Replace the matching `LICENSE-*.txt` if the license set changed.
3. Update the version column in `ui/vendor/README.md` and the version comment at
   the top of `ui/index.html`.
4. Recompute the SHA-256 block.
5. Rebuild.

## Style that the tree enforces on itself

- Comments explain **why**, and they are allowed to be long. Every non-obvious
  decision in this tree carries the failure it prevents.
- A refusal carries the reason and the way out: "path escapes working dir (link to
  ...)", "the watchdog limit is 5-3600; raise `shell_timeout_sec`".
- Nothing is offered that would only be refused.
- A new setting needs a bound, so no single field can switch a safety net off.
