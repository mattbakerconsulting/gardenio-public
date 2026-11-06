package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// Every retired /agents/chat* URL lands somewhere (Lands Commons I step 10,
// gd-EwISaXh6Xk): an agent or message with a thread in the Commons is sent to
// that thread, an agent with no thread to its card in #crew, one with neither
// opens its bead in the garden, and the page and its rail open the garden.
func TestAgentChatRedirectsToTheCommons(t *testing.T) {
	d := commonstest.TwoLands(time.Now().Add(-20 * time.Minute))
	cl := []string{"project:claimlayer"}
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent1", Type: "agent", Title: "claimlayer-crew-1", Status: "open", Labels: cl,
		Fields: map[string]string{"thread_id": "cl-wish1"}})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent2", Type: "agent", Title: "claimlayer-crew-2", Status: "open", Labels: cl})
	d.Put(&beadsapi.BeadDetail{ID: "cl-agent9", Type: "agent", Title: "claimlayer-crew-9", Status: "open", Labels: cl})
	serve := func(withCommons bool) http.Handler {
		srv := NewServer(newFake(), testLogger(), "", "", "", &fakeScreenClient{})
		if withCommons {
			srv.SetCommonsHandler(newCommonsHandler(d, nil, nil, "", nil))
		}
		mux := http.NewServeMux()
		srv.RegisterRoutes(mux)
		return scopeMiddleware(mux, func(r *http.Request) string { _, p := mux.Handler(r); return p })
	}
	thread := "/garden/c/claimlayer/general/t/cl-wish1"
	for _, c := range []struct {
		path, want string
		commons    bool
	}{
		{"/agents/chat/conversation/cl-agent1", thread, true},
		{"/agents/chat/card/cl-agent1", thread, true},
		{"/agents/chat/message/cl-rep1", thread, true},
		{"/agents/chat?agent=cl-agent1&scope=everyone", thread, true},
		{"/agents/chat/conversation/cl-agent2", "/garden/c/claimlayer/crew/t/cl-card2", true},
		{"/agents/chat/conversation/cl-agent9", "/garden?bead=cl-agent9", true},
		{"/agents/chat/conversation/cl-nope", "/garden?bead=cl-nope", true},
		{"/agents/chat", "/garden", true},
		{"/agents/chat/rail?scope=mine", "/garden", true},
		{"/agents/chat/conversation/cl-agent1", "/garden?bead=cl-agent1", false},
	} {
		r := requestAs("GET", c.path, adminPrincipal())
		rec := httptest.NewRecorder()
		serve(c.commons).ServeHTTP(rec, r.WithContext(beadsapi.WithUserBearer(r.Context(), adminBearer)))
		if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != c.want {
			t.Errorf("GET %s (commons=%v) = %d to %q, want 303 to %q", c.path, c.commons, rec.Code, rec.Header().Get("Location"), c.want)
		}
	}
}
