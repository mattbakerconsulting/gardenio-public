package mailer

import (
	"bytes"
	"errors"
	"fmt"
	htmltemplate "html/template"
	"net/url"
	"strconv"
	"strings"
	"text/template"
)

// Kind names one Lands mail template (§2.7 of the Track B spec). The values
// are stable: SES tags each send with one, so changing a value breaks the
// trail from a bounce back to its template.
type Kind string

const (
	// The Gate (§2.5): one mail per application state change.
	KindApplicationReceived Kind = "application_received"
	KindApplicationApproved Kind = "application_approved"
	KindApplicationDenied   Kind = "application_denied"
	KindApplicationQuestion Kind = "application_question"
	// The applicant withdrew from /lands/applications (step 7.7): the mail
	// confirms it, so a withdrawal nobody meant to make is noticed.
	KindApplicationWithdrawn Kind = "application_withdrawn"
	// The provisioner (§3.3 step 7).
	KindLandReady Kind = "land_ready"
	// The purse (§3.4): mail to the owner at 80 and 100 percent spent.
	KindPurseLow   Kind = "purse_low"
	KindPurseEmpty Kind = "purse_empty"
	// The kill switch (§5.5) and deprovision (§3.3).
	KindLandFrozen Kind = "land_frozen"
	KindLandClosed Kind = "land_closed"
	// KindLandJoined goes to someone an owner invited, once they have joined
	// the land (§3.7, step 8.9).
	KindLandJoined Kind = "land_joined"
	// KindInvite goes to the email address a sign-in invite names, when the
	// invite is filed (gd-ycE484kLyk): GitHub sign-in is invite-only, and
	// the invite is matched to this address.
	KindInvite Kind = "invite"
)

// Data is what a template says. Each Kind reads only some fields; Render
// refuses a Data that lacks one its Kind requires.
type Data struct {
	// Name is the greeting name, the recipient's GitHub login. Optional.
	Name string
	// ApplicationID is the plot_application bead id.
	ApplicationID string
	// Land is the land's name: the requested name on an application, the
	// project name once provisioned.
	Land string
	// Repo is the land's repo, "owner/name" on github.com.
	Repo string
	// Note is the operator's free text: the reason on a denial, the question
	// on an ask, the reason on a freeze or close. Optional except on a
	// question.
	Note string
	// GrantedUSD is the credit granted: on approval, the grant; on the purse
	// mails, the sum of grants.
	GrantedUSD float64
	// RemainingUSD is what the purse has left.
	RemainingUSD float64
	// InvitedBy names who sent a sign-in invite.
	InvitedBy string
}

// field names a Data field a Kind requires.
type field int

const (
	fieldApplicationID field = iota
	fieldLand
	fieldRepo
	fieldNote
	fieldGranted
	fieldInvitedBy
)

// templateDef is one Kind's words. Body is a text/template whose paragraphs
// are separated by blank lines; the HTML body is built from the same
// paragraphs, so the two can never say different things.
type templateDef struct {
	subject   string
	body      string
	linkLabel string
	// link returns the path (with query) the call to action points at,
	// relative to Templates' base URL.
	link     func(Data) string
	requires []field
	// tenant marks a mail to a land's owner rather than to an applicant; it
	// picks the footer that says why the recipient is getting mail.
	tenant bool
	// footer, when set, is why the recipient is getting mail, in place of
	// the applicant's or the owner's.
	footer string
}

func applicationsLink(Data) string { return "/lands/applications" }

func landLink(d Data) string { return "/garden?land=" + url.QueryEscape(d.Land) }

