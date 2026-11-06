package commons

import (
	"bytes"
	"cmp"
	"html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gardenio/controller/internal/gardenmd"
	"gardenio/controller/internal/kin"
	"gardenio/controller/internal/respond"
)

// The fragments are the one render path (D8): the first paint and every
// refetch after an invalidation get the same HTML. They carry structure and
// data-* hooks, not layout; the /garden pane (step 9) styles them.

var fragments = template.Must(template.Must(template.New("commons").Funcs(template.FuncMap{
	"linkify": linkify,
	// markdown is a decision's context as /garden's one Markdown (gardenmd), its bead ids linked as linkify links them,
	// folded after gardenmd.FoldAt runes behind Show more (rubric C11, gd-OSI1uK35Wa)
	"markdown": func(cls, text, base string, names beadNames) template.HTML {
		return gardenmd.Block(cls, text, gardenmd.Options{Base: base, BeadLink: func(id string) template.HTML { return beadLink(id, base, names) }})
	},
	"doc":     docMarkdown,
	"bullets": bullets,
	"bead":    beadLink,
	"subject": subjectLink,
	"frozen":  func() string { return FrozenNote },
	"label":   channelLabel,
	"words":   func(s string) string { return strings.ReplaceAll(s, "_", " ") },
	"avatar":  avatarInitial,
	"author":  authorName,
	"party":   authorParty,
	// Every time is UTC; the pane rewrites each <time> in the reader's zone
	// from its datetime (gardenhub web/commons.js localize).
	"stamp": func(t time.Time) string { return t.UTC().Format(time.RFC3339) },
	// a time in two parts, so a phone's timeline hides the date its day
	// separator already gives (garden.css)
	"date":    func(t time.Time) string { return t.UTC().Format("Jan 2") },
	"hm":      func(t time.Time) string { return t.UTC().Format("15:04") },
	"day":     func(t time.Time) string { return t.UTC().Format("Monday, Jan 2") },
	"bytes":   fileSize,
	"snapcap": snapCaption,
	// the "seescreen" link of an open decision or permission card
	"see": func(base, bead, name string) seeScreen {
		return seeScreen{Base: base, Bead: bead, Name: cmp.Or(name, bead)}
	},
	"plural": func(n int, one, many string) string {
		if n == 1 {
			return "1 " + one
		}
		return strconv.Itoa(n) + " " + many
	},
}).Parse(`
{{define "compose"}}
{{- if and .Access.Frozen .Access.Post}}<p class="cm-frozen" role="status">{{.Access.FrozenNote}}</p>{{end}}
{{- if .Access.Post}}<form class="cm-compose{{if .Attach}} cm-compose--attach{{end}}" method="post" action="{{.Action}}" data-commons="compose"{{if .Attach}} enctype="multipart/form-data"{{end}}{{if .Mentions}} data-mentions="{{.Mentions}}"{{end}}>
<p class="cm-to" data-slot="to" title="{{.Label}}" aria-hidden="true">{{.Label}}</p>
<div class="cm-about" data-slot="about" hidden></div>
<div class="cm-quick" data-slot="quick" role="group" aria-label="Quick replies" hidden></div>
{{- if .Attach}}
<ul class="cm-picked" aria-label="Files to send" hidden></ul>
<textarea class="cm-input" name="text" rows="1" maxlength="10000" aria-label="{{.Label}}" placeholder="{{.Placeholder}}" enterkeyhint="send"></textarea>
<div class="cm-plus">
<button class="btn cm-plusbtn" type="button" aria-expanded="false"><span class="cm-word">Attach or snap</span></button>
<div class="cm-plusmenu">
<label class="btn cm-attach"><input class="cm-attach-input" type="file" name="file" multiple aria-label="Attach files"><span class="cm-word">Attach</span><span class="cm-more"> a file</span></label>
<button class="btn cm-snapit" type="button" hidden>Snap the farm</button>
</div>
</div>
{{- else}}
<textarea class="cm-input" name="text" rows="1" maxlength="10000" required aria-label="{{.Label}}" placeholder="{{.Placeholder}}" enterkeyhint="send"></textarea>
{{- end}}
<button class="btn cm-send" type="submit"><span class="cm-word">Send</span></button>
</form>
{{- else if .Access.Frozen}}<p class="cm-frozen" role="status">{{frozen}}</p>
{{- else if .Access.Join}}<form class="cm-join" method="post" action="{{.Members}}/join" data-commons="join">
<p class="cm-note">You are not in {{label .Channel}}. Join it to post here.</p>
<button class="btn cm-joinbtn" type="submit">Join {{label .Channel}}</button>
</form>
{{- else if not .Access.Self}}<p class="cm-note cm-readonly">Sign in to post here.</p>
{{- else if .Access.MembersOnly}}<p class="cm-note cm-readonly">Only the members of {{label .Channel}} post here.</p>
{{- else}}<p class="cm-note cm-readonly">You can read {{label .Channel}} but not post in it.</p>
{{- end}}{{end}}

{{define "thread"}}<section class="cm-thread" data-commons="thread" data-land="{{.Channel.Land}}" data-channel="{{.Channel.ID}}" data-thread="{{.Root.M.ID}}">
<header class="cm-thread-head">
<a class="cm-back" href="{{.Base}}/garden/c/{{.Channel.Land}}/{{.Channel.Name}}">#{{.Channel.Name}}</a>
{{- with .Root.Subject}}
<p class="cm-thread-subj" title="{{.}}">{{.}}</p>
{{- end}}
<div class="cm-glass" data-slot="glass" hidden></div>
<div class="cm-wake" data-slot="wake" hidden></div>
</header>
<ol class="cm-messages">
{{template "message" .Root}}
{{- range .Rows}}
{{template "message" .}}
{{- end}}
</ol>
<p class="cm-typing" data-slot="typing" aria-live="polite" aria-atomic="true"></p>
{{template "compose" .Compose}}
</section>
{{end}}

{{define "message"}}<li class="cm-msg cm-msg--{{.M.Kind}}{{if .M.Thread}} cm-msg--reply{{end}}{{if .InHand}} cm-msg--inhand{{end}}" id="m-{{.M.ID}}"{{if .InHand}} data-audience="{{.Escalation.Audience}}"{{end}} data-id="{{.M.ID}}" data-channel="{{.M.Channel}}"{{if .M.Thread}} data-thread="{{.M.Thread}}"{{end}}{{with .M.Nonce}} data-nonce="{{.}}"{{end}}{{with .M.Voice}} data-voice="{{.}}"{{end}}{{with .M.Via}} data-via="{{.}}"{{end}}>
<p class="cm-meta"><span class="cm-author cm-author--{{party .M.Author}}" title="{{.M.Author}}">{{author .M.Author}}</span>
{{- if eq .M.Via "squawk"}} <span class="cm-via cm-via--squawk" title="A squawk: the agent's own report">squawk</span>{{end}}
{{- with .M.ImpersonatedBy}} <span class="cm-as" title="Written by {{.}} while signed in as {{$.M.Author}}">by {{.}} as {{author $.M.Author}}</span>{{end}} <time datetime="{{stamp .M.At}}"><span class="cm-date">{{date .M.At}}</span> {{hm .M.At}}</time>
{{- if .M.Head}} <span class="cm-wish cm-wish--{{.M.Status}}">wish · {{words .M.Status}}</span>{{end}}</p>
{{- if .M.Deleted}}
<p class="cm-text cm-tombstone">Message deleted</p>
{{- else if not .HasFace}}
{{- if eq .M.Kind "card"}}{{/* a card with no face is its subject's chip below, never its "Agent: ..." prose */}}
{{- else if .IsDoc}}
{{doc .M.ID "cm-text cm-doc" .M.Text .Base .Names}}
{{- else if and (not .M.Notice) (or .M.Text (not .M.Files))}}
<p class="cm-text">{{linkify .M.Text .Base .Names}}</p>
{{- end}}
{{- with .M.Files}}
<ul class="cm-files" aria-label="Attachments">
{{- range $i, $f := .}}
{{- if .Src}}
<li class="cm-file cm-file--img"><figure class="cm-img"><a href="{{$.Base}}{{.Src}}" target="_blank" rel="noopener noreferrer"><img src="{{$.Base}}{{.Src}}" alt="{{.Name}}" loading="lazy" decoding="async"{{with .BoxW}} width="{{.}}" height="{{$f.BoxH}}"{{end}}></a><figcaption class="cm-file-meta"><span class="cm-img-name">{{.Name}}</span>{{if .Size}} · {{bytes .Size}}{{end}}{{with $.M.Snap}}{{if eq .On $i}} · <span class="cm-snap">{{template "snapline" .}}</span>{{end}}{{end}}</figcaption></figure></li>
{{- else}}
<li class="cm-file"><a class="cm-link" href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{.Name}}</a> <span class="cm-file-meta">{{if .Image}}image{{else}}file{{end}}{{if .Size}} · {{bytes .Size}}{{end}}</span></li>
{{- end}}
{{- end}}
</ul>
{{- end}}
{{- with .M.Snap}}{{if lt .On 0}}
<p class="cm-snap">{{template "snapline" .}}</p>
{{- end}}{{end}}
{{- end}}
{{- if .Agent}}
{{template "agentcard" .}}
{{- else if .Decision}}
{{template "decisioncard" .}}
{{- else if .Permission}}
{{template "permissioncard" .}}
{{- else if .Escalation}}
{{template "escalationcard" .}}
{{- else if and .M.Notice (not .M.Deleted)}}
{{template "notice" .}}
{{- else if .M.SubjectID}}
<p class="cm-card">{{template "arms" .}}{{subject .M.SubjectID .Base .Names}}</p>
{{- end}}
{{- if .Done}}
<form class="cm-done" method="post" action="{{.Done}}" data-commons="act"><button class="btn small" type="submit">Mark done</button></form>
{{- end}}
{{- if .ThreadHref}}
<p class="cm-thread-link"><a href="{{.ThreadHref}}"{{if .M.Replies}} class="cm-replies"{{end}}>{{if .M.Replies}}{{plural .M.Replies "reply" "replies"}}{{else}}Reply{{end}}</a>
{{- if .M.Replies}} <time datetime="{{stamp .M.LastReply}}"><span class="cm-date">{{date .M.LastReply}}</span> {{hm .M.LastReply}}</time>{{end}}</p>
{{- end}}
</li>{{end}}

{{define "snapline"}}{{/* a farm snapshot's caption and view link (snap.go): in its picture's figcaption, else a line under the files; a place named by its title keeps the address in title= */}}{{if .Place}}<span class="cm-snap-cap" title="{{.At}}">{{snapcap .}}</span>{{else}}{{snapcap .}}{{end}}{{if .URL}} · <a class="cm-link cm-snap-view" href="{{.URL}}">Open the view</a>{{end}}{{end}}

{{define "notice"}}{{with .M.Notice}}<article class="cm-notice cm-notice--{{.Kind}}" data-notice="{{.Kind}}"{{if .Subject}} data-subject="{{.Subject}}"{{end}}>
{{- if eq .Kind "death_storm"}}
<p class="cm-notice-head"><span class="cm-notice-mark" aria-hidden="true">{{.Mark}}</span> <span class="cm-notice-what">Death storm</span></p>
<p class="cm-notice-body">{{plural .Count "more agent" "more agents"}} died{{if .Window}} within {{.Window}}{{end}}. Their reports were held back to keep the channel readable; each cause is on the agent's bead.</p>
{{- if .Dead}}
<ul class="cm-notice-dead">
{{- range .Dead}}
<li>{{if .ID}}<a href="{{$.Base}}/garden?bead={{.ID}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</li>
{{- end}}
{{- if and .Unnamed (not .Rest)}}
<li class="cm-notice-more">and {{.Unnamed}} not named</li>
{{- end}}
</ul>
{{- if .Rest}}
<details class="cm-notice-rest"><summary class="cm-notice-more">and {{.More}} more</summary>
<ul class="cm-notice-dead">
{{- range .Rest}}
<li>{{if .ID}}<a href="{{$.Base}}/garden?bead={{.ID}}">{{.Name}}</a>{{else}}{{.Name}}{{end}}</li>
{{- end}}
{{- if .Unnamed}}
<li class="cm-notice-more">and {{.Unnamed}} not named</li>
{{- end}}
</ul>
</details>
{{- end}}
{{- end}}
{{- else}}
<p class="cm-notice-head"><span class="cm-notice-mark" aria-hidden="true">{{.Mark}}</span> <span class="cm-notice-what">{{if eq .Kind "death"}}Agent died{{else}}Agent crashed{{end}}</span> · {{if .Subject}}<a class="cm-notice-agent" href="{{$.Base}}/garden?bead={{.Subject}}">{{.Agent}}</a>{{else}}<span class="cm-notice-agent">{{.Agent}}</span>{{end}}</p>
{{- if .Cause}}
<p class="cm-notice-body cm-notice-cause"><span class="cm-notice-label">Cause</span> {{linkify .Cause $.Base $.Names}}</p>
{{- end}}
{{- if or .Pod .Phase}}
<p class="cm-notice-facts">{{if .Pod}}<span>Pod {{.Pod}}</span>{{end}}{{if and .Pod .Phase}} · {{end}}{{if .Phase}}<span>phase {{.Phase}}</span>{{end}}</p>
{{- end}}
{{- end}}
</article>{{end}}{{end}}

{{define "arms"}}{{/* a card subject's arms, the picture in the card's header (arms.go); its name is the SVG's aria-label */}}{{with .Arms}}<span class="cm-arms">{{.}}</span>{{end}}{{end}}

{{define "agentcard"}}{{with .Agent}}<article class="cm-acard cm-acard--{{if .Live}}live{{else}}wrapup{{end}}" data-subject="{{.ID}}" data-state="{{.State}}" data-glass="{{.Glass}}">
{{- if .Live}}
{{template "arms" $}}<p class="cm-acard-task" title="{{.Headline}}">{{.Headline}}</p>
<p class="cm-acard-who"><span class="cm-acard-glyph" aria-hidden="true">{{.Mark}}</span> <span class="cm-acard-state">{{.Label}}</span> · {{.Land}}{{if .Role}}/{{.Role}}{{end}} · <span class="cm-acard-name">{{.Name}}</span></p>
{{- if .Stream}}
<p class="cm-acard-stream" role="status">{{.Stream}}</p>
{{- end}}
{{- with .Facts}}
<p class="cm-acard-facts">{{range $i, $f := .}}{{if $i}} · {{end}}<span>{{$f}}</span>{{end}}</p>
{{- end}}
{{- else}}
{{- /* the wrap-up face as a document (doc.go): the state and facts, a headline with the arms, its paragraphs, then what it waits on */}}
<p class="cm-acard-who"><span class="cm-acard-tag">{{if eq .State "died"}}Died{{else}}Wrap-up{{end}}</span> <span class="cm-acard-glyph" aria-hidden="true">{{.Mark}}</span> <span class="cm-acard-state">{{.Label}}</span> · {{.Land}}{{if .Role}}/{{.Role}}{{end}}{{range .Facts}} · <span>{{.}}</span>{{end}}</p>
<h3 class="cm-acard-task">{{template "arms" $}}{{.Headline}}</h3>
{{- if .Cause}}
<p class="cm-acard-cause" title="{{.Cause}}"><span class="cm-notice-label">Cause</span> {{linkify .Cause $.Base $.Names}}</p>
{{- end}}
{{- if and .TLDR (not .HeadlineTLDR)}}{{doc (print $.M.ID "/tldr") "cm-acard-tldr" .TLDR $.Base $.Names}}{{end}}
{{- with .Accomplishments}}{{doc (print $.M.ID "/done") "cm-acard-done" (bullets .) $.Base $.Names}}{{end}}
{{- with .Blockers}}
<p class="cm-acard-label cm-acard-blocked">Blocked</p>{{doc (print $.M.ID "/blocked") "cm-acard-blockers" (bullets .) $.Base $.Names}}
{{- end}}
{{- with .Handoff}}{{doc (print $.M.ID "/next") "cm-acard-next" . $.Base $.Names}}{{end}}
{{- with .Restart}}
<p class="cm-acard-restart"><span class="cm-acard-label">Restart</span> {{linkify . $.Base $.Names}}</p>
{{- end}}
<p class="cm-card cm-acard-links">{{range .PRs}}<a class="cm-link" href="{{.URL}}" target="_blank" rel="noopener noreferrer">{{if .Num}}PR #{{.Num}}{{else}}PR{{end}}</a> · {{end}}<a class="cm-link" href="{{$.Base}}/garden/c/n/{{.ID}}">Jobs</a> · <a class="cm-card-subject" href="{{$.Base}}/garden?bead={{.ID}}">Open {{.Name}}</a></p>
{{- end}}
{{- if .Live}}
<div class="cm-acard-acts">
{{- /* Watch and Nudge (Glass GL-12, GL-7): shown to a viewer who may watch (body[data-glass]); web/glassact.js fills the nudge box */}}
<a class="btn small cm-acard-watch" href="{{$.Base}}/garden?glass={{.ID}}" aria-label="Watch {{.Name}} live">Watch</a>
<details class="cm-switch cm-nudge" data-agent="{{.ID}}" data-name="{{.Name}}"><summary class="btn small">Nudge…</summary></details>
{{- if .Thread}}
<form method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act"><button class="btn small" type="submit" name="action" value="restart">Restart</button></form>
{{- end}}
<details class="cm-switch cm-kill"><summary class="btn small">Kill…</summary>
<form class="cm-killconfirm" method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act">
<p class="cm-note">Kill agent {{.Name}}? It stops now, mid-task.{{if .Thread}} Restart brings it back with its session.{{end}}</p>
<button class="btn small cm-killbtn" type="submit" name="action" value="kill">Kill agent {{.Name}}</button>
</form>
</details>
</div>
<p class="cm-card"><a class="cm-card-subject" href="{{$.Base}}/garden?bead={{.ID}}">Open {{.Name}}</a></p>
{{- end}}
</article>{{end}}{{end}}

{{define "decisioncard"}}{{with .Decision}}<article class="cm-dcard cm-dcard--{{if .Open}}open{{else}}{{.Outcome}}{{end}}" data-subject="{{.ID}}" data-urgency="{{.Urgency}}">
<p class="cm-dcard-head">{{template "arms" $}}<span class="cm-dcard-pri" title="Priority {{.Priority}}">P{{.Priority}}</span> <span class="cm-dcard-kind">{{if .Open}}Decision needed{{else}}Decision{{end}}</span>{{if .Asker}} · <span class="cm-dcard-asker">{{.Asker}}</span>{{end}}</p>
{{- template "waiting" .Waiting}}
<p class="cm-dcard-prompt" title="{{.Prompt}}">{{linkify .Prompt $.Base $.Names}}</p>
{{- if .Context}}
{{markdown "cm-dcard-context" .Context $.Base $.Names}}
{{- else if .Open}}
<p class="cm-dcard-noctx">No context was given with this question.</p>
{{- end}}
{{- .Evidence}}
{{- if .Open}}
{{- if .Options}}
<form class="cm-dcard-opts" method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act">
<input type="hidden" name="action" value="resolve">
<ol class="cm-opts">
{{- range .Options}}
<li class="cm-opt"><button class="btn cm-opt-btn{{if not .Primary}} ghost{{end}}" type="submit" name="value" value="{{.N}}"><span class="cm-opt-n" aria-hidden="true">{{.N}}</span> <span class="cm-opt-title">{{.Title}}</span></button>
{{- if or .Recommended .Label .Description .Artifact}}
<p class="cm-opt-more">{{if .Recommended}}<span class="cm-opt-rec">Recommended</span>{{end}}{{if .Label}}<span>{{.Label}}</span>{{end}}{{if .Description}}<span>{{.Description}}</span>{{end}}{{if .Artifact}}<span class="cm-opt-art">Needs a {{words .Artifact}}</span>{{end}}</p>
{{- end}}</li>
{{- end}}
</ol>
</form>
{{- end}}
<div class="cm-dcard-acts">
<a class="btn small" href="{{$.Base}}/garden/c/m/{{$.M.ID}}/lightbox/other" data-commons="lightbox">Other answer…</a>
<a class="btn small" href="{{$.Base}}/garden/c/m/{{$.M.ID}}/lightbox/promote" data-commons="lightbox">Escalate…</a>
<details class="cm-switch cm-kill"><summary class="btn small ghost">Dismiss…</summary>
<form class="cm-killconfirm" method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act"><input type="hidden" name="action" value="dismiss">
<input class="cm-reason" name="reason" maxlength="500" autocomplete="off" aria-label="Why dismiss this decision? (optional)" placeholder="Reason (optional)">
<button class="btn small cm-killbtn" type="submit" data-arm="Tap again to dismiss">Dismiss decision</button>
</form>
</details>
</div>
{{- else}}
<p class="cm-dcard-outcome" role="status">{{if eq .Outcome "dismissed"}}Dismissed{{else if eq .Outcome "escalated"}}Handed back for an escalation{{else}}Answered: <strong>{{.Chosen}}</strong>{{end}}{{if .By}} · by {{.By}}{{end}}</p>
{{- if .Rationale}}
<p class="cm-dcard-why">{{linkify .Rationale $.Base $.Names}}</p>
{{- end}}
{{- end}}
<p class="cm-card"><a class="cm-card-subject" href="{{$.Base}}/garden?bead={{.ID}}" title="{{.ID}}">Open the decision</a>{{if and .Open .AskerBead}} {{template "seescreen" (see $.Base .AskerBead .Asker)}}{{end}}</p>
</article>{{end}}{{end}}

{{define "permissioncard"}}{{with .Permission}}<article class="cm-pcard cm-pcard--{{if .Open}}open{{else if .Behavior}}{{.Behavior}}{{else}}closed{{end}}" data-subject="{{.ID}}">
<p class="cm-pcard-head">{{template "arms" $}}<span class="cm-pcard-kind">{{if .Open}}Permission needed{{else}}Permission{{end}}</span> · <code class="cm-pcard-tool">{{.Tool}}</code>{{if .Agent}} for <span class="cm-pcard-agent">{{.Agent}}</span>{{end}}</p>
{{- template "waiting" .Waiting}}
{{- if .Description}}
<p class="cm-pcard-desc">{{.Description}}</p>
{{- end}}
{{- if .Preview}}
<pre class="cm-pcard-input"><code>{{.Preview}}</code></pre>
{{- end}}
{{- if .Open}}
{{template "respond" $.PermissionActs}}
{{- else}}
<p class="cm-pcard-outcome" role="status">{{if eq .Behavior "allow"}}Allowed{{else if eq .Behavior "deny"}}Denied{{else}}Answered{{end}}{{if .By}} by {{.By}}{{end}}{{if .Failed}} · the answer did not reach the agent{{end}}</p>
{{- end}}
<p class="cm-card"><a class="cm-card-subject" href="{{$.Base}}/garden?bead={{.ID}}" title="{{.ID}}">Open the permission</a>{{if and .Open .AgentBead}} {{template "seescreen" (see $.Base .AgentBead .Agent)}}{{end}}</p>
</article>{{end}}{{end}}

{{define "waiting"}}{{/* whom an open decision, permission or escalation card waits on (rubric C9): "you" stands out */}}{{with .}}
<p class="cm-waiting{{if eq . "you"}} cm-waiting--you{{end}}">Waiting on {{.}}</p>{{end}}{{end}}

{{define "seescreen"}}{{/* an open decision or permission card's way to its agent's Glass, at the prompt (Lands workbench-glass spec 3.3); web/glass.js shows it to a viewer who may watch */}}<a class="cm-card-subject cm-see" href="{{.Base}}/garden?glass={{.Bead}}" data-glass-to="prompt" aria-label="See the screen of {{.Name}}" hidden>See the screen</a>{{end}}

{{define "escalationcard"}}{{with .Escalation}}<article class="cm-ecard cm-ecard--{{.State}}" data-subject="{{.ID}}" data-urgency="{{.Urgency}}">
<p class="cm-ecard-head">{{template "arms" $}}<span class="cm-dcard-pri" title="Priority {{.Priority}}">P{{.Priority}}</span> <span class="cm-ecard-kind">{{if .Open}}Escalation needed{{else}}Escalation{{end}}</span>{{if .Asker}} · <span class="cm-ecard-asker">{{.Asker}}</span>{{end}}</p>
{{- if or (eq .Waiting "you") (not .InHand)}}{{template "waiting" .Waiting}}{{end}}
<p class="cm-ecard-title" title="{{.Title}}">{{linkify .Title $.Base $.Names}}</p>
<p class="cm-ecard-meta">{{if .InHand}}<span class="cm-ecard-aud cm-ecard-aud--{{.Audience}}" title="Who has it">{{if .WaitingOn}}{{.WaitingOn}}{{else}}{{.Audience}}{{end}}</span> {{end}}{{if .Operation}}<code class="cm-ecard-op">{{.Operation}}</code>{{end}}{{if .Where}} <span>{{.Where}}</span>{{end}}{{if .Sensitivity}} <span class="cm-ecard-sens cm-ecard-sens--{{.Sensitivity}}">{{.Sensitivity}}</span>{{end}}</p>
{{- if .Creds}}
<p class="cm-ecard-creds">Needs {{range $i, $c := .Creds}}{{if $i}}, {{end}}<code>{{$c}}</code>{{end}}{{if .MoreCreds}} and {{.MoreCreds}} more{{end}}</p>
{{- end}}
{{- if .Context}}
<p class="cm-ecard-context" title="{{.Context}}">{{linkify .Context $.Base $.Names}}</p>
{{- end}}
{{- if or .MR .Task .Script}}
<p class="cm-ecard-links">{{if .MR}}<a href="{{.MR}}" rel="noopener noreferrer" target="_blank">Open the merge request</a>{{end}}{{if .Task}}<span>Blocks {{bead .Task $.Base $.Names}}</span>{{end}}{{if .Script}}<span>Runs an attached script: review it first</span>{{end}}</p>
{{- end}}
{{- if .Open}}
{{- if .Pingbacks}}
<p class="cm-ecard-ping"{{if .PingReason}} title="{{.PingReason}}"{{end}}>Pinged {{.Pingbacks}} {{if eq .Pingbacks 1}}time{{else}}times{{end}}{{if .PingReason}}: {{.PingReason}}{{end}}</p>
{{- end}}
<div class="cm-ecard-acts">
{{- if .CanApprove}}
<form method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act"><button class="btn cm-allow" type="submit" name="action" value="approve">Approve</button></form>
{{- end}}
<a class="btn small ghost" href="{{$.Base}}/{{.ID}}">Review</a>
{{- if .CanApprove}}
<details class="cm-switch cm-kill"><summary class="btn ghost cm-deny">Reject…</summary>
<form class="cm-killconfirm" method="post" action="{{$.Base}}/garden/c/action/{{$.M.ID}}" data-commons="act">
<p class="cm-note">Reject this escalation?{{if .Asker}} {{.Asker}} is told no.{{end}}</p>
<button class="btn small cm-killbtn" type="submit" name="action" value="reject">Reject escalation</button>
</form>
</details>
{{- end}}
</div>
{{- else}}
<p class="cm-ecard-outcome" role="status">{{if eq .State "rejected"}}Rejected{{if .By}} by {{.By}}{{end}}{{else if eq .State "approved"}}Approved{{if .By}} by {{.By}}{{end}} · running{{else}}{{if eq .Result "success"}}Done{{else if eq .Result "partial"}}Partly done{{else if eq .Result "failure"}}Failed{{else}}Closed{{end}}{{if .By}} · approved by {{.By}}{{end}}{{if .Executor}} · run by {{.Executor}}{{end}}{{end}}</p>
{{- if .Reason}}
<p class="cm-ecard-why">{{linkify .Reason $.Base $.Names}}</p>
{{- end}}
{{- if .Summary}}
<p class="cm-ecard-why">{{.Summary}}</p>
{{- end}}
{{- if and .Sensitive (eq .State "done")}}
<p class="cm-ecard-note">The full result is {{.Sensitivity}}: open the escalation to read it.</p>
{{- end}}
{{- end}}
<p class="cm-card"><a class="cm-card-subject" href="{{$.Base}}/garden?bead={{.ID}}" title="{{.ID}}">Open the escalation</a></p>
</article>{{end}}{{end}}

{{define "people"}}<section class="cm-lightbox cm-people" data-commons="lightbox" data-kind="people" data-land="{{.Land}}" role="dialog" aria-modal="true" aria-labelledby="cm-lb-title">
<h2 class="cm-lb-title" id="cm-lb-title">People in {{.Land}}</h2>
{{- with .Note}}
<p class="cm-note cm-lb-done" role="status">{{.}}</p>
{{- end}}
<form class="cm-lb-form cm-invite" method="post" action="{{.Members}}/invite" data-commons="invite">
{{- with .Error}}
<p class="cm-lb-error" id="cm-invite-error" role="alert">{{.}}</p>
{{- end}}
<label class="cm-lb-field"><span>GitHub login</span><input class="cm-input" name="login" value="{{.Login}}" required maxlength="40" autocomplete="off" autocapitalize="none" spellcheck="false"{{if .Error}} aria-invalid="true" aria-describedby="cm-invite-error"{{end}}></label>
<fieldset class="cm-lb-roles"><legend>Invite as</legend>
<label class="cm-lb-role"><input type="radio" name="role" value="member"{{if ne .Role "guest"}} checked{{end}}><span><b>Member</b><span class="cm-lb-hint">Talks, files work and starts agents</span></span></label>
<label class="cm-lb-role"><input type="radio" name="role" value="guest"{{if eq .Role "guest"}} checked{{end}}><span><b>Guest</b><span class="cm-lb-hint">Talks and reacts, nothing more</span></span></label>
</fieldset>
<p class="cm-lb-note">They join when they next sign in with that GitHub account. The invite lasts 14 days.</p>
<div class="cm-lb-acts">
<button class="btn ghost" type="button" data-commons="close">Close</button>
<button class="btn" type="submit">Invite</button>
</div>
</form>
<h3 class="cm-lb-sub">Members</h3>
<ul class="cm-invites cm-members">
{{- range .People}}
<li class="cm-invite-row cm-member-row"><span class="cm-invite-who">{{if .Name}}{{.Name}}{{else}}{{author .Principal}}{{end}}</span><span class="cm-invite-what">{{if .Role}}{{.Role}}{{else}}member{{end}}</span>
{{- if not .Me}}
<button class="btn small ghost cm-become" type="button" data-become="{{.Principal}}" data-become-name="{{if .Name}}{{.Name}}{{else}}{{author .Principal}}{{end}}" aria-label="Become {{if .Name}}{{.Name}}{{else}}{{author .Principal}}{{end}}" hidden>Become</button>
{{- end}}</li>
{{- else}}
<li class="cm-empty">{{if .NoPeople}}The members could not be read just now.{{else}}Nobody has joined yet.{{end}}</li>
{{- end}}
</ul>
<h3 class="cm-lb-sub">Invited</h3>
<ul class="cm-invites">
{{- range .Invites}}
<li class="cm-invite-row" data-invite="{{.ID}}"><span class="cm-invite-who">@{{.Login}}</span><span class="cm-invite-what">{{.Role}} · {{if .Revoking}}revoking{{else if eq .State "joined"}}joined{{else}}waiting, until {{day .ExpiresAt}}{{end}}</span>
{{- if not .Revoking}}
<form class="cm-invite-revoke" method="post" action="{{$.Members}}/revoke" data-commons="invite"><input type="hidden" name="id" value="{{.ID}}"><button class="btn small ghost" type="submit" aria-label="Revoke @{{.Login}}">Revoke</button></form>
{{- end}}</li>
{{- else}}
<li class="cm-empty">Nobody invited yet.</li>
{{- end}}
</ul>
</section>
{{end}}

{{define "lightbox"}}<section class="cm-lightbox" data-commons="lightbox" data-kind="{{.Kind}}" data-card="{{.Card}}" role="dialog" aria-modal="true" aria-labelledby="cm-lb-title">
<form class="cm-lb-form" method="post" action="{{.Base}}/garden/c/submit/{{.Card}}" data-commons="submit">
<input type="hidden" name="kind" value="{{.Kind}}">
<h2 class="cm-lb-title" id="cm-lb-title">{{if eq .Kind "other"}}Answer in your own words{{else}}Escalate this decision{{end}}</h2>
<p class="cm-lb-prompt">{{linkify .D.Prompt .Base .Names}}</p>
{{- if .D.Context}}
{{markdown "cm-lb-context" .D.Context .Base .Names}}
{{- end}}
{{- .D.Evidence}}
{{- if not .D.Open}}
<p class="cm-note" role="status">This decision is closed already.</p>
{{- else if eq .Kind "other"}}
<label class="cm-lb-field"><span>Your answer</span><textarea class="cm-input" name="text" rows="4" maxlength="10000" required></textarea></label>
{{- else}}
<p class="cm-lb-note">{{if .D.Asker}}{{.D.Asker}}{{else}}The agent that asked{{end}} gets the decision back and files a structured escalation (<code>gb escalation create</code>) for an operator to approve. The decision closes.</p>
{{- end}}
<div class="cm-lb-acts">
<button class="btn ghost" type="button" data-commons="close">Cancel</button>
{{- if .D.Open}}
<button class="btn" type="submit">{{if eq .Kind "other"}}Send answer{{else}}Escalate{{end}}</button>
{{- end}}
</div>
</form>
</section>
{{end}}
`)).Parse(respond.Template))

