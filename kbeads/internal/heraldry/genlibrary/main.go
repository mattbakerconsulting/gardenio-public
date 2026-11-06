// Command genlibrary generates the heraldry library: library.go and
// theme_exclusions.go in kbeads/internal/heraldry, and the doc page
// gardenio/docs/heraldry/library.md, all from the same inputs:
//
//   - the pinned Noto Emoji release's 72 px PNG asset list (the charges the
//     artwork set can draw), from the GitHub tree API;
//   - Unicode's emoji-test.txt at the matching version (names, VS16,
//     groups);
//   - the bridge theme (gardenio/controller/internal/bridge/theme.go),
//     whose glyphs the library excludes (spec R4);
//   - drop.txt beside the package, the hand-kept list of charges the
//     renderer's 8-px uniqueness test drops (spec §6.6, step 2.1).
//
// Run it with `go generate ./internal/heraldry/` from kbeads (network
// needed), or pass -noto-list and -emoji-test to work from local copies.
// Re-pinning is a PR that changes NotoTag and UnicodeVersion below; the
// library only ever grows (R3).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/format"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"gardenio/kbeads/internal/heraldry/themeglyphs"
)

// The pins. NotoTag is a googlefonts/noto-emoji release tag; UnicodeVersion
// is the emoji-test.txt version that release draws.
const (
	NotoTag        = "v2026-09-24-unicode18_0"
	UnicodeVersion = "18.0"
	notoAssetDir   = "2D/png/72"
)

func notoTreeURL() string {
	return "https://api.github.com/repos/googlefonts/noto-emoji/git/trees/" + NotoTag + ":" + notoAssetDir
}

func emojiTestURL() string {
	return "https://www.unicode.org/Public/" + UnicodeVersion + ".0/emoji/emoji-test.txt"
}

func main() {
	root := flag.String("root", "", "repository root (default: the nearest parent holding go.work)")
	notoList := flag.String("noto-list", "", "file listing the Noto asset names, one per line (default: fetch the pinned tree)")
	emojiTest := flag.String("emoji-test", "", "local emoji-test.txt (default: fetch the pinned version)")
	flag.Parse()
	if *root == "" {
		r, err := findRoot()
		if err != nil {
			log.Fatal(err)
		}
		*root = r
	}
	if err := run(*root, *notoList, *emojiTest); err != nil {
		log.Fatal(err)
	}
}

func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.work")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.work above the working directory; pass -root")
		}
		dir = parent
	}
}

// entry is one emoji-test.txt line.
type entry struct {
	cps    []rune // as listed, FE0F included
	status string
	emoji  string
	name   string
	group  string
}

// charge is one library row.
type charge struct {
	emoji, name, hex, group string
	vs16                    bool
}

// dropped is a reason and the emoji it removed.
type dropped struct {
	reason string
	emoji  []string
}

