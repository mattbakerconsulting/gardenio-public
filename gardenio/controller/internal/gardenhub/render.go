package gardenhub

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/url"
	"strings"
	"unicode/utf8"

	"gardenio/controller/internal/decisionview"
	"gardenio/controller/internal/gardenmd"
	"gardenio/controller/internal/heraldry"
)

func lower(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// itemTmpl renders one plant. It is rendered once per change (in refresh) and
// the resulting string is shared by every viewer, so it must not contain
// anything viewer-specific: the per-user "mine" class is applied client-side
// from data-op. The data-* attributes feed the farm scene (web/garden.js).
// The title reads, never the id (titles, not ids: gd-9yOATV2qCs): the id is
// the link's tooltip, or its text when the bead has no title.
var itemTmpl = template.Must(template.New("item").Parse(
	`<li class="item{{if .Blocked}} blocked{{end}}{{if .InHand}} inhand{{end}}" id="i-{{.ID}}" data-op="{{.Operator}}" data-u="{{.Updated}}" data-pj="{{.Project}}" data-st="{{.State}}"{{if eq .Section "agent"}} data-role="{{.Role}}"{{end}} data-ty="{{.Type}}" data-pr="{{.Priority}}"{{if .Report}} data-rp="1"{{end}}{{if .Arms}} data-arms="{{.Arms}}" data-an="{{.ArmsN}}" data-aname="{{.ArmsName}}"{{end}}>` +
		`<a class="t" href="{{.Href}}" title="{{.ID}}">` +
		`{{if .Arms}}<span class="ar-pic ar-line"><img class="ar-day" src="{{.ArmsDay}}" width="{{.ArmsW}}" height="20" alt="{{.ArmsName}}" decoding="async">` +
		`<img class="ar-night" src="{{.ArmsNight}}" width="{{.ArmsW}}" height="20" alt="{{.ArmsName}}" decoding="async"></span>{{end}}` +
		`{{if .Title}}{{.Title}}{{else}}{{.ID}}{{end}}</a>` +
		`<span class="m">` +
		`{{if .Project}}<span class="chip">{{.Project}}</span>{{end}}` +
		`{{if .State}}<span class="st st-{{.State}}">{{.State}}</span>{{end}}` +
		`{{if .InHand}}<span class="chip" title="The merge desk or the operators have it">in hand</span>{{end}}` +
		`{{if .Blocked}}<span class="flag" title="waiting on a decision">&#9208;</span>{{end}}` +
		`{{if .Report}}<span class="flag" title="report attached">&#128196;</span>{{end}}` +
		`<time data-t="{{.Updated}}"></time></span>` +
		`{{if eq .Type "decision"}}<div class="act" data-kind="decision">` +
		`{{if .Operator}}<p class="who" data-who>asked of {{if .AskedOf}}{{.AskedOf}}{{else}}its operator{{end}}</p>{{end}}` +
		`{{if .Ask}}<p class="ask">{{.Ask}}</p>{{end}}` +
		`{{if .Context}}{{.ContextHTML}}{{else}}<p class="ctx-none">No context was given with this question.</p>{{end}}` +
		`{{.EvidenceHTML}}` +
		`<div class="acts">{{range .OptionList}}<button type="button" class="btn small" data-act="resolve" data-id="{{$.ID}}" data-opt="{{.ID}}"{{if .Label}} title="{{.Label}}"{{end}}>{{.Text}}</button>{{end}}` +
		`<button type="button" class="btn small ghost" data-act="other">Other&hellip;</button></div>` +
		`<div class="free" hidden><textarea rows="3" maxlength="10000" aria-label="Your answer" placeholder="Write your answer"></textarea>` +
		`<button type="button" class="btn small" data-act="resolve" data-id="{{.ID}}">Send answer</button></div>` +
		`<p class="note" role="status"></p></div>{{end}}` +
		`{{if eq .Type "escalation"}}<div class="act" data-kind="escalation">` +
		`{{if .Held}}<p class="who held">{{.HeldNote}}</p>{{else if not .InHand}}<p class="who">for the owner to approve</p>{{end}}` +
		`{{if .OpType}}<p class="ask"><b>{{.OpType}}</b>{{if .Ask}} · {{.Ask}}{{end}}</p>{{else if .Ask}}<p class="ask">{{.Ask}}</p>{{end}}` +
		`<div class="acts">{{if .Held}}<button type="button" class="btn small ghost" data-act="approve" data-id="{{.ID}}" data-arm="Tap again to approve ahead of {{.Held}}">Approve anyway</button>` +
		`{{else}}<button type="button" class="btn small" data-act="approve" data-id="{{.ID}}" data-arm="Tap again to approve">Approve</button>{{end}}` +
		`<button type="button" class="btn small ghost" data-act="reject" data-id="{{.ID}}" data-arm="Tap again to reject">Reject</button>` +
		`<button type="button" class="btn small ghost" data-act="comment" aria-expanded="false">Comment&hellip;</button></div>` +
		`<div class="free" hidden><textarea rows="2" maxlength="4000" aria-label="Comment (optional)" placeholder="Comment (optional): sent with your approval or rejection"></textarea></div>` +
		`<p class="note" role="status"></p></div>{{end}}` +
		`</li>`))

type itemView struct {
	Item
	Href string
	// ContextHTML is a decision's context as Markdown (gardenmd), under the ask and above the answers, folded after
	// gardenmd.FoldAt runes behind Show more (rubric C11): never a closed fold nor a hard clamp (gd-OSI1uK35Wa).
	ContextHTML template.HTML
	// EvidenceHTML is Item.Evidence, the decision's screenshot strip, under the context (gd-Pi7C83262N).
	EvidenceHTML template.HTML
	// ArmsDay and ArmsNight are the line arms' pictures (GET /garden/arms.svg, bead_arms.go) for the two skies, ArmsW
	// their width at the row's 20 px (heraldry.Width); the page's CSS shows the one for the sky (garden.css .ar-pic).
	ArmsDay, ArmsNight string
	ArmsW              int
}

// armsRowH is the height, in CSS px, of the line arms before a row's title (garden.css .ar-line).
const armsRowH = 20

// armsSrc is the address of arms' picture for one sky: web/arms.js svgSrc, server-side.
func armsSrc(basePath, canonical, theme string) string {
	return basePath + "/garden/arms.svg?a=" + url.QueryEscape(canonical) + "&theme=" + theme
}

// askMax clips the prompt or context shown inline; the full text is one tap away in the bead view.
const askMax = 280

// contextMax clips a decision's context carried on its Mailbox row: the Slack
// card's and the Commons card's limit, so every surface says the same.
const contextMax = 2900

type optionView struct{ ID, Text, Label string }

// OptionList turns the decision's raw options JSON into answer buttons. An option with no id cannot be
// chosen (the resolve call takes an id), so it is left to the free-text answer.
func (it Item) OptionList() []optionView {
	var raw []map[string]any
	if json.Unmarshal([]byte(it.Options), &raw) != nil {
		return nil
	}
	str := func(m map[string]any, k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
	var out []optionView
	for _, m := range raw {
		id := str(m, "id")
		if id == "" {
			continue
		}
		v := optionView{ID: id, Text: str(m, "short"), Label: str(m, "label")}
		if v.Text == "" {
			v.Text = v.Label
		}
		if v.Text == "" {
			v.Text = id
		}
		out = append(out, v)
	}
	return out
}

// HeldNote is a held escalation's line in words: "The merge desk has this; nothing waits on you." web/answer.js heldNote
// says the same in the bead view and the Queues panel.
func (it Item) HeldNote() string {
	if it.Held == "" {
		return ""
	}
	return strings.ToUpper(it.Held[:1]) + it.Held[1:] + " has this; nothing waits on you."
}

// clip collapses whitespace and cuts s to at most n runes.
func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}

func renderItem(it Item, basePath string) string {
	var b bytes.Buffer
	v := itemView{Item: it, Href: beadHref(basePath, it.ID)}
	if it.Type == "decision" {
		v.ContextHTML = gardenmd.Block("ctx", it.Context, gardenmd.Options{Base: basePath})
		v.EvidenceHTML = template.HTML(it.Evidence) //nolint:gosec // decisionview.EvidenceStrip's html/template output (dressEvidence)
	}
	if it.Arms != "" {
		v.ArmsDay, v.ArmsNight, v.ArmsW = armsSrc(basePath, it.Arms, "day"), armsSrc(basePath, it.Arms, "night"), heraldry.Width(it.ArmsN, armsRowH)
	}
	if err := itemTmpl.Execute(&b, v); err != nil {
		return ""
	}
	return b.String()
}

// beadHref is a bead's address inside the garden: the page itself with the bead view open (web/workbench.js reads ?bead=), so a
// title opens the in-garden panel, a reload or a shared link reopens it, and nothing leaves for the beads viewer.
func beadHref(basePath, id string) string { return basePath + "/garden?bead=" + url.QueryEscape(id) }

// evidenceStrip draws a decision's evidence images as the shared strip, a
// source bead opening in the garden's bead view.
func evidenceStrip(basePath, slot, id string, imgs []decisionview.Image) template.HTML {
	return decisionview.EvidenceStrip(id, imgs, decisionview.StripOptions{
		Base: basePath, Slot: slot,
		Href: func(src string) string { return beadHref(basePath, src) },
	})
}
