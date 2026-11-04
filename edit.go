package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// Limits for one edit. The size cap exists because the file is read into memory
// whole to count matches before anything is replaced; a file past it is a job for
// a shell command and an editor that streams, not for a tool call.
const (
	maxEditBytes = 8 << 20
	// maxEditLines is how many matched line numbers the refusal lists. A string
	// that matches forty places needs one more line of context, not forty numbers.
	maxEditLines = 8
	// maxEditShown is the width of the before/after line in the result.
	maxEditShown = 160
)

// editFileTool performs one exact-string replacement in a text file.
//
// It exists because write_file can only overwrite a whole file, so patching one
// line of a 900-line file meant sending the whole thing out and back - and, worse,
// rewriting every byte the task never asked to touch, which is exactly what the
// system prompt forbids. An exact match replaced in place leaves every other byte
// of the file as it was, by construction: no re-encoding, no line-ending
// normalisation, no reformatting, no lost trailing newline.
//
// The rule that makes it safe rather than merely convenient is the count. `old`
// must identify exactly one place in the file unless `all` says otherwise: a
// string that matches twice has not identified anything, and replacing the first
// one anyway is how a patch tool corrupts a file while reporting success.
func editFileTool(root, rawArgs string) string {
	var args struct {
		Path string `json:"path"`
		Old  string `json:"old"`
		New  string `json:"new"`
		All  bool   `json:"all"`
	}
	if err := json.Unmarshal([]byte(rawArgs), &args); err != nil {
		return "BAD ARGS: " + err.Error() + ` - retry with {"path": "...", "old": "...", "new": "..."}`
	}
	rel := strings.TrimSpace(args.Path)
	if rel == "" {
		return `BAD ARGS: edit_file needs a "path". It patches a file that exists; write_file is how one is created.`
	}
	if args.Old == "" {
		return `BAD ARGS: "old" is empty. It has to be the exact text to replace, copied out of the file - an empty search matches everywhere.`
	}
	if strings.TrimSpace(args.Old) == "" {
		return `BAD ARGS: "old" is only whitespace, so it cannot identify a place in the file. Include the line it sits on.`
	}
	if args.Old == args.New {
		return `BAD ARGS: "old" and "new" are identical, so this edit would change nothing. Send the text as it is now and as it should become.`
	}

	p, err := safeJoin(root, rel)
	if err != nil {
		return "ERROR: " + rel + ": " + err.Error()
	}
	st, err := os.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "ERROR: there is no file at " + rel + ". edit_file patches an existing file - call write_file to create one, or list_dir to find the right name."
		}
		return "ERROR: cannot read " + rel + ": " + err.Error()
	}
	if st.IsDir() {
		return "ERROR: " + rel + " is a folder, not a file; call list_dir to see what is inside it"
	}
	if st.Size() > maxEditBytes {
		return fmt.Sprintf("ERROR: %s is %s, past the %s limit for one exact-string edit; that is a job for a shell command rather than a tool call", rel, humanBytes(st.Size()), humanBytes(maxEditBytes))
	}

	b, err := os.ReadFile(p)
	if err != nil {
		return "ERROR: cannot read " + rel + ": " + err.Error()
	}
	data := string(b)
	// A NUL byte is what a text editor shows as "binary": replacing a string in
	// it would either miss or splice bytes into something that is not text.
	if strings.IndexByte(data, 0) >= 0 {
		return "ERROR: " + rel + " contains NUL bytes, so it is not a text file and an exact-string edit cannot work on it"
	}

	oldUsed, newUsed, count, note := matchEdit(data, args.Old, args.New)
	if count == 0 {
		return editNotFound(rel, data, args.Old)
	}
	if count > 1 && !args.All {
		return editAmbiguous(rel, data, args.Old, count)
	}

	replaced := 1
	if args.All {
		replaced = -1
	}
	out := strings.Replace(data, oldUsed, newUsed, replaced)
	if err := os.WriteFile(p, []byte(out), st.Mode().Perm()); err != nil {
		return "ERROR: cannot write " + rel + ": " + err.Error()
	}

	return editReport(rel, data, oldUsed, newUsed, count, st.Size(), int64(len(out)), note)
}

// matchEdit finds `old` in data and returns the text that actually matched, the
// replacement to use with it, how many places matched, and a note when the two
// differ from what the caller sent.
//
// The one mismatch that is not the model's fault is the line ending: a file
// written on Windows has CRLF and the text in a tool call almost always has LF,
// because that is what a model types. Retrying the file's own convention is the
// difference between a patch that works and one that reports "not found" for text
// sitting in plain sight. The replacement is converted the same way, so a file
// with CRLF endings does not come back with one LF-only line in it.
func matchEdit(data, old, new string) (oldUsed, newUsed string, count int, note string) {
	if n := strings.Count(data, old); n > 0 {
		return old, new, n, ""
	}
	lf := strings.ReplaceAll(old, "\r\n", "\n")
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	for _, cand := range []struct {
		old, new string
	}{{crlf, strings.ReplaceAll(strings.ReplaceAll(new, "\r\n", "\n"), "\n", "\r\n")},
		{lf, strings.ReplaceAll(new, "\r\n", "\n")}} {
		if cand.old == old {
			continue
		}
		if n := strings.Count(data, cand.old); n > 0 {
			return cand.old, cand.new, n, "the file's line endings differ from the text you sent, so the search used the file's own convention"
		}
	}
	return old, new, 0, ""
}

