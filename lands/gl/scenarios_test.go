//go:build conformance

package main

// The scenario replay (Door D.12, task gd-VMGMkafIBm; contract design
// gd-EWSKPH7pa5 §3 and §8 step 10): each scenario under
// ../contract/scenarios/ is seeded into each land the way the crew's own
// tooling writes it (scenarioGivens), and its steps are replayed through the
// land door as the land's member. Golden fixtures prove the contract's
// shapes; these prove its meaning on a real daemon, which is the only place a
// kbeads rename that keeps the old field shows: a 200 with the wrong answer.
// The format is checked against the spec in ../contract/scenarios_test.go.

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gardenio/lands/contract/landsv1"
)

type scenario struct {
	Scenario string         `json:"scenario"`
	Steps    []scenarioStep `json:"steps"`
}

type scenarioStep struct {
	Say    string            `json:"say"`
	When   string            `json:"when"`
	Call   string            `json:"call"`
	Body   json.RawMessage   `json:"body"`
	Status int               `json:"status"`
	Schema string            `json:"schema"`
	Match  string            `json:"match"`
	Answer json.RawMessage   `json:"answer"`
	Bind   map[string]string `json:"bind"`
	Any    []string          `json:"any"`
}

// scenarioTypes are the contract types a step's answer (and its page) decode
// into, strictly.
var scenarioTypes = map[string]func() any{
	"Decision":     func() any { return new(landsv1.Decision) },
	"DecisionPage": func() any { return new(landsv1.DecisionPage) },
	"Ticket":       func() any { return new(landsv1.Ticket) },
	"TicketPage":   func() any { return new(landsv1.TicketPage) },
	"Member":       func() any { return new(landsv1.Member) },
	"MemberPage":   func() any { return new(landsv1.MemberPage) },
	"Release":      func() any { return new(landsv1.Release) },
	"ReleasePage":  func() any { return new(landsv1.ReleasePage) },
}

var scenarioPlaceholder = regexp.MustCompile(`\{([a-z_]+)\}`)

func loadScenarios(t *testing.T) []scenario {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("..", "contract", "scenarios", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no scenarios under ../contract/scenarios: %v", err)
	}
	sort.Strings(paths)
	var out []scenario
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var sc scenario
		if err := json.Unmarshal(raw, &sc); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
		out = append(out, sc)
	}
	return out
}

// ── Givens ──────────────────────────────────────────────────────────────────