var defs = map[Kind]templateDef{
	KindApplicationReceived: {
		subject: `We have your application for {{.Land}}`,
		body: `Hello{{with .Name}} {{.}}{{end}},

Thank you for applying for a plot called {{.Land}}, for the repo {{.Repo}}. The operator reads every application by hand, so an answer can take a few days.

We will email you when they decide, or if they have a question. Until then you can withdraw the application from the page below.`,
		linkLabel: "See your applications",
		link:      applicationsLink,
		requires:  []field{fieldApplicationID, fieldLand, fieldRepo},
	},
	KindApplicationApproved: {
		subject: `Your application for {{.Land}} is approved`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The operator approved your application for {{.Land}} ({{.Repo}}) with {{usd .GrantedUSD}} of credit.

We are setting up your land now. We will email you again when it is ready.`,
		linkLabel: "See your applications",
		link:      applicationsLink,
		requires:  []field{fieldApplicationID, fieldLand, fieldRepo, fieldGranted},
	},
	KindApplicationDenied: {
		subject: `Your application for {{.Land}} was not approved`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The operator did not approve your application for {{.Land}} ({{.Repo}}).

{{if .Note}}They wrote:

{{.Note}}{{else}}They did not give a reason.{{end}}

You are welcome to apply again.`,
		linkLabel: "See your applications",
		link:      applicationsLink,
		requires:  []field{fieldApplicationID, fieldLand, fieldRepo},
	},
	KindApplicationQuestion: {
		subject: `A question about your application for {{.Land}}`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The operator has a question about your application for {{.Land}} ({{.Repo}}):

{{.Note}}

Your application stays open while you answer.{{if .ReplyTo}} Reply to this email to answer.{{end}}`,
		linkLabel: "See your applications",
		link:      applicationsLink,
		requires:  []field{fieldApplicationID, fieldLand, fieldRepo, fieldNote},
	},
	KindApplicationWithdrawn: {
		subject: `You withdrew your application for {{.Land}}`,
		body: `Hello{{with .Name}} {{.}}{{end}},

Your application for {{.Land}} ({{.Repo}}) is withdrawn, and the operator will not decide on it.

If you did not mean to withdraw it, you are welcome to apply again.`,
		linkLabel: "See your applications",
		link:      applicationsLink,
		requires:  []field{fieldApplicationID, fieldLand, fieldRepo},
	},
	KindLandReady: {
		subject: `Your land {{.Land}} is ready`,
		body: `Hello{{with .Name}} {{.}}{{end}},

Your land {{.Land}} is ready, and its agents work on {{.Repo}}. The steward is waiting for you in #general.

Your purse holds {{usd .RemainingUSD}} of credit. Agents spend it as they work, and we will email you when it runs low.`,
		linkLabel: "Open {{.Land}}",
		link:      landLink,
		requires:  []field{fieldLand, fieldRepo},
		tenant:    true,
	},
	KindPurseLow: {
		subject: `{{.Land}} is running low on credit`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The purse for {{.Land}} has {{usd .RemainingUSD}} left of {{usd .GrantedUSD}}.

When it runs out, no new agents start and running agents pause. To add credit, ask the operator{{if .ReplyTo}} by replying to this email{{else}} in #support{{end}}.`,
		linkLabel: "Open {{.Land}}",
		link:      landLink,
		requires:  []field{fieldLand, fieldGranted},
		tenant:    true,
	},
	KindPurseEmpty: {
		subject: `{{.Land}} is out of credit`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The purse for {{.Land}} is empty. New agents will not start, and running agents have been asked to push their work and pause.

When the operator adds credit, the steward decides what to pick up again. To add credit, ask the operator{{if .ReplyTo}} by replying to this email{{else}} in #support{{end}}.`,
		linkLabel: "Open {{.Land}}",
		link:      landLink,
		requires:  []field{fieldLand},
		tenant:    true,
	},
	KindLandFrozen: {
		subject: `{{.Land}} is frozen`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The operator has frozen {{.Land}}. Its agents are pausing, no new agents will start, and the land is read-only until it is thawed.

{{if .Note}}They wrote:

{{.Note}}{{else}}They did not give a reason.{{end}}`,
		linkLabel: "Open {{.Land}}",
		link:      landLink,
		requires:  []field{fieldLand},
		tenant:    true,
	},
	KindLandClosed: {
		subject: `{{.Land}} is closed`,
		body: `Hello{{with .Name}} {{.}}{{end}},

The operator has closed {{.Land}}. Its agents have stopped and its credit is no longer available.

{{if .Note}}They wrote:

{{.Note}}

{{end}}{{if .Repo}}Please uninstall the Gardenio Lands GitHub App from {{.Repo}}: it no longer needs access. {{end}}The land's records are kept for the period the privacy policy sets, then deleted.`,
		linkLabel: "Read the privacy policy",
		link:      func(Data) string { return "/lands/privacy" },
		requires:  []field{fieldLand},
		tenant:    true,
	},
	KindLandJoined: {
		subject: `You have joined {{.Land}}`,
		body: `Hello{{with .Name}} {{.}}{{end}},

You were invited to {{.Land}}, and you joined it when you signed in. Its channels and agents are on the page below.

If you did not expect this, you can ignore it. The land's owner can take you out again at any time.`,
		linkLabel: "Open {{.Land}}",
		link:      landLink,
		requires:  []field{fieldLand},
		footer:    "You are receiving this because the owner of a Gardenio land invited your GitHub account.",
	},
	KindInvite: {
		subject: `You are invited to Gardenio Lands`,
		body: `Hello,

{{.InvitedBy}} invited you to Gardenio Lands{{with .Land}}, to the land {{.}}{{end}}.

To accept, open the page below and sign in with GitHub using this email address. The invite is matched to it, so the GitHub account must have this address, and you will be asked to confirm it the first time.

If you did not expect this, you can ignore it. The invite ends on its own.`,
		linkLabel: "Sign in to Gardenio Lands",
		link:      func(Data) string { return "/lands/" },
		requires:  []field{fieldInvitedBy},
		footer:    "You are receiving this because someone at Gardenio invited this email address.",
	},
}

