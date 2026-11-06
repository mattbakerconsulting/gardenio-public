package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// landDoor returns the body of the edge's `location ^~ /land/` block.
func landDoor(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("../../../helm/gardenio-ziti/public-edge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(b)
	i := strings.Index(doc, "location ^~ /land/ {")
	if i < 0 {
		t.Fatal("public-edge.yaml has no `location ^~ /land/`")
	}
	depth := 0
	for j := i; j < len(doc); j++ {
		switch doc[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return doc[i : j+1]
			}
		}
	}
	t.Fatal("unbalanced `location ^~ /land/` block")
	return ""
}

// landgate 404s exactly the routes the edge's land door 404s: it is a second
// copy of that check, so a route opened or closed in one place and not the
// other fails here (gd-ParwLpmgID).
func TestEdgeParity_DeniedRoutes(t *testing.T) {
	door := landDoor(t)
	re := regexp.MustCompile(`location ~ "\^/land/\[a-z\]\[a-z0-9-\]\{0,62\}/v1/\(([^)]+)\)\(/\|\$\)" \{\s*return 404;`)
	var edge []string
	for _, m := range re.FindAllStringSubmatch(door, -1) {
		edge = append(edge, strings.Split(m[1], "|")...)
	}
	if len(edge) == 0 {
		t.Fatal("found no `return 404` route locations in the land door")
	}
	slices.Sort(edge)
	ours := slices.Clone(deniedRoutes)
	slices.Sort(ours)
	if !slices.Equal(edge, ours) {
		t.Errorf("edge land door 404s %v, landgate 404s %v", edge, ours)
	}
}

// Every header the land door blanks, landgate drops too. Connection is
// hop-by-hop: httputil.ReverseProxy never forwards it (gd-ParwLpmgID).
func TestEdgeParity_BlankedHeaders(t *testing.T) {
	door := landDoor(t)
	ms := regexp.MustCompile(`(?m)^\s*proxy_set_header (\S+)\s+"";`).FindAllStringSubmatch(door, -1)
	if len(ms) == 0 {
		t.Fatal("found no blanked headers in the land door")
	}
	for _, m := range ms {
		if m[1] == "Connection" {
			continue
		}
		h := http.Header{m[1]: {"x"}}
		scrub(h)
		if len(h) != 0 {
			t.Errorf("the edge blanks %s; landgate forwards it", m[1])
		}
	}
	h := http.Header{"Authorization": {"Bearer x"}}
	scrub(h)
	if h.Get("Authorization") == "" {
		t.Error("scrub dropped Authorization: the caller's bearer must pass through")
	}
}

// block returns the balanced `{...}` block of doc that opens at the first
// match of head at or after from, and where it ends; "" when there is none.
// Braces inside a quoted location pattern ({0,62}) do not count.
func block(doc, head string, from int) (string, int) {
	i := strings.Index(doc[from:], head)
	if i < 0 {
		return "", -1
	}
	i += from
	depth, quoted := 0, false
	for j := i; j < len(doc); j++ {
		switch c := doc[j]; {
		case c == '"':
			quoted = !quoted
		case quoted:
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return doc[i : j+1], j + 1
			}
		}
	}
	return "", -1
}

// The other way round: every header landgate scrubs by name, the edge blanks
// in each land door location that forwards a caller, so neither wall lets
// through what the other drops. That pins the agent-pod family kbeads honours
// for a per-agent token and the admission check's marker (gd-Utbyy4xJRX).
func TestEdgeParity_BlankedHeadersBothWays(t *testing.T) {
	door := landDoor(t)
	forwarding := 0
	for from := 0; ; {
		loc, next := block(door, `location ~ "^/land/`, from)
		if loc == "" {
			break
		}
		from = next
		if !strings.Contains(loc, "proxy_pass ") {
			continue
		}
		forwarding++
		head, _, _ := strings.Cut(loc, "\n")
		for _, h := range blankedHeaders {
			if !regexp.MustCompile(`(?m)^\s*proxy_set_header ` + regexp.QuoteMeta(h) + `\s+"";`).MatchString(loc) {
				t.Errorf("landgate drops %s; the edge's %s forwards it", h, head)
			}
		}
	}
	if forwarding < 2 {
		t.Fatalf("found %d forwarding locations in the land door, want the door and the feed", forwarding)
	}
	for _, h := range []string{"X-Trigger-Bead", "X-User-Bead-Id", "X-No-Owner-Default", "X-No-Auto-Parent", "X-No-Auto-Inherit", "X-Force-Parent-Edge", landDoorCheckHeader} {
		if !slices.Contains(blankedHeaders, h) {
			t.Errorf("landgate forwards %s", h)
		}
	}
}