func run(root, notoList, emojiTest string) error {
	assets, err := loadAssets(notoList)
	if err != nil {
		return fmt.Errorf("noto assets: %w", err)
	}
	testData, err := load(emojiTest, emojiTestURL())
	if err != nil {
		return fmt.Errorf("emoji-test: %w", err)
	}
	entries, err := parseEmojiTest(testData)
	if err != nil {
		return err
	}
	themeSrc, err := os.ReadFile(filepath.Join(root, themeglyphs.ThemePath))
	if err != nil {
		return err
	}
	themeGlyphs, err := themeglyphs.Extract(themeSrc)
	if err != nil {
		return err
	}
	pkgDir := filepath.Join(root, "kbeads/internal/heraldry")
	drops, err := loadDrops(filepath.Join(pkgDir, "drop.txt"))
	if err != nil {
		return err
	}

	// Every emoji Unicode lists, by key, for telling theme emoji from other
	// theme glyphs (○, —).
	isEmoji := map[string]bool{}
	for _, e := range entries {
		isEmoji[themeglyphs.Key(e.emoji)] = true
	}
	var themeEx []string
	for _, g := range themeGlyphs {
		if isEmoji[themeglyphs.Key(g)] {
			themeEx = append(themeEx, g)
		}
	}
	themeKeys := map[string]bool{}
	for _, g := range themeEx {
		themeKeys[themeglyphs.Key(g)] = true
	}

	var (
		lib        []charge
		sequences  = map[string]*dropped{}
		flags      = &dropped{reason: "flags (the Flags group, including the single-codepoint flags)"}
		components = &dropped{reason: "skin-tone and hair components"}
		noArt      = &dropped{reason: "no artwork in the pinned Noto Emoji release"}
		themeDrop  = &dropped{reason: "used by the bridge Garden or Business theme (R4)"}
		pixelDrop  = &dropped{reason: "dropped by the 8-px uniqueness test (drop.txt, §6.6)"}
		used       = map[string]bool{}
	)
	for _, e := range entries {
		if e.status != "fully-qualified" && e.status != "component" {
			continue
		}
		base := withoutVS16(e.cps)
		if len(base) != 1 {
			r := sequenceReason(base)
			if sequences[r] == nil {
				sequences[r] = &dropped{reason: r}
			}
			sequences[r].emoji = append(sequences[r].emoji, e.emoji)
			continue
		}
		hex := strconv.FormatInt(int64(base[0]), 16)
		used[hex] = true
		switch {
		case e.group == "Flags":
			flags.emoji = append(flags.emoji, e.emoji)
			continue
		case e.status == "component":
			components.emoji = append(components.emoji, e.emoji)
			continue
		}
		switch {
		case !assets[hex]:
			noArt.emoji = append(noArt.emoji, e.emoji)
		case themeKeys[themeglyphs.Key(e.emoji)]:
			themeDrop.emoji = append(themeDrop.emoji, e.emoji)
		case drops[themeglyphs.Key(e.emoji)]:
			pixelDrop.emoji = append(pixelDrop.emoji, e.emoji)
		default:
			lib = append(lib, charge{
				emoji: e.emoji,
				name:  strings.ToLower(e.name),
				hex:   hex,
				group: e.group,
				vs16:  len(e.cps) > len(base),
			})
		}
	}
	for k := range drops {
		if !anyKey(pixelDrop.emoji, k) {
			return fmt.Errorf("drop.txt names %s, which is not a library candidate", k)
		}
	}
	var unlisted []string
	for hex := range assets {
		if !used[hex] {
			unlisted = append(unlisted, hex)
		}
	}
	sort.Strings(unlisted)
	if len(lib) == 0 {
		return errors.New("empty library: check the inputs")
	}

	seqs := make([]*dropped, 0, len(sequences))
	for _, d := range sequences {
		seqs = append(seqs, d)
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i].reason < seqs[j].reason })

	if err := writeGo(filepath.Join(pkgDir, "library.go"), libraryGo(lib)); err != nil {
		return err
	}
	if err := writeGo(filepath.Join(pkgDir, "theme_exclusions.go"), exclusionsGo(themeEx)); err != nil {
		return err
	}
	doc := libraryMD(lib, themeEx, themeDrop, pixelDrop, noArt, flags, components, seqs, unlisted)
	return os.WriteFile(filepath.Join(root, "gardenio/docs/heraldry/library.md"), doc, 0o644)
}

func withoutVS16(cps []rune) []rune {
	var out []rune
	for _, r := range cps {
		if r != 0xFE0F {
			out = append(out, r)
		}
	}
	return out
}

func sequenceReason(cps []rune) string {
	has := func(lo, hi rune) bool {
		for _, r := range cps {
			if r >= lo && r <= hi {
				return true
			}
		}
		return false
	}
	switch {
	case has(0x200D, 0x200D):
		return "ZWJ sequences"
	case has(0x20E3, 0x20E3):
		return "keycap sequences"
	case has(0x1F1E6, 0x1F1FF):
		return "flag sequences (regional indicators)"
	case has(0xE0020, 0xE007F):
		return "tag sequences"
	case has(0x1F3FB, 0x1F3FF):
		return "skin-tone modifier sequences"
	}
	return "other multi-codepoint sequences"
}

func anyKey(ss []string, k string) bool {
	for _, s := range ss {
		if themeglyphs.Key(s) == k {
			return true
		}
	}
	return false
}