// compose is the foot of a thread: the composer when the viewer
// may post, Join for a user channel the viewer is not in, else a line saying
// why there is no composer.
type compose struct {
	Access  access
	Channel Channel
	// Action is the POST target of the composer: the thread's .../reply.
	Action string
	// Members is the channel's URL, which join and the @ list hang off.
	Members string
	// Label is the composer's accessible name, and the line on its top edge
	// that says where the words go, so a thread's composer beside its
	// channel's reads apart from it (gd-EvK4BmuKos). The placeholder stays
	// the house voice (gardenhub.EmptyPrompt).
	Label       string
	Placeholder string
	// Attach: the composer offers attaching files (Handler.Files is set). The
	// text is then not required, since files alone are a message.
	Attach bool
	// Mentions is the composer's @ list (mention.go), when the viewer may
	// start agents and the Commons reaches the bridge, or the Commons reads
	// the land's people; "" offers none.
	Mentions string
}

// composeFor is the reply composer of ch's thread. It carries the empty slots
// a gardener's thread fills from the farm: the bead context chip and the quick
// replies (homestead-agents-as-people.md section 12, web/chat.js).
func (h *Handler) composeFor(r *http.Request, ch Channel, thread string) compose {
	c := compose{
		Channel: ch, Placeholder: h.EmptyPrompt, Attach: h.Files != nil,
		Members: h.BasePath + "/garden/c/" + url.PathEscape(ch.Land) + "/" + url.PathEscape(ch.Name),
		Access:  h.accessTo(r, ch, ActionReply),
		Action:  h.BasePath + "/garden/c/t/" + url.PathEscape(thread) + "/reply", Label: "Reply in thread",
	}
	if c.Access.Post && (h.People != nil || h.Compose != nil && h.Gate.MayAct != nil && h.Gate.MayAct(r, ch.Land, actionSpawn)) {
		c.Mentions = c.Members + "/mentions"
	}
	return c
}