// The edge marks its admission check as landgate does, so kbeads leaves both
// out of the rate limit and counts the caller's own requests.
func TestEdgeParity_AdmissionCheckMarker(t *testing.T) {
	b, err := os.ReadFile("../../../helm/gardenio-ziti/public-edge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	check, _ := block(string(b), "location = /oauth2/auth-land-token {", 0)
	if check == "" {
		t.Fatal("public-edge.yaml has no `location = /oauth2/auth-land-token`")
	}
	if !strings.Contains(check, "proxy_set_header "+landDoorCheckHeader+" 1;") {
		t.Errorf("/oauth2/auth-land-token does not set %s 1, as landgate's admitLandToken does", landDoorCheckHeader)
	}
}

// landgate sends a bearer to kbeads' land token check (admitLandToken)
// exactly when the edge sends it to /oauth2/auth-land-token: the same
// pattern, so a bearer cannot take the JWT path at one wall and the
// land-token path at the other (gd-WTyoLCSMUp).
func TestEdgeParity_LandTokenBearer(t *testing.T) {
	b, err := os.ReadFile("../../../helm/gardenio-ziti/public-edge.yaml")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`map \$http_authorization \$land_door_land_token \{\s*default 0;\s*"~([^"]+)" 1;\s*\}`).FindSubmatch(b)
	if m == nil {
		t.Fatal("public-edge.yaml: no `map $http_authorization $land_door_land_token` with one entry")
	}
	if string(m[1]) != landTokenBearer.String() {
		t.Errorf("the edge marks a land token with %s, landgate with %s", m[1], landTokenBearer)
	}
}

// The land name landgate accepts is the one the edge captures (gd-ParwLpmgID).
func TestEdgeParity_LandName(t *testing.T) {
	door := landDoor(t)
	if !strings.Contains(door, `"^/land/[a-z][a-z0-9-]{0,62}/v1/"`) {
		t.Fatal("the land door's land-name pattern moved; update landNamePattern with it")
	}
	if landNamePattern.String() != `^[a-z][a-z0-9-]{0,62}$` {
		t.Errorf("landNamePattern = %s", landNamePattern)
	}
}

// landgate's allowlist is kbeads': the same file, byte for byte, so the pod-
// side twin and the daemon open exactly the same routes (gd-nvvTcZeBZD).
func TestEdgeParity_Routes(t *testing.T) {
	theirs, err := os.ReadFile("../../../../kbeads/internal/server/land_door_routes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if string(theirs) != landDoorRoutesFile {
		t.Error("land_door_routes.txt differs from kbeads/internal/server/land_door_routes.txt; copy it over")
	}
}

func TestRoutes_Parse(t *testing.T) {
	if landDoorRoutesErr != nil {
		t.Fatal(landDoorRoutesErr)
	}
	if len(landDoorRoutes.patterns) < 40 {
		t.Fatalf("only %d routes parsed", len(landDoorRoutes.patterns))
	}
	for _, bad := range []string{
		"GET /v1/beads extra", "get /v1/beads", "GET /beads", "GET /v1/beads\nGET /v1/beads",
		"GET /v1/beads/{id}\nGET /v1/beads/{x}", "GET /v1/beads/{id",
	} {
		if _, err := parseRoutes(bad); err == nil {
			t.Errorf("parseRoutes(%q) accepted it", bad)
		}
	}
	var nilSet *routeSet
	if nilSet.allows(httptest.NewRequest(http.MethodGet, "/v1/beads", nil)) {
		t.Error("a routeSet that failed to parse allows a route")
	}
}