// scenarioGivens seed a scenario's given into l and return the seeded ids by
// the names the scenario's steps use.
var scenarioGivens = map[string]func(s *suite, l *land) map[string]string{
	// gb decision create --requesting-task: the decision carries the ticket
	// in requesting_task and the ticket a blocks edge on the decision.
	"open_decision": func(s *suite, l *land) map[string]string {
		ticket := s.seedTicket(l, "Hang the autumn banner")
		prompt := "Keep the current logo on the autumn banner, or draw a new one?"
		decision := s.createBead(map[string]any{
			"title": prompt, "short_title": "Logo for the banner", "type": "decision", "kind": "data",
			"labels": []string{"project:" + l.name}, "force": true,
			"fields": map[string]any{
				"prompt":  prompt,
				"context": "The banner goes to the printer on Friday.",
				"options": []map[string]string{
					{"id": "keep", "short": "Keep", "label": "Keep the current logo", "artifact_type": "report"},
					{"id": "new", "short": "Draw a new logo", "artifact_type": "plan"},
				},
				"requested_by":    "orchard-crew",
				"requesting_task": ticket,
			},
		})
		l.owns(decision)
		s.addBlocker(ticket, decision)
		return map[string]string{"ticket": ticket, "decision": decision}
	},
	// gb image upload's viewer links in a comment on the ticket, and a
	// picture from another site; the decision is asked about the ticket.
	"decision_evidence": func(s *suite, l *land) map[string]string {
		ticket := s.seedTicket(l, "Hang the autumn banner")
		shots := "https://viewer.example/beads/image/coop/sessions/images/" + ticket
		text := "Before: ![front](" + shots + "/1a2b3c4d-front.png) and " + shots + "/5e6f7a8b-back.png. Also https://cdn.example/banner.png"
		if r := s.direct(http.MethodPost, "/v1/beads/"+ticket+"/comments", map[string]any{"text": text}); r.status < 200 || r.status > 299 {
			s.t.Fatalf("comment on %s: %s", ticket, r)
		}
		prompt := "Keep the current logo on the autumn banner?"
		decision := s.createBead(map[string]any{
			"title": prompt, "short_title": "Logo for the banner", "type": "decision", "kind": "data",
			"labels": []string{"project:" + l.name}, "force": true,
			"fields": map[string]any{
				"prompt":          prompt,
				"options":         []map[string]string{{"id": "keep", "label": "Keep the current logo"}},
				"requested_by":    "orchard-crew",
				"requesting_task": ticket,
			},
		})
		l.owns(decision)
		return map[string]string{"ticket": ticket, "decision": decision}
	},
	"blocked_ticket": func(s *suite, l *land) map[string]string {
		ids := map[string]string{
			"blocker":   s.seedTicket(l, "Buy the paint"),
			"waits":     s.seedTicket(l, "Paint the gate"),
			"elsewhere": s.seedTicket(l, "Mend the shared fence"),
			"by_hand":   s.seedTicket(l, "Wait for the rain to stop"),
			"on_hidden": s.seedTicket(l, "Prune the hedge"),
		}
		chore := s.createBead(map[string]any{
			"title": "Rotate the crew's logs", "short_title": "Rotate logs", "type": "chore", "kind": "issue",
			"priority": 2, "labels": []string{"project:" + l.name},
		})
		l.owns(chore)
		s.addBlocker(ids["waits"], ids["blocker"])
		s.addBlocker(ids["elsewhere"], s.other(l).ticket)
		s.addBlocker(ids["on_hidden"], chore)
		if r := s.direct(http.MethodPatch, "/v1/beads/"+ids["by_hand"], map[string]any{"status": "blocked"}); r.status != http.StatusOK {
			s.t.Fatalf("mark %s blocked: %s", ids["by_hand"], r)
		}
		return ids
	},
	// Two tags pushed, as github-bridge records them; the first reported on
	// prod the way the land's CD reports it.
	"release_deployed": func(s *suite, l *land) map[string]string {
		pr := func(n int, title string) map[string]any {
			return map[string]any{"pr": n, "title": title, "url": "https://github.com/orchard/shop/pull/" + strconv.Itoa(n)}
		}
		first := s.createRelease(l.name, "v2.0.0", "2026-10-20T09:00:00Z", []map[string]any{pr(11, "Build the first shelf")})
		second := s.createRelease(l.name, "v2.1.0", "2026-10-24T09:00:00Z", []map[string]any{
			pr(12, "Show the autumn banner"), pr(13, "Fix the price label"),
			{"title": "Bump the copyright year", "merge_sha": "9c1f2e0a"},
		})
		l.owns(first, second)
		r := s.direct(http.MethodPost, "/v1/releases/"+first+"/deployments", map[string]any{
			"target": "prod", "since": "2026-10-20T10:00:00Z", "by": "orchard-deploy", "revision": "7",
		})
		if r.status != http.StatusOK {
			s.t.Fatalf("deploy %s: %s", first, r)
		}
		return map[string]string{"first": first, "second": second}
	},
	// Nobody to seed: the land's member has signed in; sam signs in in a step.
	"crew_after_rename": func(*suite, *land) map[string]string { return map[string]string{} },
}

func (s *suite) seedTicket(l *land, title string) string {
	s.t.Helper()
	id := s.createBead(map[string]any{
		"title": title, "short_title": title, "type": "task", "kind": "issue",
		"priority": 2, "labels": []string{"project:" + l.name},
	})
	l.owns(id)
	return id
}

// addBlocker holds id back on blocker, as `kd dep add` and gb decision create
// do: id depends on blocker through a blocks edge.
func (s *suite) addBlocker(id, blocker string) {
	s.t.Helper()
	r := s.direct(http.MethodPost, "/v1/beads/"+id+"/dependencies", map[string]any{"depends_on_id": blocker, "type": "blocks"})
	if r.status < 200 || r.status > 299 {
		s.t.Fatalf("%s blocks on %s: %s", id, blocker, r)
	}
}

