package commons

import (
	"hash/fnv"
	"html/template"
	"regexp"
	"strings"
	"sync"

	"gardenio/controller/internal/gardenmd"
)

// The transcript as documents (Commons v2 step 5, gd-Z05Hye7yP4): an agent's
// turn written as Markdown (a heading, a list, a table, a fence) and its
// wrap-up's parts render through /garden's one Markdown (gardenmd); a plain
// line stays a line, and accomplishments written as "a; b" read as bullets.
//
// Each is rendered once per message and kept by message id (docMemo), so a
// node or thread opened again does not run goldmark again. An entry is used
// only while its text, base and every bead title it linked are what this
// request would render: titles are the viewer's (names.go), so one viewer's
// render never shows another's titles. Past docMemoMax entries it starts again.
const docMemoMax = 4096

type docEntry struct {
	sum   uint64
	base  string
	links []docLink // the beads it linked, with the titles it gave them
	html  template.HTML
}

type docLink struct {
	id, title string
	read      bool
}

var docMemo = struct {
	sync.Mutex
	m map[string]docEntry
}{m: map[string]docEntry{}}

// docMarkdown is text as a Markdown block of class cls, kept under key (a
// message id and the part of it); an empty key renders without the memo.
func docMarkdown(key, cls, text, base string, names beadNames) template.HTML {
	h := fnv.New64a()
	_, _ = h.Write([]byte(cls + "\x00" + text))
	sum := h.Sum64()
	docMemo.Lock()
	e, ok := docMemo.m[key]
	docMemo.Unlock()
	if ok && key != "" && e.sum == sum && e.base == base && sameTitles(e.links, names) {
		return e.html
	}
	var seen []docLink
	html := gardenmd.Block(cls, text, gardenmd.Options{Base: base, BeadLink: func(id string) template.HTML {
		t, read := names[id]
		seen = append(seen, docLink{id, t, read})
		return beadLink(id, base, names)
	}})
	if key != "" {
		docMemo.Lock()
		if len(docMemo.m) >= docMemoMax {
			docMemo.m = map[string]docEntry{}
		}
		docMemo.m[key] = docEntry{sum, base, seen, html}
		docMemo.Unlock()
	}
	return html
}

func sameTitles(links []docLink, names beadNames) bool {
	for _, l := range links {
		if t, read := names[l.id]; t != l.title || read != l.read {
			return false
		}
	}
	return true
}

// mdShape is a line that makes text a document: a heading, a list item, a
// quote, a table row or a fence; or strong words anywhere.
var mdShape = regexp.MustCompile("(?m)^ {0,3}(#{1,6} |[-*+] |\\d{1,3}[.)] |> |\\|.*\\||```)|\\*\\*\\S")

// IsDoc reports whether the row's words are an agent's turn written as a
// document, so they render as Markdown rather than as one linkified line.
func (r row) IsDoc() bool {
	return authorParty(r.M.Author) == "agent" && mdShape.MatchString(r.M.Text)
}

// bullets is a wrap-up part as Markdown: as written when it is Markdown
// already, else "a; b; c" as a list of its parts.
func bullets(s string) string {
	if mdShape.MatchString(s) || !strings.Contains(s, "; ") {
		return s
	}
	return "- " + strings.Join(strings.Split(s, "; "), "\n- ")
}