func parseEmojiTest(data []byte) ([]entry, error) {
	var out []entry
	group := ""
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		line := sc.Text()
		if g, ok := strings.CutPrefix(line, "# group: "); ok {
			group = strings.TrimSpace(g)
			continue
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		left, comment, ok := strings.Cut(line, "#")
		if !ok {
			return nil, fmt.Errorf("emoji-test line without comment: %q", line)
		}
		cpField, status, ok := strings.Cut(left, ";")
		if !ok {
			return nil, fmt.Errorf("emoji-test line without status: %q", line)
		}
		var cps []rune
		for _, h := range strings.Fields(cpField) {
			v, err := strconv.ParseUint(h, 16, 32)
			if err != nil {
				return nil, fmt.Errorf("emoji-test codepoint %q: %w", h, err)
			}
			cps = append(cps, rune(v))
		}
		// comment: " 😀 E1.0 grinning face"
		parts := strings.SplitN(strings.TrimSpace(comment), " ", 3)
		if len(parts) != 3 || !strings.HasPrefix(parts[1], "E") {
			return nil, fmt.Errorf("emoji-test comment %q", comment)
		}
		if parts[0] != string(cps) {
			return nil, fmt.Errorf("emoji-test line %q: emoji does not match its codepoints", line)
		}
		out = append(out, entry{cps: cps, status: strings.TrimSpace(status), emoji: parts[0], name: parts[2], group: group})
	}
	if len(out) == 0 {
		return nil, errors.New("emoji-test.txt has no entries")
	}
	return out, sc.Err()
}

// loadAssets returns the hex codepoint of every single-codepoint asset.
func loadAssets(notoList string) (map[string]bool, error) {
	var names []string
	if notoList != "" {
		data, err := os.ReadFile(notoList)
		if err != nil {
			return nil, err
		}
		names = strings.Fields(string(data))
	} else {
		data, err := fetch(notoTreeURL())
		if err != nil {
			return nil, err
		}
		var tree struct {
			Truncated bool `json:"truncated"`
			Tree      []struct {
				Path string `json:"path"`
			} `json:"tree"`
		}
		if err := json.Unmarshal(data, &tree); err != nil {
			return nil, err
		}
		if tree.Truncated {
			return nil, errors.New("GitHub truncated the asset tree")
		}
		for _, e := range tree.Tree {
			names = append(names, e.Path)
		}
	}
	out := map[string]bool{}
	for _, n := range names {
		hex, ok := strings.CutPrefix(n, "emoji_u")
		if !ok {
			continue
		}
		hex, ok = strings.CutSuffix(hex, ".png")
		if !ok || strings.Contains(hex, "_") {
			continue
		}
		out[strings.ToLower(hex)] = true
	}
	if len(out) == 0 {
		return nil, errors.New("no single-codepoint assets found")
	}
	return out, nil
}

func loadDrops(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line, _, _ = strings.Cut(line, "#")
		for _, f := range strings.Fields(line) {
			out[themeglyphs.Key(f)] = true
		}
	}
	return out, nil
}

func load(path, url string) ([]byte, error) {
	if path != "" {
		return os.ReadFile(path)
	}
	return fetch(url)
}