// ── Whens ───────────────────────────────────────────────────────────────────

// scenarioWhens are what happens to a land between steps; ids are the
// scenario's ids so far.
var scenarioWhens = map[string]func(s *suite, l *land, ids map[string]string){
	"sam signs in as an owner": func(s *suite, l *land, _ map[string]string) {
		s.signInAs(l, samEmail(l), "/project-"+l.name+"/owners")
	},
	"sam signs in as a member": func(s *suite, l *land, _ map[string]string) { s.signInAs(l, samEmail(l), "/project-"+l.name) },
	// Through the land door, as gl release deployed sends it.
	"the land's owner reports second running on prod": func(s *suite, l *land, ids map[string]string) {
		path := "/v1/lands/v1/lands/" + l.name + "/releases/" + ids["second"] + "/deployments"
		s.ok("reportDeployment as the owner", s.via(l.owner, l.name, "POST", path, `{"target":"prod","revision":"8"}`), 200, &landsv1.Release{})
	},
	"sam is renamed Sam Rowan": func(s *suite, l *land, _ map[string]string) { s.rename(samEmail(l), "Sam Rowan") },
	// The crew made blocker, so the crew closes it: the daemon lets a member
	// close only a ticket they made, hold or own.
	"the crew closes blocker": func(s *suite, _ *land, ids map[string]string) {
		if r := s.direct(http.MethodPost, "/v1/beads/"+ids["blocker"]+"/close", map[string]any{"reason": "Paint bought."}); r.status < 200 || r.status > 299 {
			s.t.Fatalf("close %s: %s", ids["blocker"], r)
		}
	},
}

func samEmail(l *land) string { return "sam@" + l.name + ".example" }

// signInAs is a person's first request to the land with a token carrying
// group, which is when the daemon records their membership and tier.
func (s *suite) signInAs(l *land, email, group string) {
	s.t.Helper()
	tok := s.is.mint(person{email: email, roles: []string{roleMember}, groups: []string{group}, lands: []string{l.name}})
	s.ok("whoami as "+email, s.via(tok, l.name, "GET", "/v1/lands/v1/lands/"+l.name+"/whoami", ""), 200, &landsv1.Member{})
}

// rename retitles a person's user bead, as an operator renames someone.
func (s *suite) rename(email, name string) {
	s.t.Helper()
	r := s.direct(http.MethodGet, "/v1/beads?type=user&limit=200", nil)
	var page struct {
		Beads []struct {
			ID     string `json:"id"`
			Fields struct {
				Email string `json:"email"`
			} `json:"fields"`
		} `json:"beads"`
	}
	if r.status != http.StatusOK || json.Unmarshal(r.body, &page) != nil {
		s.t.Fatalf("list people: %s", r)
	}
	for _, b := range page.Beads {
		if strings.EqualFold(b.Fields.Email, email) {
			if r := s.direct(http.MethodPatch, "/v1/beads/"+b.ID, map[string]any{"title": name}); r.status != http.StatusOK {
				s.t.Fatalf("rename %s: %s", email, r)
			}
			return
		}
	}
	s.t.Fatalf("no person %s among %d", email, len(page.Beads))
}

// ── The replay ──────────────────────────────────────────────────────────────

func (s *suite) scenarios(l *land) {
	for _, sc := range loadScenarios(s.t) {
		s.t.Run(sc.Scenario, func(t *testing.T) { s.sub(t).scenario(l, sc) })
	}
}

