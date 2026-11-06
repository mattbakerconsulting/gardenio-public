package respond

import (
	"encoding/json"
	"errors"
	"html/template"
	"os"
	"strings"
	"testing"

	"gardenio/controller/internal/coopmuxapi"
)

func prompts(t *testing.T) map[string]*coopmuxapi.Prompt {
	t.Helper()
	raw, err := os.ReadFile("testdata/coop_prompts.json")
	if err != nil {
		t.Fatal(err)
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(raw, &all); err != nil {
		t.Fatal(err)
	}
	out := map[string]*coopmuxapi.Prompt{}
	for k, v := range all {
		if strings.HasPrefix(k, "_") {
			continue
		}
		var p coopmuxapi.Prompt
		if err := json.Unmarshal(v, &p); err != nil {
			t.Fatalf("%s: %v", k, err)
		}
		out[k] = &p
	}
	return out
}

func render(t *testing.T, f Form) string {
	t.Helper()
	tpl := template.Must(template.New("t").Parse(Template))
	var b strings.Builder
	if err := tpl.ExecuteTemplate(&b, "respond", f); err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func labels(f Form) []string {
	var out []string
	for _, c := range f.Choices {
		out = append(out, c.Label)
	}
	return out
}

// Every coop fixture renders its options by their labels, and choosing each one sends coop that option's number.
func TestFromPromptAndCheck_EveryFixtureOptionRoundTrips(t *testing.T) {
	for name, p := range prompts(t) {
		f, ok := FromPrompt(p, "/garden/glass/gd-a/send", "glass", Field{"type", "respond"})
		if name == "permission_reading" {
			if ok {
				t.Errorf("%s: a prompt whose options coop is still reading has nothing to answer yet", name)
			}
			continue
		}
		if !ok {
			t.Fatalf("%s: FromPrompt not ok", name)
		}
		for _, c := range f.Choices {
			if c.Label == "" || strings.TrimLeft(c.Label, "0123456789. ") == "" {
				t.Errorf("%s: a bare index for a label: %q", name, c.Label)
			}
			vals := map[string]string{"type": "respond"}
			for _, h := range append(append([]Field(nil), f.Hidden...), c.Hidden...) {
				vals[h.Name] = h.Value
			}
			vals[f.Name] = c.Value
			a, err := ParseAnswer(func(k string) string { return vals[k] })
			if err != nil {
				t.Fatalf("%s %q: %v", name, c.Label, err)
			}
			r, err := Check(p, a)
			if err != nil {
				t.Fatalf("%s %q: %v", name, c.Label, err)
			}
			if r.Keys != nil {
				continue
			}
			got := 0
			switch {
			case r.Request.Option != nil:
				got = *r.Request.Option
			case len(r.Request.Answers) == 1 && r.Request.Answers[0].Option != nil:
				got = *r.Request.Answers[0].Option
			}
			want := c.Value
			if got == 0 || want != itoa(got) {
				t.Errorf("%s %q: sends option %d, want %s", name, c.Label, got, want)
			}
		}
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestFromPrompt_Shapes(t *testing.T) {
	ps := prompts(t)
	f, _ := FromPrompt(ps["bash_permission"], "/x", "glass")
	if got := labels(f); strings.Join(got, "|") != "Yes|Yes, and always allow access to tmp/ from this project|No" || f.Text != nil {
		t.Errorf("bash permission: %q, text %v", got, f.Text)
	}
	f, _ = FromPrompt(ps["plan"], "/x", "glass")
	if got := labels(f); len(got) != 2 || f.Text == nil || f.Text.Submit != "No, keep planning" {
		t.Errorf("plan: the last option takes words: %q, %+v", got, f.Text)
	}
	f, _ = FromPrompt(ps["multi_question_q1"], "/x", "glass")
	if Question(ps["multi_question_q1"]) != "Which language?" || len(f.Choices) != 4 || f.Text == nil || !f.Text.Required {
		t.Errorf("question: %+v", f)
	}
	f, _ = FromPrompt(ps["question_confirm"], "/x", "glass")
	if f.Name != FieldConfirm || strings.Join(labels(f), "|") != "Submit the answers|Cancel" {
		t.Errorf("question confirm: %+v", f)
	}
	f, _ = FromPrompt(ps["oauth_login"], "/x", "glass")
	if f.Link != "https://claude.ai/oauth/authorize?code=true" || strings.Join(labels(f), "|") != "Continue" {
		t.Errorf("oauth: %+v", f)
	}
	f, _ = FromPrompt(&coopmuxapi.Prompt{Type: "setup", Subtype: "oauth_login", Input: "javascript:alert(1)"}, "/x", "glass")
	if f.Link != "" {
		t.Errorf("a sign-in link that is not a web page is not offered: %q", f.Link)
	}
	if Heading(ps["trust_folder"]) != "Trust this workspace?" || Heading(ps["fable_consent"]) != "Consent needed" || Heading(ps["question_confirm"]) != "Answers ready to submit" {
		t.Error("headings")
	}
}

// A stale button never answers: the prompt moved on, or the option at that place says something else now.
func TestCheck_RefusesAStaleAnswer(t *testing.T) {
	ps := prompts(t)
	cases := []struct {
		name string
		p    *coopmuxapi.Prompt
		a    Answer
	}{
		{"another prompt type", ps["plan"], Answer{Prompt: "permission", Option: 1, Label: "Yes", Question: -1}},
		{"the label moved", ps["edit_permission"], Answer{Prompt: "permission", Option: 2, Label: "Yes, and always allow access to tmp/ from this project", Question: -1}},
		{"no such option", ps["bash_permission"], Answer{Prompt: "permission", Option: 4, Label: "Maybe", Question: -1}},
		{"no label", ps["bash_permission"], Answer{Prompt: "permission", Option: 1, Question: -1}},
		{"the dialog moved on", ps["multi_question_q1"], Answer{Prompt: "question", Option: 1, Label: "Rust", Question: 1}},
		{"words on a plan option that takes none", ps["plan"], Answer{Prompt: "plan", Option: 1, Label: "Yes, and auto-accept edits", Text: "hm", Question: -1}},
		{"confirm before the questions are answered", ps["multi_question_q1"], Answer{Prompt: "question", Confirm: ConfirmSubmit, Question: -1}},
	}
	for _, c := range cases {
		if _, err := Check(c.p, c.a); !errors.Is(err, ErrChanged) {
			t.Errorf("%s: err %v, want ErrChanged", c.name, err)
		}
	}
	if _, err := Check(nil, Answer{Prompt: "permission", Option: 1, Label: "Yes"}); !errors.Is(err, ErrNoPrompt) {
		t.Errorf("no prompt: %v", err)
	}
}

func TestCheck_WordsAndConfirm(t *testing.T) {
	ps := prompts(t)
	r, err := Check(ps["plan"], Answer{Prompt: "plan", Option: 3, Label: "No, keep planning", Text: " add tests ", Question: -1})
	if err != nil || *r.Request.Option != 3 || *r.Request.Text != "add tests" {
		t.Fatalf("plan feedback: %+v %v", r, err)
	}
	r, err = Check(ps["multi_question_q1"], Answer{Prompt: "question", Text: "Go", Question: 0})
	if err != nil || len(r.Request.Answers) != 1 || *r.Request.Answers[0].Text != "Go" {
		t.Fatalf("own words: %+v %v", r, err)
	}
	r, err = Check(ps["question_confirm"], Answer{Prompt: "question", Confirm: ConfirmSubmit, Question: -1})
	if err != nil || strings.Join(r.Keys, ",") != "Enter" {
		t.Fatalf("submit: %+v %v", r, err)
	}
}

func TestParseAnswer(t *testing.T) {
	get := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	if _, err := ParseAnswer(get(map[string]string{"prompt": "permission"})); err == nil {
		t.Error("an answer that chooses nothing")
	}
	if _, err := ParseAnswer(get(map[string]string{"option": "1"})); err == nil {
		t.Error("an answer for no prompt")
	}
	if _, err := ParseAnswer(get(map[string]string{"prompt": "permission", "option": "x"})); err == nil {
		t.Error("an option that is not a number")
	}
	if _, err := ParseAnswer(get(map[string]string{"prompt": "plan", "text": strings.Repeat("a", TextMax+1)})); err == nil {
		t.Error("words past the cap")
	}
	a, err := ParseAnswer(get(map[string]string{"prompt": "question", "option": "2", "label": " Python ", "question": "0"}))
	if err != nil || a.Option != 2 || a.Label != "Python" || a.Question != 0 {
		t.Errorf("%+v %v", a, err)
	}
}

// The template escapes what coop read off the screen, and names the script that posts each form.
func TestTemplate_EscapesAndHooks(t *testing.T) {
	p := &coopmuxapi.Prompt{Type: "permission", Options: []string{`<img src=x onerror=alert(1)>`, "No"}}
	f, _ := FromPrompt(p, "/garden/glass/gd-a/send", "glass", Field{"type", "respond"})
	out := render(t, f)
	if strings.Contains(out, "<img") {
		t.Errorf("unescaped label: %s", out)
	}
	if strings.Count(out, `data-glass="respond"`) != 2 || strings.Contains(out, "data-commons") {
		t.Errorf("hook: %s", out)
	}
	for _, want := range []string{`name="type" value="respond"`, `name="prompt" value="permission"`, `name="label" value="No"`, `name="option" value="2"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %s in %s", want, out)
		}
	}
}
