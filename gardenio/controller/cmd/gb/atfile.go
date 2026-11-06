package main

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// resolveTextValue expands the curl-style @file / @- / @@ prefixes on a plain
// text flag value into the string to store, matching the convention `kd`
// documents and enforces on its prose flags (-d/--description, --title,
// --notes, -f key=@file).
//
//   - "@path" reads the file at path and uses its contents as the value.
//   - "@-" reads stdin.
//   - "@@..." is an escape for a literal value that begins with a single '@'
//     (e.g. a report body that really starts with "@mention": pass "@@mention").
//   - anything else is returned verbatim.
//
// This closes the silent data-loss footgun in ka-0000003861: before, a bare
// "@/path/to/report.md" passed to `gb decision report --content` was persisted
// as the literal pointer string, shipping an empty/garbage artifact with no
// warning and a zero exit. Now the file is read (what the caller meant), and a
// missing/typo'd path fails LOUD instead of storing a dangling pointer.
//
// This is a deliberate copy of kbeads/cmd/kd/fields.go resolveTextValue —
// the two live in separate Go modules (gardenio/controller vs
// gardenio/kbeads) and kd's helper is in an unexportable
// `package main`, so they cannot share one symbol. Keep them behaviorally in
// sync: same prefixes, same @@ escape, same "fail loud on unreadable path"
// contract, so an agent's muscle memory carries across `kd` and `gb`.
func resolveTextValue(v string) (string, error) {
	if strings.HasPrefix(v, "@@") {
		// Escape: "@@foo" -> literal string "@foo".
		return v[1:], nil
	}
	if strings.HasPrefix(v, "@") {
		path := v[1:]
		var (
			data []byte
			err  error
		)
		if path == "-" {
			data, err = io.ReadAll(os.Stdin)
		} else {
			data, err = os.ReadFile(path)
		}
		if err != nil {
			return "", fmt.Errorf("reading value from %q: %w (use @@ to pass a literal value beginning with '@', or pass the text inline)", path, err)
		}
		return string(data), nil
	}
	return v, nil
}