func (s *suite) scenario(l *land, sc scenario) {
	given, ok := scenarioGivens[sc.Scenario]
	if !ok {
		s.t.Fatalf("scenario %s has no given in scenarioGivens", sc.Scenario)
	}
	ids := given(s, l)
	for i, st := range sc.Steps {
		where := fmt.Sprintf("%s step %d (%s)", sc.Scenario, i+1, st.Say)
		if st.When != "" {
			when, ok := scenarioWhens[st.When]
			if !ok {
				s.t.Fatalf("%s: no %q in scenarioWhens", where, st.When)
			}
			when(s, l, ids)
		}
		method, target, _ := strings.Cut(st.Call, " ")
		target = scenarioPlaceholder.ReplaceAllStringFunc(target, func(m string) string {
			id, ok := ids[m[1:len(m)-1]]
			if !ok {
				s.t.Fatalf("%s: %s is bound by no given and no earlier step", where, m)
			}
			return url.PathEscape(id)
		})
		r := s.api(l, method, target, string(st.Body))
		var want any
		if err := json.Unmarshal(st.Answer, &want); err != nil {
			s.t.Fatal(err)
		}
		mk, ok := scenarioTypes[st.Schema]
		if st.Match != "exact" {
			mk, ok = scenarioTypes[st.Schema+"Page"]
		}
		if !ok {
			s.t.Fatalf("%s: no type for %s in scenarioTypes", where, st.Schema)
		}
		if !s.ok(where, r, st.Status, mk()) {
			continue
		}
		if st.Match == "exact" {
			var got any
			_ = json.Unmarshal(r.body, &got)
			if diff := matchAnswer(st, want, got, ids, false); diff != "" {
				s.t.Errorf("%s: %s", where, diff)
			}
			continue
		}
		var page struct {
			Items []any `json:"items"`
		}
		_ = json.Unmarshal(r.body, &page)
		found := false
		for _, it := range page.Items {
			trial := maps.Clone(ids)
			if matchAnswer(st, want, it, trial, st.Match == "unlisted") == "" {
				found = true
				if st.Match == "listed" {
					ids = trial
				}
				break
			}
		}
		switch {
		case st.Match == "listed" && !found:
			s.t.Errorf("%s: %s %s lists no %s\n%s", where, method, target, st.Answer, r.body)
		case st.Match == "unlisted" && found:
			s.t.Errorf("%s: %s %s still lists it\n%s", where, method, target, r.body)
		}
	}
}

// matchAnswer compares got with the step's answer want. A bound pointer must
// hold the id seeded or captured under its name (an unknown name is captured
// into ids); an any pointer may hold anything; the rest must be equal. With
// onlyBound only the bound pointers are compared. It returns "" on a match.
func matchAnswer(st scenarioStep, want, got any, ids map[string]string, onlyBound bool) string {
	want = cloneJSON(want)
	for p, name := range st.Bind {
		v, ok := pointer(got, p)
		s, isString := v.(string)
		if !ok || !isString {
			return fmt.Sprintf("%s: no id at %s", name, p)
		}
		if id, known := ids[name]; known && id != s {
			return fmt.Sprintf("%s at %s is %q, want %q", name, p, s, id)
		} else if !known {
			if onlyBound {
				return fmt.Sprintf("%s is not bound yet", name)
			}
			ids[name] = s
		}
		setPointer(want, p, s)
	}
	if onlyBound {
		return ""
	}
	for _, p := range st.Any {
		if v, ok := pointer(got, p); ok {
			setPointer(want, p, v)
		}
	}
	if !reflect.DeepEqual(want, got) {
		w, _ := json.Marshal(want)
		g, _ := json.Marshal(got)
		return fmt.Sprintf("answer differs:\n want %s\n  got %s", w, g)
	}
	return ""
}

func cloneJSON(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// pointer reads a "/a/0/b" JSON pointer.
func pointer(doc any, ptr string) (any, bool) {
	cur := doc
	for _, tok := range strings.Split(strings.TrimPrefix(ptr, "/"), "/") {
		switch v := cur.(type) {
		case map[string]any:
			x, ok := v[tok]
			if !ok {
				return nil, false
			}
			cur = x
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(v) {
				return nil, false
			}
			cur = v[i]
		default:
			return nil, false
		}
	}
	return cur, true
}

// setPointer writes val at ptr, which must already exist in doc.
func setPointer(doc any, ptr string, val any) {
	i := strings.LastIndex(ptr, "/")
	parent, ok := pointer(doc, ptr[:i])
	if ptr[:i] == "" {
		parent, ok = doc, true
	}
	if !ok {
		return
	}
	switch v := parent.(type) {
	case map[string]any:
		v[ptr[i+1:]] = val
	case []any:
		if n, err := strconv.Atoi(ptr[i+1:]); err == nil && n >= 0 && n < len(v) {
			v[n] = val
		}
	}
}
