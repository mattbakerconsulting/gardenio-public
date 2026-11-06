package decisionview

import (
	"testing"

	"gardenio/controller/internal/beadsapi"
)

func TestFromReadsPromptContextAndOptions(t *testing.T) {
	b := &beadsapi.BeadDetail{ID: "gd-1", Title: "T", Status: "open", CreatedBy: "crew-1",
		Description: "  older writers put the background here  ",
		Fields: map[string]string{
			"prompt":  " Ship it? ",
			"options": `[{"id":"ship","short":"Ship","label":"Ship it today","description":"Merge PR #42","artifact_type":"report"},"Wait",{"id":"","short":"","label":""}]`,
			"files":   `[{"name":"a.png","mimetype":"image/png","url":"https://x/a.png","width":10,"height":5},{"name":"bad","url":"/relative"}]`,
		}}
	d := From(b)
	if d.ID != "gd-1" || d.Prompt != "Ship it?" || !d.Open || d.RequestedBy != "crew-1" {
		t.Fatalf("basic fields: %+v", d)
	}
	if d.Context != "older writers put the background here" || d.ContextSource != "description" {
		t.Errorf("context must fall back to the description: %q from %q", d.Context, d.ContextSource)
	}
	if len(d.Options) != 2 {
		t.Fatalf("options: %+v", d.Options)
	}
	o := d.Options[0]
	if o.N != 1 || o.ID != "ship" || o.Short != "Ship" || o.Label != "Ship it today" || o.Description != "Merge PR #42" || o.ArtifactType != "report" || o.Title() != "Ship" {
		t.Errorf("object option: %+v", o)
	}
	if w := d.Options[1]; w.N != 2 || w.ID != "" || w.Label != "Wait" || w.Title() != "Wait" {
		t.Errorf("bare string option keeps its label, has no id: %+v", w)
	}
	if len(d.Attachments) != 1 || d.Attachments[0].Name != "a.png" || !d.Attachments[0].Image() || d.Attachments[0].Width != 10 {
		t.Errorf("attachments: %+v", d.Attachments)
	}
}

func TestContextPrefersTheField(t *testing.T) {
	b := &beadsapi.BeadDetail{Description: "desc", Fields: map[string]string{"context": " the field "}}
	if got, src := Context(b); got != "the field" || src != "context" {
		t.Errorf("got %q from %q", got, src)
	}
	b = &beadsapi.BeadDetail{Description: "   ", Fields: map[string]string{"context": ""}}
	if got, src := Context(b); got != "" || src != "" {
		t.Errorf("whitespace is none: %q from %q", got, src)
	}
	if got, _ := Context(nil); got != "" {
		t.Error("nil bead has no context")
	}
}

func TestFromFallsBackToQuestionThenTitle(t *testing.T) {
	if d := From(&beadsapi.BeadDetail{Title: "T", Fields: map[string]string{"question": "Q"}}); d.Prompt != "Q" {
		t.Errorf("question key: %q", d.Prompt)
	}
	if d := From(&beadsapi.BeadDetail{Title: "T", Status: "closed"}); d.Prompt != "T" || d.Open {
		t.Errorf("title fallback / closed: %+v", d)
	}
	if d := From(nil); d.ID != "" || d.Options != nil {
		t.Error("nil bead is the zero decision")
	}
}

func TestParseOptionsShapes(t *testing.T) {
	if ParseOptions("") != nil || ParseOptions("not json") != nil || ParseOptions(`{"id":"x"}`) != nil || ParseOptions(`[]`) != nil {
		t.Error("empty, non-JSON, non-array and empty array read as no options")
	}
	got := ParseOptions(`[{"id":"a"}, 7, "", "b", {"label":"c"}]`)
	if len(got) != 3 || got[0].ID != "a" || got[0].N != 1 || got[1].Label != "b" || got[1].N != 2 || got[2].Label != "c" || got[2].N != 3 {
		t.Errorf("one bad element never drops the rest; numbering skips dropped ones: %+v", got)
	}
	if got := ParseOptions(`[{"id":"a"},{"id":"b","recommended":true},{"id":"c","recommended":"true"}]`); got[0].Recommended || !got[1].Recommended || got[2].Recommended {
		t.Errorf("recommended is a JSON true: %+v", got)
	}
	if (Option{N: 4}).Title() != "4" || (Option{N: 1, ID: "id"}).Title() != "id" || (Option{Label: "l", ID: "id"}).Title() != "l" {
		t.Error("Title: short, label, id, number")
	}
}

func TestParseAttachments(t *testing.T) {
	if ParseAttachments("") != nil || ParseAttachments("x") != nil || ParseAttachments(`[{"url":"ftp://x"}]`) != nil {
		t.Error("empty, malformed and non-http read as none")
	}
	got := ParseAttachments(`[{"url":"HTTPS://x/f","mimetype":"application/pdf"}]`)
	if len(got) != 1 || got[0].Name != "file" || got[0].Image() {
		t.Errorf("%+v", got)
	}
}
