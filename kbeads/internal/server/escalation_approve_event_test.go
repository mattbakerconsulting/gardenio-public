package server

import (
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// TestApproveEscalation_PublishesApprovedByChange: the bridge wakes an
// executor only on an update event whose Changes record the approval, and
// ignores later writes to the approved bead (gd-TMYvBxNY6h). So the approve
// endpoint must put approved_by in Changes, including for an escalation that
// was already in_progress, where there is no status flip to publish.
func TestApproveEscalation_PublishesApprovedByChange(t *testing.T) {
	for _, startStatus := range []model.Status{model.StatusOpen, model.StatusInProgress} {
		t.Run(string(startStatus), func(t *testing.T) {
			ms := newMockStore()
			cap := &capturePublisher{}
			s := NewBeadsServer(ms, cap)
			s.userProvisioner = nil
			s.SetForwardAuthClients([]string{testBrowserClient})
			h := defaultBearerMiddleware(s.NewHTTPHandler(&bearerSubjectVerifier{}))
			createRec := doJSON(t, h, "POST", "/v1/beads", map[string]any{
				"title": "create role",
				"type":  "escalation",
				"kind":  "data",
				"fields": map[string]any{
					"operation_type": "aws_iam",
					"operation_spec": map[string]any{"role_name": "deploy-bot"},
				},
			})
			requireStatus(t, createRec, 201)
			var created map[string]any
			decodeJSON(t, createRec, &created)
			id := created["id"].(string)
			ms.beads[id].Status = startStatus

			cap.mu.Lock()
			before := len(cap.topics)
			cap.mu.Unlock()

			rec := doJSON(t, h, "POST", "/v1/escalations/"+id+"/approve", map[string]any{})
			requireStatus(t, rec, 200)

			cap.mu.Lock()
			defer cap.mu.Unlock()
			var approval *events.BeadUpdated
			for i := before; i < len(cap.topics); i++ {
				if cap.topics[i] != events.TopicBeadUpdated {
					continue
				}
				if p, ok := cap.payloads[i].(events.BeadUpdated); ok && p.Bead != nil && p.Bead.ID == id {
					if _, has := p.Changes["approved_by"]; has {
						approval = &p
						break
					}
				}
			}
			if approval == nil {
				t.Fatalf("approve published no %s with approved_by in Changes", events.TopicBeadUpdated)
			}
			if got := approval.Changes["approved_by"]; got != defaultTestSubject {
				t.Errorf("Changes[approved_by]=%v, want %s", got, defaultTestSubject)
			}
			// P7 Task 12: the dispatch is claimed before this event, so the
			// local watcher reads execution_dispatch off its payload.
			if got := approval.Changes["execution_dispatch"]; got != dispatchNone {
				t.Errorf("Changes[execution_dispatch]=%v, want %s", got, dispatchNone)
			}
			if got := escalationStringFields(approval.Bead)["execution_dispatch"]; got != dispatchNone {
				t.Errorf("event bead execution_dispatch=%q, want %s", got, dispatchNone)
			}
			_, statusChanged := approval.Changes["status"]
			if wantStatus := startStatus == model.StatusOpen; statusChanged != wantStatus {
				t.Errorf("Changes has status=%v, want %v (start status %s)", statusChanged, wantStatus, startStatus)
			}
		})
	}
}