// editNotFound is the answer to a search that matched nothing. It says whether the
// text is plausibly there - with different whitespace, or with Windows line
// endings - because "not found" for a line the model just read is otherwise a
// dead end that costs two more reads.
func editNotFound(rel, data, old string) string {
	if squash(data) != "" && strings.Contains(squash(data), squash(old)) {
		return "ERROR: " + rel + " has text that matches except for the whitespace: the indentation, the spacing between words, or tabs against spaces differ from what you sent. Copy the lines back out with read_file and send them exactly, or make \"old\" a shorter fragment that does not span the whitespace. Nothing was written."
	}
	return "ERROR: \"" + clipText(firstLineOf(old), maxEditShown) + "\" does not appear in " + rel + ", so nothing was written. read_file it again and copy the text exactly - the match has to be byte for byte, including indentation."
}

// editAmbiguous refuses an edit whose search text matches more than once, and
// names the lines so the next attempt can be the right one.
func editAmbiguous(rel, data, old string, count int) string {
	lines := matchLines(data, old, maxEditLines)
	more := ""
	if count > len(lines) {
		more = fmt.Sprintf(" and %d more", count-len(lines))
	}
	return fmt.Sprintf("ERROR: \"%s\" appears %d times in %s (lines %s%s), so it does not identify one of them, and nothing was written. Include the line above or below to make it unique - or pass \"all\": true to replace every one of them on purpose.",
		clipText(firstLineOf(old), maxEditShown), count, rel, strings.Join(intsToStrings(lines), ", "), more)
}

// editReport says what changed, in terms a reader can check without reading the
// file again: where it happened, how many places, and the size before and after.
// count is the number of replacements actually made, which is not len(lines):
// the list of line numbers is capped, and a report that quietly showed the cap
// instead of the total would understate a global edit.
func editReport(rel, data, oldUsed, newUsed string, count int, before, after int64, note string) string {
	lines := matchLines(data, oldUsed, maxEditLines)
	var b strings.Builder
	switch {
	case count == 1 && len(lines) == 1:
		fmt.Fprintf(&b, "OK: edited %s - replaced 1 occurrence at line %d (%s -> %s).",
			rel, lines[0], humanBytes(before), humanBytes(after))
	case len(lines) > 0:
		more := ""
		if count > len(lines) {
			more = fmt.Sprintf(", and %d more", count-len(lines))
		}
		fmt.Fprintf(&b, "OK: edited %s - replaced %d occurrences at lines %s%s (%s -> %s).",
			rel, count, strings.Join(intsToStrings(lines), ", "), more, humanBytes(before), humanBytes(after))
	default:
		// Not reachable through editFileTool, which only reports a positive count;
		// kept so the function is total rather than indexing an empty slice.
		fmt.Fprintf(&b, "OK: edited %s (%s -> %s).", rel, humanBytes(before), humanBytes(after))
		return b.String() + "\nEvery byte outside the change is unchanged - no other line was touched."
	}
	oldLines := strings.Count(oldUsed, "\n") + 1
	newLines := strings.Count(newUsed, "\n") + 1
	if oldLines > 1 || newLines > 1 {
		fmt.Fprintf(&b, "\nLine %d held %d lines and now holds %d.", lines[0], oldLines, newLines)
	}
	fmt.Fprintf(&b, "\n  line %d was: %s\n  line %d is now: %s",
		lines[0], clipText(firstLineOf(oldUsed), maxEditShown), lines[0], clipText(firstLineOf(newUsed), maxEditShown))
	if newUsed == "" {
		b.WriteString("\n(The replacement is empty, so this text was removed.)")
	}
	if note != "" {
		b.WriteString("\nNote: " + note + ".")
	}
	b.WriteString("\nEvery byte outside that text is unchanged - no other line was touched.")
	return b.String()
}

// matchLines returns the 1-based line number of each occurrence of old in data,
// up to limit.
func matchLines(data, old string, limit int) []int {
	var lines []int
	for off := 0; len(lines) < limit; {
		i := strings.Index(data[off:], old)
		if i < 0 {
			break
		}
		lines = append(lines, 1+strings.Count(data[:off+i], "\n"))
		off += i + len(old)
	}
	return lines
}

// squash collapses every run of whitespace to one space, so two strings that
// differ only in indentation or tabs can be told apart from strings that are not
// there at all.
func squash(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// firstLineOf is the part of a multi-line string worth showing in one report line.
func firstLineOf(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSuffix(s[:i], "\r")
	}
	return s
}

// intsToStrings formats line numbers for a report.
func intsToStrings(ns []int) []string {
	out := make([]string, len(ns))
	for i, n := range ns {
		out[i] = fmt.Sprintf("%d", n)
	}
	return out
}
