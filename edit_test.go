package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemp drops a file in the sandbox and returns its relative name.
func writeTemp(t *testing.T, root, name, content string) string {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return name
}

func readTemp(t *testing.T, root, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// editCall is the tool call a model would send.
func editCall(path, old, new string) ToolCall {
	return callTool("edit_file", map[string]any{"path": path, "old": old, "new": new})
}

func TestEditFileReplacesOnePlaceAndReportsIt(t *testing.T) {
	root := t.TempDir()
	src := "package main\n\nfunc f() {\n\treturn 1\n}\n"
	writeTemp(t, root, "a.go", src)

	out := runTool(context.Background(), root, editCall("a.go", "\treturn 1", "\treturn 2"), 0, nil)
	if !strings.HasPrefix(out, "OK: edited a.go") {
		t.Fatalf("not reported as an edit: %q", out)
	}
	if !strings.Contains(out, "line 4") {
		t.Errorf("the report does not say where the change landed: %q", out)
	}
	if !strings.Contains(out, "byte-identical") && !strings.Contains(out, "unchanged") {
		t.Errorf("the report does not say the rest of the file is untouched: %q", out)
	}
	want := "package main\n\nfunc f() {\n\treturn 2\n}\n"
	if got := readTemp(t, root, "a.go"); got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
}

// The rule the whole tool rests on. A search text that matches twice has not
// identified a place, and replacing the first of them is how a patch tool
// corrupts a file while reporting success.
func TestEditFileRefusesAnAmbiguousMatch(t *testing.T) {
	root := t.TempDir()
	src := "a := 1\nb := 2\na := 1\n"
	writeTemp(t, root, "dup.txt", src)

	out := runTool(context.Background(), root, editCall("dup.txt", "a := 1", "a := 9"), 0, nil)
	if !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("an ambiguous match was accepted: %q", out)
	}
	for _, want := range []string{"appears 2 times", "lines 1, 3", "nothing was written", `"all": true`} {
		if !strings.Contains(out, want) {
			t.Errorf("the refusal does not mention %q: %q", want, out)
		}
	}
	if got := readTemp(t, root, "dup.txt"); got != src {
		t.Fatalf("a refused edit still changed the file: %q", got)
	}
}

func TestEditFileAllReplacesEveryOccurrence(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "dup.txt", "x = old\ny = old\n")

	out := runTool(context.Background(), root, callTool("edit_file", map[string]any{
		"path": "dup.txt", "old": "old", "new": "new", "all": true,
	}), 0, nil)
	if !strings.HasPrefix(out, "OK:") {
		t.Fatalf("all:true was refused: %q", out)
	}
	if got := readTemp(t, root, "dup.txt"); got != "x = new\ny = new\n" {
		t.Fatalf("file is %q", got)
	}
}

// A global edit reports the number it actually replaced, not the number of line
// numbers the report had room for: an understated count is a bad thing to hand a
// model that is about to tell the user what it did.
func TestEditFileReportsTheFullCountPastTheLineCap(t *testing.T) {
	root := t.TempDir()
	var src strings.Builder
	for i := 0; i < 12; i++ {
		src.WriteString("item old\n")
	}
	writeTemp(t, root, "many.txt", src.String())

	out := runTool(context.Background(), root, callTool("edit_file", map[string]any{
		"path": "many.txt", "old": "old", "new": "new", "all": true,
	}), 0, nil)
	if !strings.Contains(out, "replaced 12 occurrences") {
		t.Fatalf("the report understates how many places changed: %q", out)
	}
	if !strings.Contains(out, "and 4 more") {
		t.Fatalf("the report hides how much of the list it left out: %q", out)
	}
	if got := strings.Count(readTemp(t, root, "many.txt"), "item new"); got != 12 {
		t.Fatalf("%d of 12 lines were replaced", got)
	}
}

func TestEditFileReportsAMultiLineSpan(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "m.txt", "one\ntwo\nthree\n")

	out := runTool(context.Background(), root, editCall("m.txt", "one\ntwo", "1\n2\n2a"), 0, nil)
	if !strings.Contains(out, "held 2 lines and now holds 3") {
		t.Fatalf("a span change is not described: %q", out)
	}
	if got := readTemp(t, root, "m.txt"); got != "1\n2\n2a\nthree\n" {
		t.Fatalf("file is %q", got)
	}
}