// templateFuncs are the helpers the subject and body templates call.
var templateFuncs = template.FuncMap{"usd": formatUSD}

// formatUSD renders dollars as "$50" when whole and "$12.50" otherwise.
func formatUSD(v float64) string {
	cents := int64(v*100 + 0.5)
	if v < 0 {
		cents = int64(v*100 - 0.5)
	}
	sign := ""
	if cents < 0 {
		sign, cents = "-", -cents
	}
	if cents%100 == 0 {
		return sign + "$" + strconv.FormatInt(cents/100, 10)
	}
	return fmt.Sprintf("%s$%d.%02d", sign, cents/100, cents%100)
}

// compiled is one Kind's parsed templates.
type compiled struct {
	def     templateDef
	subject *template.Template
	body    *template.Template
	label   *template.Template
}

var compiledDefs = func() map[Kind]compiled {
	out := make(map[Kind]compiled, len(defs))
	for k, d := range defs {
		out[k] = compiled{
			def:     d,
			subject: template.Must(template.New(string(k) + ".subject").Funcs(templateFuncs).Option("missingkey=error").Parse(d.subject)),
			body:    template.Must(template.New(string(k) + ".body").Funcs(templateFuncs).Option("missingkey=error").Parse(d.body)),
			label:   template.Must(template.New(string(k) + ".label").Funcs(templateFuncs).Option("missingkey=error").Parse(d.linkLabel)),
		}
	}
	return out
}()