func fetch(url string) ([]byte, error) {
	c := &http.Client{Timeout: 60 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

const header = "// Code generated by genlibrary from the pinned Noto Emoji release, Unicode emoji-test.txt and the bridge theme; DO NOT EDIT.\n// Regenerate with `go generate ./internal/heraldry/` from kbeads.\n\npackage heraldry\n\n"

func libraryGo(lib []charge) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	fmt.Fprintf(&b, "// NotoEmojiTag is the googlefonts/noto-emoji release the library is drawn from.\nconst NotoEmojiTag = %q\n\n", NotoTag)
	fmt.Fprintf(&b, "// UnicodeEmojiVersion is the emoji-test.txt version the names and VS16 come from.\nconst UnicodeEmojiVersion = %q\n\n", UnicodeVersion)
	b.WriteString("// library is every charge, in Unicode emoji order.\nvar library = []Charge{\n")
	for _, c := range lib {
		fmt.Fprintf(&b, "\t{Emoji: %q, Name: %q, Codepoints: %q, VS16: %t},\n", c.emoji, c.name, c.hex, c.vs16)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

func exclusionsGo(ex []string) []byte {
	var b bytes.Buffer
	b.WriteString(header)
	b.WriteString("// themeExclusions are the emoji the bridge's Garden and Business themes use\n// (gardenio/controller/internal/bridge/theme.go). The library never holds\n// them (spec R4); TestThemeExclusionsMatchTheme keeps this list in step.\nvar themeExclusions = []string{\n")
	for _, g := range ex {
		fmt.Fprintf(&b, "\t%q,\n", g)
	}
	b.WriteString("}\n")
	return b.Bytes()
}

func writeGo(path string, src []byte) error {
	out, err := format.Source(src)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return os.WriteFile(path, out, 0o644)
}

func libraryMD(lib []charge, themeEx []string, themeDrop, pixelDrop, noArt, flags, components *dropped, seqs []*dropped, unlisted []string) []byte {
	var b bytes.Buffer
	b.WriteString("<!-- Generated by kbeads/internal/heraldry/genlibrary; DO NOT EDIT. Regenerate with `go generate ./internal/heraldry/` from kbeads. -->\n\n")
	b.WriteString("# The heraldry library\n\n")
	b.WriteString("Every emoji a bead's arms may hold (spec [`2026-10-25-heraldry-design.md`](../superpowers/specs/2026-10-25-heraldry-design.md) R2-R4). The validator's table is `kbeads/internal/heraldry/library.go`, generated by the same tool from the same inputs as this page.\n\n")
	b.WriteString("## Source\n\n")
	fmt.Fprintf(&b, "- Artwork: [Noto Emoji](https://github.com/googlefonts/noto-emoji) release `%s`, the 72 px PNGs in `%s` (images Apache 2.0).\n", NotoTag, notoAssetDir)
	fmt.Fprintf(&b, "- Names and presentation: Unicode `emoji-test.txt` version %s (%s).\n", UnicodeVersion, emojiTestURL())
	fmt.Fprintf(&b, "- Theme glyphs: `%s`, the `Garden` and `Business` themes.\n\n", themeglyphs.ThemePath)
	b.WriteString("## Counts\n\n")
	b.WriteString("| | Emoji |\n|---|---:|\n")
	fmt.Fprintf(&b, "| **In the library** | **%d** |\n", len(lib))
	vs := 0
	for _, c := range lib {
		if c.vs16 {
			vs++
		}
	}
	fmt.Fprintf(&b, "| of which carry VS16 (U+FE0F) | %d |\n", vs)
	for _, d := range append([]*dropped{themeDrop, pixelDrop, noArt, flags, components}, seqs...) {
		fmt.Fprintf(&b, "| Left out: %s | %d |\n", d.reason, len(d.emoji))
	}
	fmt.Fprintf(&b, "| Noto assets that are not an emoji on their own (digits, #, *, regional indicators) | %d |\n\n", len(unlisted))
	b.WriteString("Only single-codepoint base emoji are charges (R2): a charge is one grapheme, and a VS16 selector is part of its charge. Anything that takes several codepoints (ZWJ, keycap, flag, tag and skin-tone sequences) is out of v1.\n\n")
	b.WriteString("## Exclusions\n\n")
	b.WriteString("### Theme glyphs (R4)\n\n")
	b.WriteString("The bridge theme already gives these a meaning on Slack cards, so a shield may not hold them. A test in kbeads (`TestThemeExclusionsMatchTheme`) reads `theme.go` and fails when this list drifts.\n\n")
	b.WriteString(strings.Join(themeEx, " "))
	b.WriteString("\n\n")
	b.WriteString("### The 8-px uniqueness test (§6.6)\n\n")
	if len(pixelDrop.emoji) == 0 {
		b.WriteString("None yet. The renderer (step 2.1) owns the test; the charges it drops go in `kbeads/internal/heraldry/drop.txt` and leave the library on the next generate.\n\n")
	} else {
		b.WriteString(strings.Join(pixelDrop.emoji, " "))
		b.WriteString("\n\n")
	}
	if len(noArt.emoji) > 0 {
		b.WriteString("### No artwork\n\n")
		b.WriteString(strings.Join(noArt.emoji, " "))
		b.WriteString("\n\n")
	}
	b.WriteString("### Flags\n\n")
	b.WriteString(strings.Join(flags.emoji, " "))
	b.WriteString("\n\n")
	b.WriteString("## The charges, by name\n\n")
	b.WriteString("| Emoji | Name | Codepoint | Group |\n|---|---|---|---|\n")
	sorted := append([]charge(nil), lib...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].name < sorted[j].name })
	for _, c := range sorted {
		cp := "U+" + strings.ToUpper(c.hex)
		if c.vs16 {
			cp += " U+FE0F"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s |\n", c.emoji, strings.ReplaceAll(c.name, "|", "\\|"), cp, c.group)
	}
	return b.Bytes()
}
