package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// splitField splits "key=value" into (key, value, true).
// Returns ("", "", false) if there is no '=' or key is empty.
func splitField(s string) (string, string, bool) {
	i := strings.IndexByte(s, '=')
	if i <= 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// resolveFieldValue turns a raw -f value into the JSON-encodable value to send.
//
//   - "@path" reads the file at path and uses its contents verbatim as a
//     literal STRING (never auto-detected as JSON). "@-" reads stdin. This is
//     how you store JSON-object text (e.g. a Grafana dashboard model) in a
//     string field, and how you pass multiline values that the key=value split
//     would otherwise mangle. (ka-0000002135)
//   - "@@..." is an escape for a literal value that begins with a single '@'.
//   - anything else falls back to rawOrString — the JSON auto-detection used
//     for typed fields like decision options=[...].
func resolveFieldValue(v string) (any, error) {
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
			return nil, fmt.Errorf("reading field value from %q: %w", path, err)
		}
		// File/stdin contents are always a literal string — no JSON
		// auto-detection, so JSON-object text lands in a string field intact.
		return string(data), nil
	}
	return rawOrString(v), nil
}

// resolveTextValue expands the curl-style @file / @- / @@ prefixes on a plain
// text flag value (--title / --description / --notes) into the string to store.
//
//   - "@path" reads the file at path and uses its contents as the value. "@-"
//     reads stdin. This is the expansion agents already expect from muscle
//     memory (curl -d @file), and it mirrors resolveFieldValue's -f key=@file.
//   - "@@..." is an escape for a literal value that begins with a single '@'
//     (e.g. a description that really is "@mention …": pass "@@mention …").
//   - anything else is returned verbatim.
//
// Crucially this closes the data-loss footgun in ka-0000003715: before, a bare
// "@/path/to/body.md" was silently persisted as the literal description,
// clobbering the real body. Now a real file is read (what the caller meant),
// and a missing/typo'd path fails LOUD instead of overwriting the bead with a
// dangling pointer string.
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

// rawOrString returns a json.RawMessage if v looks like a JSON literal
// (object, array, quoted string, boolean, null, or number). Otherwise it
// returns v as a plain Go string so json.Marshal will quote it.
func rawOrString(v string) any {
	if len(v) == 0 {
		return v
	}
	switch v[0] {
	case '{', '[', '"':
		if json.Valid([]byte(v)) {
			return json.RawMessage(v)
		}
	default:
		// true, false, null, or a number
		if v == "true" || v == "false" || v == "null" {
			return json.RawMessage(v)
		}
		if len(v) > 0 && (v[0] == '-' || unicode.IsDigit(rune(v[0]))) {
			if json.Valid([]byte(v)) {
				return json.RawMessage(v)
			}
		}
	}
	return v // will be JSON-quoted as a string
}

// jsonMarshal is a thin wrapper so callers don't need to import encoding/json.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
