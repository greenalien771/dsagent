package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// The UI is one 4000-line HTML file that gets patched by hand. These tests are
// the cheap static safety net for that: they catch the class of mistake a
// hand-edit actually makes - a handler the markup calls but the script never
// defines, an element id one side spells differently from the other, a bracket
// left open - none of which the Go compiler can see.

func loadUI(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatalf("ui/index.html: %v", err)
	}
	return string(b)
}

// scriptBody returns the contents of the last <script> block, which is where the
// application code lives (the first blocks load the vendored libraries).
func scriptBody(html string) string {
	i := strings.LastIndex(html, "<script>")
	if i < 0 {
		return ""
	}
	j := strings.LastIndex(html, "</script>")
	if j < i {
		return ""
	}
	return html[i+len("<script>") : j]
}

// uiBracketError reports the first structural problem in a JS source: an
// unbalanced bracket, or an unterminated string, comment or regex literal.
//
// It is a tokeniser, not a parser, and deliberately so: the point is to catch a
// hand-edit that left a brace open, which is the one class of damage a
// single-file UI takes that the Go compiler cannot see. Strings, comments and
// regex literals are skipped because a bracket inside one is text.
func uiBracketError(src string) error {
	depth := map[byte]int{'{': 0, '(': 0, '[': 0}
	pairs := map[byte]byte{'}': '{', ')': '(', ']': '['}
	// A slash starts a regex here and a division there; this is the usual
	// previous-significant-character rule, plus the keywords that may legally be
	// followed by a regex ("return /…/", "typeof /…/").
	regexStart := func(prev byte) bool {
		switch prev {
		case 0, '(', ',', '=', ':', '[', '!', '&', '|', '?', '{', '}', ';', '+', '-', '*', '%', '<', '>', '~', '^':
			return true
		}
		return false
	}
	regexKeywords := map[string]bool{
		"return": true, "typeof": true, "instanceof": true, "case": true, "in": true,
		"of": true, "delete": true, "void": true, "new": true, "do": true, "else": true,
		"yield": true, "await": true, "throw": true,
	}
	// precededByKeyword reads the identifier that ends just before i.
	precededByKeyword := func(i int) bool {
		j := i - 1
		for j >= 0 && (src[j] == ' ' || src[j] == '\t') {
			j--
		}
		end := j + 1
		for j >= 0 && (src[j] >= 'a' && src[j] <= 'z' || src[j] >= 'A' && src[j] <= 'Z') {
			j--
		}
		return regexKeywords[strings.ToLower(src[j+1:end])]
	}
	var line int
	var prev byte
	isRegex := func(i int) bool { return regexStart(prev) || precededByKeyword(i) }
	for i := 0; i < len(src); i++ {
		c := src[i]
		if c == '\n' {
			line++
			prev = c
			continue
		}
		if c == ' ' || c == '\t' || c == '\r' {
			continue
		}

		if c == '/' && i+1 < len(src) && src[i+1] == '/' {
			for i < len(src) && src[i] != '\n' {
				i++
			}
			line++
			prev = '\n'
			continue
		}
		if c == '/' && i+1 < len(src) && src[i+1] == '*' {
			i += 2
			for i+1 < len(src) && !(src[i] == '*' && src[i+1] == '/') {
				if src[i] == '\n' {
					line++
				}
				i++
			}
			if i+1 >= len(src) {
				return fmt.Errorf("unterminated block comment at line %d", line)
			}
			i++
			prev = '/'
			continue
		}

		if c == '/' && isRegex(i) {
			inClass := false
			closed := false
			for i++; i < len(src); i++ {
				if src[i] == '\\' {
					i++
					continue
				}
				if src[i] == '\n' {
					return fmt.Errorf("unterminated regex at line %d", line)
				}
				if src[i] == '[' {
					inClass = true
				} else if src[i] == ']' {
					inClass = false
				} else if src[i] == '/' && !inClass {
					closed = true
					break
				}
			}
			if !closed {
				return fmt.Errorf("unterminated regex at line %d", line)
			}
			prev = '/'
			continue
		}

		if c == '\'' || c == '"' || c == '`' {
			quote := c
			closed := false
			for i++; i < len(src); i++ {
				if src[i] == '\\' {
					i++
					continue
				}
				if src[i] == quote {
					closed = true
					break
				}
				if src[i] == '\n' {
					if quote != '`' {
						return fmt.Errorf("unterminated %q string at line %d", string(quote), line)
					}
					line++
				}
			}
			if !closed {
				return fmt.Errorf("unterminated %q string at line %d", string(quote), line)
			}
			prev = quote
			continue
		}

		if _, ok := depth[c]; ok {
			depth[c]++
		} else if open, ok := pairs[c]; ok {
			depth[open]--
			if depth[open] < 0 {
				return fmt.Errorf("extra %q at line %d", string(c), line)
			}
		}
		prev = c
	}

	for _, b := range []byte{'{', '(', '['} {
		if depth[b] != 0 {
			return fmt.Errorf("%q left open %d time(s)", string(b), depth[b])
		}
	}
	return nil
}