// The most common false negative: the file was written on Windows with CRLF and
// the text in the call has LF, because that is what a model types. Failing there
// sends the model to re-read a file whose text it already had in front of it.
func TestEditFileMatchesTheFilesLineEndings(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "crlf.txt", "alpha\r\nbeta\r\ngamma\r\n")

	out := runTool(context.Background(), root, editCall("crlf.txt", "beta\ngamma", "beta\nGAMMA"), 0, nil)
	if !strings.HasPrefix(out, "OK:") {
		t.Fatalf("a CRLF file was not matched by LF search text: %q", out)
	}
	if !strings.Contains(out, "line endings") {
		t.Errorf("the report does not say it used the file's convention: %q", out)
	}
	got := readTemp(t, root, "crlf.txt")
	if got != "alpha\r\nbeta\r\nGAMMA\r\n" {
		t.Fatalf("file is %q, want every line ending still CRLF", got)
	}
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Fatalf("the edit left an LF-only line in a CRLF file: %q", got)
	}
}

// Everything outside the match is untouched by construction, and this pins it:
// no BOM added, no trailing newline invented or lost, CRLF and unicode kept.
func TestEditFileLeavesEveryOtherByteAlone(t *testing.T) {
	root := t.TempDir()
	src := "\ufeffline one\r\nline two \u2013 dash\r\nno trailing newline"
	writeTemp(t, root, "keep.txt", src)

	out := runTool(context.Background(), root, editCall("keep.txt", "line two \u2013 dash", "line 2 \u2013 dash"), 0, nil)
	if !strings.HasPrefix(out, "OK:") {
		t.Fatalf("unicode edit refused: %q", out)
	}
	want := "\ufeffline one\r\nline 2 \u2013 dash\r\nno trailing newline"
	if got := readTemp(t, root, "keep.txt"); got != want {
		t.Fatalf("file is %q, want %q", got, want)
	}
}

func TestEditFileSaysWhenOnlyWhitespaceDiffers(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "ws.go", "func f() {\n    return 1\n}\n")

	out := runTool(context.Background(), root, editCall("ws.go", "\t\treturn 1", "\t\treturn 2"), 0, nil)
	if !strings.HasPrefix(out, "ERROR:") {
		t.Fatalf("an unmatched search was accepted: %q", out)
	}
	if !strings.Contains(out, "whitespace") || !strings.Contains(out, "Nothing was written") {
		t.Fatalf("the refusal does not explain the likely cause: %q", out)
	}
	if got := readTemp(t, root, "ws.go"); got != "func f() {\n    return 1\n}\n" {
		t.Fatalf("a refused edit changed the file: %q", got)
	}
}

func TestEditFileDeletesWhenTheReplacementIsEmpty(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "del.txt", "keep\nremove me\nkeep\n")

	out := runTool(context.Background(), root, editCall("del.txt", "remove me\n", ""), 0, nil)
	if !strings.HasPrefix(out, "OK:") {
		t.Fatalf("deletion refused: %q", out)
	}
	if !strings.Contains(out, "removed") {
		t.Errorf("the report does not say the text was removed: %q", out)
	}
	if got := readTemp(t, root, "del.txt"); got != "keep\nkeep\n" {
		t.Fatalf("file is %q", got)
	}
}

// A bad argument is a sentence about what to send instead, not a panic and not a
// half-applied edit.
func TestEditFileRefusesBadArguments(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "f.txt", "hello\n")

	cases := []struct {
		name string
		tc   ToolCall
		want []string
	}{
		{"no path", callTool("edit_file", map[string]any{"old": "hello", "new": "bye"}),
			[]string{`"path"`, "write_file"}},
		{"empty search", callTool("edit_file", map[string]any{"path": "f.txt", "old": "", "new": "x"}),
			[]string{`"old" is empty`, "matches everywhere"}},
		{"whitespace search", callTool("edit_file", map[string]any{"path": "f.txt", "old": "  \n", "new": "x"}),
			[]string{"only whitespace"}},
		{"no change", callTool("edit_file", map[string]any{"path": "f.txt", "old": "hello", "new": "hello"}),
			[]string{"identical", "would change nothing"}},
		{"bad json", brokenJSONCall(),
			[]string{"BAD ARGS"}},
	}
	for _, c := range cases {
		out := runTool(context.Background(), root, c.tc, 0, nil)
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: %q does not mention %q", c.name, out, want)
			}
		}
	}
	if got := readTemp(t, root, "f.txt"); got != "hello\n" {
		t.Fatalf("a refused call changed the file: %q", got)
	}
}