// htmlLayout wraps a message's paragraphs and call to action. Every value is
// escaped by html/template; the operator's free text arrives as ordinary
// paragraphs, never as markup.
var htmlLayout = htmltemplate.Must(htmltemplate.New("layout").Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Subject}}</title>
</head>
<body style="margin:0;padding:0;background:#f6f3e8;color:#2f3a2a;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',Helvetica,Arial,sans-serif;font-size:16px;line-height:1.5;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="background:#f6f3e8;">
<tr><td align="center" style="padding:24px 16px;">
<table role="presentation" width="100%" cellpadding="0" cellspacing="0" style="max-width:560px;background:#fffdf6;border:1px solid #e3dcc6;border-radius:12px;">
<tr><td style="padding:24px;">
<p style="margin:0 0 16px;font-size:13px;letter-spacing:0.04em;text-transform:uppercase;color:#6b7a5c;">Gardenio Lands</p>
{{range .Paragraphs}}<p style="margin:0 0 16px;">{{range $i, $line := .}}{{if $i}}<br>{{end}}{{$line}}{{end}}</p>
{{end}}<p style="margin:24px 0 0;"><a href="{{.LinkURL}}" style="display:inline-block;padding:12px 20px;background:#4f7a3a;color:#ffffff;text-decoration:none;border-radius:8px;font-weight:600;">{{.LinkLabel}}</a></p>
</td></tr>
</table>
<p style="margin:16px 0 0;font-size:13px;color:#6b7a5c;">{{.Footer}}</p>
</td></tr>
</table>
</body>
</html>
`))

// Templates renders Lands mail. Its base URL makes every link absolute, and
// whether a reply-to address is configured decides whether a mail may say
// "reply to this email".
type Templates struct {
	baseURL string
	replyTo bool
}

// NewTemplates returns Templates for cfg's base URL and reply-to address.
func NewTemplates(cfg Config) *Templates {
	return &Templates{baseURL: strings.TrimRight(cfg.BaseURL, "/"), replyTo: cfg.ReplyTo != ""}
}

// ErrUnknownKind is returned by Render for a Kind with no template.
var ErrUnknownKind = errors.New("mailer: unknown template kind")

// ErrMissingData is returned by Render when Data lacks a field its Kind
// requires.
var ErrMissingData = errors.New("mailer: missing template data")

// Render builds the Message of Kind k for one recipient. It refuses an
// unknown Kind, a Data missing a field the Kind requires, and a Templates
// with no base URL, so a mail never goes out with a broken link.
func (t *Templates) Render(k Kind, to string, d Data) (Message, error) {
	c, ok := compiledDefs[k]
	if !ok {
		return Message{}, fmt.Errorf("%w: %q", ErrUnknownKind, k)
	}
	if t.baseURL == "" {
		return Message{}, errors.New("mailer: templates have no base URL")
	}
	if err := checkRequired(k, c.def.requires, d); err != nil {
		return Message{}, err
	}
	view := struct {
		Data
		ReplyTo bool
	}{Data: d, ReplyTo: t.replyTo}

	subject, err := execText(c.subject, view)
	if err != nil {
		return Message{}, err
	}
	subject = strings.Join(strings.Fields(subject), " ")
	body, err := execText(c.body, view)
	if err != nil {
		return Message{}, err
	}
	label, err := execText(c.label, view)
	if err != nil {
		return Message{}, err
	}
	linkURL := t.baseURL + c.def.link(d)
	footer := "You are receiving this because you applied for a Gardenio land."
	if c.def.tenant {
		footer = "You are receiving this because you own a Gardenio land."
	}
	if c.def.footer != "" {
		footer = c.def.footer
	}
	if t.replyTo {
		footer += " Reply to this email to reach the operator."
	}

	paragraphs := splitParagraphs(body)
	var text strings.Builder
	for _, p := range paragraphs {
		text.WriteString(strings.Join(p, "\n"))
		text.WriteString("\n\n")
	}
	fmt.Fprintf(&text, "%s: %s\n\n-- \n%s\n", label, linkURL, footer)

	var html bytes.Buffer
	if err := htmlLayout.Execute(&html, map[string]any{
		"Subject":    subject,
		"Paragraphs": paragraphs,
		"LinkURL":    linkURL,
		"LinkLabel":  label,
		"Footer":     footer,
	}); err != nil {
		return Message{}, fmt.Errorf("mailer: render %s html: %w", k, err)
	}

	return Message{
		Kind:    k,
		To:      []string{to},
		Subject: subject,
		Text:    text.String(),
		HTML:    html.String(),
	}, nil
}

func checkRequired(k Kind, requires []field, d Data) error {
	for _, f := range requires {
		var missing string
		switch f {
		case fieldApplicationID:
			if strings.TrimSpace(d.ApplicationID) == "" {
				missing = "ApplicationID"
			}
		case fieldLand:
			if strings.TrimSpace(d.Land) == "" {
				missing = "Land"
			}
		case fieldRepo:
			if strings.TrimSpace(d.Repo) == "" {
				missing = "Repo"
			}
		case fieldNote:
			if strings.TrimSpace(d.Note) == "" {
				missing = "Note"
			}
		case fieldGranted:
			if d.GrantedUSD <= 0 {
				missing = "GrantedUSD"
			}
		case fieldInvitedBy:
			if strings.TrimSpace(d.InvitedBy) == "" {
				missing = "InvitedBy"
			}
		}
		if missing != "" {
			return fmt.Errorf("%w: %s needs %s", ErrMissingData, k, missing)
		}
	}
	return nil
}

func execText(t *template.Template, data any) (string, error) {
	var b strings.Builder
	if err := t.Execute(&b, data); err != nil {
		return "", fmt.Errorf("mailer: render %s: %w", t.Name(), err)
	}
	return b.String(), nil
}

// splitParagraphs splits a rendered body at blank lines into paragraphs of
// lines, dropping empty ones and trailing spaces. The operator's free text
// keeps its own line breaks and paragraphs.
func splitParagraphs(body string) [][]string {
	body = strings.ReplaceAll(body, "\r\n", "\n")
	var out [][]string
	var cur []string
	flush := func() {
		if len(cur) > 0 {
			out = append(out, cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()
	return out
}