func TestUIScriptBracesBalance(t *testing.T) {
	src := scriptBody(loadUI(t))
	if len(src) < 1000 {
		t.Fatalf("script block looks truncated (%d bytes)", len(src))
	}
	if err := uiBracketError(src); err != nil {
		t.Fatalf("ui/index.html script does not tokenise cleanly: %v", err)
	}
}

// The checker above is only worth having if it fails on real damage, so it is
// tested against sources that are wrong on purpose.
func TestUIBracketCheckerRejectsDamage(t *testing.T) {
	cases := []struct {
		name string
		src  string
		ok   bool
	}{
		{"clean", `function f() { return 1; }`, true},
		{"bracket in a string", `function f() { return '(' ; }`, true},
		{"quotes in a regex class", `function f() { return /[&<>'"]/g; }`, true},
		{"block comment with a bracket", `function f() { /* ( */ return 1; }`, true},
		{"line comment with a bracket", "function f() { // (\n}", true},
		{"template literal brackets", "const t = `( ${a} `;", true},
		{"brace left open", `function f() { return 'x'; `, false},
		{"unterminated string", `function f() { return 'x ; }`, false},
		{"extra brace", `function f() { } }`, false},
		{"unterminated regex", `const r = /abc`, false},
	}
	for _, c := range cases {
		err := uiBracketError(c.src)
		if c.ok && err != nil {
			t.Errorf("%s: valid source rejected: %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: broken source accepted", c.name)
		}
	}
}

// Every handler named in the markup must exist in the script.
func TestUIHandlersAreDefined(t *testing.T) {
	html := loadUI(t)
	src := scriptBody(html)

	call := regexp.MustCompile(`\bon[a-z]+="([A-Za-z_$][\w$]*)\s*\(`)
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^function\s+([A-Za-z_$][\w$]*)`).FindAllStringSubmatch(src, -1) {
		defined[m[1]] = true
	}
	// Arrow-function delegates count as definitions too.
	for _, m := range regexp.MustCompile(`(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s*)?\(`).FindAllStringSubmatch(src, -1) {
		defined[m[1]] = true
	}

	for _, m := range call.FindAllStringSubmatch(html, -1) {
		if !defined[m[1]] {
			t.Errorf("markup calls %s(), which the script never defines", m[1])
		}
	}
	// The attachment handlers specifically: they are the whole point of the new
	// menu entries, and a typo here is a dead menu item.
	for _, fn := range []string{"pickFiles", "pickImages", "onFilesPicked", "onImagesPicked", "renderChips", "refreshModelVisionHint"} {
		if !defined[fn] {
			t.Errorf("%s() is missing from the script", fn)
		}
	}
}

// Every element id the script reaches for must exist in the markup, and every
// static getElementById target must be spelled the same on both sides.
func TestUIElementIDsExist(t *testing.T) {
	html := loadUI(t)
	src := scriptBody(html)

	ids := map[string]bool{}
	for _, m := range regexp.MustCompile(`\bid="([^"]+)"`).FindAllStringSubmatch(html, -1) {
		ids[m[1]] = true
	}
	for _, m := range regexp.MustCompile(`getElementById\('([^']+)'\)`).FindAllStringSubmatch(src, -1) {
		if !ids[m[1]] {
			t.Errorf("getElementById('%s') has no matching element", m[1])
		}
	}
	for _, id := range []string{"filePick", "imgPick", "visionHint", "plusPop", "chips"} {
		if !ids[id] {
			t.Errorf("id %q is missing from the markup", id)
		}
	}
}

// The Add Context menu must keep Add File and Add Image in their own group,
// separated from the prompt-context entries.
func TestAddContextMenuSeparatesAttachments(t *testing.T) {
	html := loadUI(t)

	i := strings.Index(html, `id="plusPop"`)
	if i < 0 {
		t.Fatal("plusPop menu is gone")
	}
	end := strings.Index(html[i:], "</div>")
	// The pop is a container of children, so take a generous slice and cut at the
	// closing of the two file inputs that follow it.
	j := strings.Index(html[i:], `id="imgPick"`)
	if j < 0 {
		t.Fatal("image picker input is gone")
	}
	menu := html[i : i+j+200]

	file := strings.Index(menu, "pickFiles()")
	image := strings.Index(menu, "pickImages()")
	sep := strings.Index(menu, `class="pop-sep"`)
	chip := strings.Index(menu, "addChip('workspace')")

	if file < 0 || image < 0 {
		t.Fatalf("Add File / Add Image entries missing from the menu")
	}
	if sep < 0 {
		t.Fatal("no separator in the Add Context menu")
	}
	if !(file < sep && image < sep) {
		t.Fatalf("the separator is not below Add File and Add Image (file=%d image=%d sep=%d)", file, image, sep)
	}
	if chip >= 0 && chip < sep {
		t.Fatal("the context entries are above the separator; it separates nothing")
	}
	_ = end
}