// channelLabel is how a channel is named on screen: #name, or a DM's title.
func channelLabel(ch Channel) string {
	if ch.Kind == "dm" {
		return ch.Title
	}
	return "#" + ch.Name
}

// seeScreen is a card's See the screen: the agent's Glass, by its bead.
type seeScreen struct{ Base, Bead, Name string }

// row is one message as a fragment renders it.
type row struct {
	M    Message
	Base string
	// Agent, Decision, Permission and Escalation are a card's face, read from
	// its subject (cards.go, decision_cards.go, escalation_cards.go); with none
	// the card is a link to its subject.
	Agent      *AgentCard
	Decision   *DecisionCard
	Permission *PermissionCard
	Escalation *EscalationCard
	// Arms is the subject's arms as inline SVG (arms.go), shown in the card's
	// header; "" when it bears none.
	Arms template.HTML
	// ThreadHref opens the message's thread; set on top-level entries that are
	// not tombstones.
	ThreadHref string
	// Done is the POST target of an open wish head's Done button, set when
	// the viewer may mark it done (Handler.withDone); "" offers none.
	Done string
	// Names is the titles of the beads the row links to (names.go
	// Handler.withNames); nil links every bead under its id.
	Names beadNames
}

// InHand reports whether the row is an escalation card that is not the
// owner's (gd-bmOiteQi36): its row is marked in hand, with its audience.
func (r row) InHand() bool { return r.Escalation != nil && r.Escalation.InHand() }

