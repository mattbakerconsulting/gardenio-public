package main

import (
	"net/http"
	"net/url"
	"strings"
)

// handleAgentChatRedirect answers every retired /agents/chat* route (Lands
// Commons I step 10, gd-EwISaXh6Xk). The agent chat page and its fragments
// are gone; the Commons is where an agent's conversation is read now. A URL
// that names an agent or a message (/agents/chat/conversation/{id},
// /card/{id}, /message/{id}, or the page's ?agent=) is sent to its thread in
// the Commons, or to its card when it has no thread; one whose bead has
// neither opens the bead in the garden; the bare page and its rail open the
// garden.
func (s *Server) handleAgentChatRedirect(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if id == "" {
		id = strings.TrimSpace(r.URL.Query().Get("agent"))
	}
	to := s.basePath + "/garden"
	if id != "" {
		to += "?bead=" + url.QueryEscape(id)
		if s.commons != nil {
			if p, err := s.commons.Locate(r, id); err == nil {
				to = p.Href
			}
		}
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}
