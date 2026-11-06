package commons

import (
	"html/template"
	"strconv"
)

// The node fragment (node.go): the root's head and face, its tree, and the
// Open and Jobs rails. Like the other fragments it carries structure and data-*
// hooks, not layout: every row names its bead (data-id), its own node
// (href, and data-expand when it has more below it than is sent) and its state,
// so the workbench draws the tree and the stream's node and job events patch
// it in place.

var _ = template.Must(fragments.Funcs(template.FuncMap{"dur": jobDuration, "tick": func() template.HTML { return stateMarks["done"] }}).Parse(`
{{define "node"}}<section class="cm-node cm-node--{{.Kind}}" data-commons="node" data-id="{{.Root.ID}}" data-kind="{{.Kind}}" data-land="{{.Land}}" data-state="{{.Root.State}}">
<header class="cm-node-head">
{{- with .Parent}}<a class="cm-node-up" href="{{.Href}}" data-node="{{.ID}}">{{.Title}}</a>{{end}}
<h2 class="cm-node-title">{{.Root.Title}}</h2>
{{template "nodemeta" .Root}}
{{- with .Live}}
<p class="cm-node-live" data-glass="{{.Glass}}"><span class="cm-acard-glyph" aria-hidden="true">{{.Mark}}</span> <span class="cm-acard-state">{{.Label}}</span>{{with .Cost}} · {{.}}{{end}}{{with .Turns}} · {{.}}{{end}} <a class="btn small cm-acard-watch" href="{{$.Base}}/garden?glass={{.ID}}" aria-label="Watch {{.Name}} live">Watch live</a> <a class="btn small cm-nudge" href="{{$.Base}}/garden?glass={{.ID}}" data-agent="{{.ID}}" data-name="{{.Name}}" aria-label="Nudge {{.Name}}">Nudge</a></p>
{{- end}}
{{- with .Reason}}
<p class="cm-node-reason" data-reason="{{.Kind}}" data-id="{{$.Root.ID}}" data-agent="{{.Agent}}"{{with .Task}} data-task="{{.}}"{{end}}>{{.Text}}</p>
{{- end}}
{{- with .Steward}}{{template "stewardhead" .}}{{end}}
{{- with .Gardener}}<p class="cm-node-gardener">Gardener <a href="{{.Href}}" data-node="{{.ID}}">{{.Title}}</a> <span class="cm-state" data-state="{{.State}}">{{words .State}}</span></p>{{end}}
{{- with .Thread}}<a class="cm-node-thread" href="{{.Href}}" data-thread="{{.ID}}">{{if .Replies}}{{plural .Replies "reply" "replies"}}{{else}}Thread{{end}}</a>{{end}}
</header>
{{- if .Face}}
<div class="cm-node-face"{{with .FaceFrom}} data-from="{{.}}"{{end}}>{{if .FaceWhole}}<div class="cm-cut"><div class="cm-cut-head">{{markdown "cm-node-md" .Face .Base .Names}}</div><div class="cm-cut-whole" hidden>{{markdown "cm-node-md" .FaceWhole .Base .Names}}</div><button type="button" class="cm-link cm-cut-go" aria-expanded="false">Read it all</button></div>{{else}}{{markdown "cm-node-md" .Face .Base .Names}}{{end}}</div>
{{- end}}
<ol class="cm-tree" data-slot="tree">
{{- range .Children}}
{{template "noderow" .}}
{{- else}}
<li class="cm-empty">Nothing under it yet.</li>
{{- end}}
{{- if .More}}
<li class="cm-tn-more">{{.More}} more</li>
{{- end}}
</ol>
{{- with .Siblings}}
<nav class="cm-node-siblings" aria-label="Other phases"><ol>{{range .}}{{template "noderow" .}}{{end}}</ol></nav>
{{- end}}
{{- with .Dependents}}
<section class="cm-node-deps"><h3>Waiting on it</h3><ol>{{range .}}{{template "noderow" .}}{{end}}</ol></section>
{{- end}}
<section class="cm-node-open" data-slot="open"{{with .Steward}} data-done="{{.Done}}"{{end}}><h3>{{if .Steward}}Asks for you{{else}}Open{{end}}</h3>
{{- with .Open}}<ul>
{{- range .}}
<li class="cm-open cm-open--{{.Type}}{{if .Done}} cm-open--done{{end}}" data-id="{{.ID}}" data-state="{{.State}}">{{if .Done}}<span class="cm-tick" role="img" aria-label="done">{{tick}}</span> {{end}}{{if .URL}}<a class="cm-link" href="{{.URL}}">{{.Title}}</a>{{else if .ID}}{{bead .ID $.Base $.Names}}{{else}}{{.Title}}{{end}}</li>
{{- end}}
</ul>{{else}}<p class="cm-empty">Nothing open.</p>{{end}}</section>
{{- with .Steward}}
{{template "stewardrails" .}}
{{- end}}
{{- if or (eq .Kind "agent") .Steward}}
<section class="cm-jobs" data-slot="jobs" data-count="{{.JobCount}}"><h3>Jobs</h3><ol>
{{- range .Jobs}}
{{template "job" .}}
{{- else}}
<li class="cm-empty">No jobs seen yet.</li>
{{- end}}
</ol></section>
{{- end}}
</section>
{{end}}

{{define "noderow"}}<li class="cm-tn cm-tn--{{.Type}}{{if .Closed}} cm-tn--closed{{end}}" data-id="{{.ID}}" data-type="{{.Type}}" data-state="{{.State}}"{{with .Expand}} data-expand="{{.}}"{{end}}{{with .Flag}} data-flag="{{.}}"{{end}}{{with .Thread}} data-thread="{{.}}"{{end}}><a class="cm-tn-title" href="{{.Href}}" data-node="{{.ID}}">{{.Title}}</a>{{template "nodemeta" .}}
{{- with .Children}}<ol>{{range .}}{{template "noderow" .}}{{end}}</ol>{{end}}
{{- if .More}}<a class="cm-tn-more" href="{{.Href}}" data-node="{{.ID}}">{{.More}} more</a>{{end}}</li>{{end}}

{{define "nodemeta"}}<span class="cm-tn-meta"><span class="cm-state" data-state="{{.State}}">{{words .State}}</span>
{{- with .PR}} <a class="cm-tn-pr" data-state="{{.State}}"{{with .URL}} href="{{.}}"{{end}}>PR {{.State}}</a>{{end}}
{{- with .Ask}} <a class="cm-tn-ask" href="{{.Href}}" data-node="{{.ID}}" data-type="{{.Type}}" title="{{.Title}}">asks</a>{{end}}
{{- range .Blocked}} <a class="cm-tn-blocked" href="{{.Href}}" data-node="{{.ID}}" title="{{.Title}}">waits on {{.ID}}</a>{{end}}
{{- if .Jobs}} <span class="cm-tn-jobs" data-slot="jobs">{{plural .Jobs "job" "jobs"}}</span>{{end}}
{{- with .Wish}} {{template "wishmeta" .}}{{end}}</span>{{end}}

{{define "job"}}<li class="cm-job{{if .Running}} cm-job--running{{else if .Failed}} cm-job--failed{{end}}" data-job="{{.ID}}" data-tool="{{.Tool}}"><code class="cm-job-cmd">{{or .Command .Tool}}</code> <span class="cm-job-meta">{{with .Exit}}exit {{.}} · {{end}}{{if .Running}}running{{else}}{{dur .DurationMs}}{{end}}{{if .OutBytes}} · {{bytes .OutBytes}}{{end}}</span> <time datetime="{{stamp .Start}}">{{hm .Start}}</time>
{{- with .Head}}<pre class="cm-job-out">{{range $i, $l := .}}{{if $i}}
{{end}}{{$l}}{{end}}</pre>{{end}}</li>{{end}}
`))

// jobDuration says how long a job took: milliseconds under a second, seconds
// to one decimal under a minute, then minutes and seconds.
func jobDuration(ms int64) string {
	switch {
	case ms < 1000:
		return strconv.FormatInt(ms, 10) + " ms"
	case ms < 60_000:
		return strconv.FormatFloat(float64(ms)/1000, 'f', 1, 64) + " s"
	}
	s := ms / 1000
	return strconv.FormatInt(s/60, 10) + "m " + strconv.FormatInt(s%60, 10) + "s"
}