// brokenJSONCall is the shape the API sends when a model truncates its arguments
// mid-object: the tool has to answer with a sentence, not a panic.
func brokenJSONCall() ToolCall {
	tc := ToolCall{ID: "c1", Type: "function"}
	tc.Function.Name = "edit_file"
	tc.Function.Arguments = `{"path": `
	return tc
}

func TestEditFileNeedsAnExistingTextFile(t *testing.T) {
	root := t.TempDir()
	writeTemp(t, root, "bin.dat", "text\x00bytes")
	if err := os.Mkdir(filepath.Join(root, "folder"), 0755); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		path string
		want []string
	}{
		{"missing", "nope.txt", []string{"there is no file at nope.txt", "write_file"}},
		{"folder", "folder", []string{"is a folder", "list_dir"}},
		{"binary", "bin.dat", []string{"NUL bytes", "not a text file"}},
	}
	for _, c := range cases {
		out := runTool(context.Background(), root, editCall(c.path, "x", "y"), 0, nil)
		for _, want := range c.want {
			if !strings.Contains(out, want) {
				t.Errorf("%s: %q does not mention %q", c.name, out, want)
			}
		}
	}
}

// The sandbox is the same one write_file uses, junctions included: an edit is a
// write, so it is resolved and re-checked like one.
func TestEditFileStaysInsideTheWorkspace(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "escaped.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0644); err != nil {
		t.Fatal(err)
	}

	out := runTool(context.Background(), root, editCall("../escaped.txt", "secret", "pwned"), 0, nil)
	if !strings.HasPrefix(out, "ERROR:") || !strings.Contains(out, "../escaped.txt") {
		t.Fatalf("a path outside the workspace was not refused in words: %q", out)
	}
	if got, _ := os.ReadFile(outside); string(got) != "secret\n" {
		t.Fatalf("a file outside the sandbox was changed: %q", got)
	}
}

// edit_file is a write, so it is offered exactly where write_file is, it always
// stops at the approval card, and the risk layer judges its destination by the
// same rules - an edit into .git/hooks is not less dangerous for being one line.
func TestEditFileIsGatedLikeWriteFile(t *testing.T) {
	for _, mode := range []string{ModeAgent, ModePlan, ModeAsk} {
		offered := false
		wantOffered := mode == ModeAgent
		for _, tl := range toolSchemas(mode) {
			if tl["function"].(map[string]any)["name"] == "edit_file" {
				offered = true
			}
		}
		if offered != wantOffered {
			t.Errorf("%s: edit_file offered = %v, want %v", mode, offered, wantOffered)
		}
		if _, denied := modeDenies(mode, "edit_file"); denied != !wantOffered {
			t.Errorf("%s: modeDenies(edit_file) = %v, want %v", mode, denied, !wantOffered)
		}
	}
	if !needsApproval("edit_file") {
		t.Error("a mutation that rewrites a file must be shown to the user first")
	}

	paths := []string{
		".git/hooks/pre-commit",
		".env",
		"sub/.ssh/authorized_keys",
		"api_key_deepseek.bin",
		"package.json",
		"go.mod",
		"cmd/main.go",
	}
	for _, p := range paths {
		wr := classifyTool("write_file", `{"path":"`+p+`"}`)
		ed := classifyTool("edit_file", `{"path":"`+p+`"}`)
		if wr.Level != ed.Level {
			t.Errorf("%s: write_file is %q and edit_file is %q; the two must agree", p, wr.Level, ed.Level)
		}
	}
	if classifyTool("edit_file", `{"path":".git/hooks/pre-commit"}`).Level != riskDangerous {
		t.Error("an edit inside .git is not classified dangerous")
	}
}