// PermissionActs is an open permission card's Allow and Deny, as the respond
// component the Glass answers a prompt with (internal/respond, Lands GL-8):
// one way to answer an agent, wherever it asks.
func (r row) PermissionActs() respond.Form {
	p := r.Permission
	if p == nil {
		return respond.Form{}
	}
	deny := "Deny " + p.Tool
	if p.Agent != "" {
		deny += " for " + p.Agent
	}
	return respond.Form{
		Action: r.Base + "/garden/c/action/" + r.M.ID, Hook: "commons", Class: "cm-pcard-acts", Name: "action",
		Choices: []respond.Choice{
			{Label: "Allow", Value: "allow", Class: "cm-allow"},
			{Label: "Deny", Value: "deny", Class: "cm-deny", Confirm: deny + "? The agent is told no and goes on without it.", ConfirmLabel: "Deny " + p.Tool},
		},
	}
}

// HasFace reports whether the row is a card with a face, which stands in for
// the message's text.
func (r row) HasFace() bool {
	return r.Agent != nil || r.Decision != nil || r.Permission != nil || r.Escalation != nil
}

// subjectMax is the rune cap on a thread's subject; the CSS cuts it to the
// head's width.
const subjectMax = 120

// Subject names a thread in its head (Commons on a phone M2, design note
// 3.2): who began it and the first line of what they said, or of the face
// that stands in for it.
func (r row) Subject() string {
	var line string
	switch n := r.M.Notice; {
	case r.M.Deleted:
		line = "Message deleted"
	case r.Agent != nil:
		line = cmp.Or(r.Agent.Task, strings.ReplaceAll(r.Agent.State, "_", " "))
	case r.Decision != nil:
		line = r.Decision.Prompt
	case r.Permission != nil:
		line = cmp.Or(r.Permission.Description, r.Permission.Tool)
	case n != nil && n.Kind == "death_storm":
		line = "Death storm"
	case n != nil && n.Kind == "death":
		line = "Agent died · " + n.Agent
	case n != nil:
		line = "Agent crashed · " + n.Agent
	default:
		line = lastLine(r.M)
	}
	line = strings.Join(strings.Fields(line), " ")
	if line == "" {
		return authorName(r.M.Author)
	}
	return clipRunes(authorName(r.M.Author)+": "+line, subjectMax)
}

