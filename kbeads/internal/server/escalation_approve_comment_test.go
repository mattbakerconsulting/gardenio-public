package server

import (
	"strings"
	"testing"
)

// Approve with comment (gd-yuOG6cX6VI): the approver's note rides the approve
// request and lands as a comment on the escalation, by the approver, and as
// approval_comment with the approval; no note, no comment. A note past
// maxApprovalCommentLen is refused before anything is written.
func TestApproveEscalation_RecordsComment(t *testing.T) {
	ms := newMockStore()
	s := NewBeadsServer(ms, &capturePublisher{})
	s.userProvisioner = nil
	s.SetForwardAuthClients([]string{testBrowserClient})
	h := defaultBearerMiddleware(s.NewHTTPHandler(&bearerSubjectVerifier{}))
	create := func(title string) string {
		rec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
			"title": title, "type": "escalation", "kind": "data",
			"fields": map[string]any{"operation_type": "aws_iam", "operation_spec": map[string]any{"role_name": "deploy-bot"}},
		})
		requireStatus(t, rec, 201)
		var created map[string]any
		decodeJSON(t, rec, &created)
		return created["id"].(string)
	}

	id := create("with a note")
	rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{"comment": "  merge after the cut  "})
	requireStatus(t, rec, 200)
	cs := ms.comments[id]
	if len(cs) != 1 || cs[0].Text != "merge after the cut" || cs[0].Author != defaultTestSubject {
		t.Fatalf("comments after approve with comment: %+v", cs)
	}
	if got := escalationStringFields(ms.beads[id])["approval_comment"]; got != "merge after the cut" {
		t.Errorf("approval_comment = %q", got)
	}

	id = create("no note")
	rec = doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{})
	requireStatus(t, rec, 200)
	if len(ms.comments[id]) != 0 {
		t.Errorf("an approval with no note writes no comment: %+v", ms.comments[id])
	}
	if _, has := escalationStringFields(ms.beads[id])["approval_comment"]; has {
		t.Errorf("an approval with no note writes no approval_comment")
	}

	id = create("too long")
	rec = doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{"comment": strings.Repeat("x", maxApprovalCommentLen+1)})
	requireStatus(t, rec, 400)
	if got := escalationStringFields(ms.beads[id])["approved_by"]; got != "" {
		t.Errorf("a refused comment approves nothing: approved_by=%q", got)
	}
}