func newRow(m Message, base, channelRef string) row {
	r := row{M: m, Base: base}
	// A tombstone keeps its thread open only when the thread has replies.
	if m.Thread == "" && (!m.Deleted || m.Replies > 0) {
		r.ThreadHref = base + "/garden/c/" + url.PathEscape(m.Land) + "/" + url.PathEscape(channelRef) + "/t/" + url.PathEscape(m.ID)
	}
	return r
}

func render(name string, data any) ([]byte, error) {
	var b bytes.Buffer
	if err := fragments.ExecuteTemplate(&b, name, data); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// authorName is the short display name of a principal: the email's local part
// for a person, the name for an agent.
func authorName(p string) string {
	switch {
	case strings.HasPrefix(p, "oidc:"):
		local, _, _ := strings.Cut(strings.TrimPrefix(p, "oidc:"), "@")
		return local
	case strings.HasPrefix(p, "agent:"), strings.HasPrefix(p, "slack:"):
		_, name, _ := strings.Cut(p, ":")
		return name
	case p == "":
		return "someone"
	}
	return p
}

// authorParty is "human" for a person (kin.VoicePerson) and "agent" otherwise;
// a bare actor (an agent's static-token writes carry its name) counts as an agent.
func authorParty(p string) string {
	if kin.VoiceOf(p, "") == kin.VoicePerson {
		return "human"
	}
	return "agent"
}

// links finds http(s) URLs and bead ids (two letters, a dash, ten
// alphanumerics), the same shapes the in-garden bead view links (web/workbench.js).
var links = regexp.MustCompile("(https?://[^\\s<>\"'`]+)|\\b([a-z]{2}-[A-Za-z0-9]{10})\\b")

// linkify escapes text and links the URLs and bead ids in it. A URL opens in a
// new tab, labelled by its host and last two path segments; a bead id opens in
// the garden's bead view, labelled by its title from names (beadLink).
func linkify(text, base string, names beadNames) template.HTML {
	var b strings.Builder
	last := 0
	for _, m := range links.FindAllStringSubmatchIndex(text, -1) {
		start, end := m[0], m[1]
		var link string
		switch {
		case m[2] >= 0:
			raw := strings.TrimRight(text[m[2]:m[3]], ".,;:!?)]}")
			end = m[2] + len(raw)
			u, err := url.Parse(raw)
			if err != nil || u.Host == "" {
				continue
			}
			link = `<a class="cm-link" href="` + template.HTMLEscapeString(raw) + `" title="` + template.HTMLEscapeString(raw) +
				`" target="_blank" rel="noopener noreferrer">` + template.HTMLEscapeString(urlLabel(u)) + `</a>`
		case m[4] >= 0:
			id := text[m[4]:m[5]]
			if !beadLike(id) {
				continue
			}
			link = string(beadLink(id, base, names))
		}
		b.WriteString(template.HTMLEscapeString(text[last:start]))
		b.WriteString(link)
		last = end
	}
	b.WriteString(template.HTMLEscapeString(text[last:]))
	return template.HTML(b.String())
}

// fileSize is a byte count as a person reads it: 512 B, 3.4 KB, 1.2 MB.
func fileSize(n int) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatFloat(float64(n)/(1<<20), 'f', 1, 64) + " MB"
	case n >= 1<<10:
		return strconv.FormatFloat(float64(n)/(1<<10), 'f', 1, 64) + " KB"
	default:
		return strconv.Itoa(n) + " B"
	}
}

// beadLike reports whether a two-letter-dash match is an id rather than a
// word: the suffix carries a capital or a digit (gd-AbCdEfGhIj, ka-0000001234).
func beadLike(id string) bool {
	for _, r := range id[3:] {
		if unicode.IsUpper(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

func urlLabel(u *url.URL) string {
	segs := strings.FieldsFunc(u.Path, func(r rune) bool { return r == '/' })
	switch {
	case len(segs) == 0:
		return u.Host
	case len(segs) > 2:
		return u.Host + "/…/" + strings.Join(segs[len(segs)-2:], "/")
	}
	return u.Host + "/" + strings.Join(segs, "/")
}

// lastLineMax is the rune cap on a message's last line (lastLine).
const lastLineMax = 60

// lastLine is the first non-blank line of m's text, or its first file's name
// for a message of files only, cut to lastLineMax runes.
func lastLine(m Message) string {
	for _, l := range strings.Split(m.Text, "\n") {
		if l = strings.Join(strings.Fields(l), " "); l != "" {
			return clipRunes(l, lastLineMax)
		}
	}
	if len(m.Files) > 0 {
		return clipRunes(m.Files[0].Name, lastLineMax)
	}
	return ""
}
