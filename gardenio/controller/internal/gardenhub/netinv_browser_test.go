package gardenhub

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/commons/commonstest"
)

// TestNetworkInventory is the network inventory of every /garden surface (P0.4 of the Lands UX performance pass,
// gd-hsekIVjOWI): what each surface fetches and streams while idle, while the farm is busy, and when its streams drop.
// Not a check: it runs only when GARDENHUB_NETINV_OUT names a directory, writes netinv.json and netinv.md there, and
// fails only when the harness itself breaks. `make garden-netinv OUT=<dir>` runs it.
//
// The page is cardWorld's (the goldens' fixture farm) with the Commons live, on a real clock. testdata/netinv_browser.js
// opens each surface in Chromium, then drives the phases through the routes below: idle (nothing moves), active (the farm
// ticks every NETINV_TICK_MS: a gardener's tool call and a farm change on every tick, a comment on plan A every second
// one, a #general message every third, and the World pans), blip (every connection cut once, the server up) and outage
// (every connection cut and every request answered 502 for NETINV_OUTAGE_S). The browser records each request and SSE
// message over CDP; this side counts the bytes it wrote per surface and phase, so a long stream's bytes fall in the
// phase they were sent in. Knobs: NETINV_IDLE_S and NETINV_ACTIVE_S (default 300), NETINV_TICK_MS (5000),
// NETINV_BLIP_S (60), NETINV_OUTAGE_S (30), NETINV_SURFACES (a comma list), NETINV_PARALLEL (pages at once, 3).
func TestNetworkInventory(t *testing.T) {
	out := os.Getenv("GARDENHUB_NETINV_OUT")
	if out == "" {
		t.Skip("GARDENHUB_NETINV_OUT not set (make garden-netinv OUT=<dir>)")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("node not installed")
	}
	mods := os.Getenv("GARDENHUB_PW_MODULES")
	if mods == "" {
		mods, _ = filepath.Abs(filepath.Join("..", "..", "..", "tests", "gardenhub-browser", "node_modules"))
	}
	if _, err := os.Stat(filepath.Join(mods, "playwright-core")); err != nil {
		t.Fatalf("playwright-core not installed under %s (make gardenhub-browser-deps)", mods)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}

	inner, rig := cardWorldRig(t, time.Now, "", nil, nil, true)
	inner.Close() // the rig's mux is served below, behind the meter
	nv := newNetMeter(rig)
	srv := nv.serve(t)

	cfg := map[string]any{
		"idleS": envInt("NETINV_IDLE_S", 300), "activeS": envInt("NETINV_ACTIVE_S", 300), "tickMs": envInt("NETINV_TICK_MS", 5000),
		"blipS": envInt("NETINV_BLIP_S", 60), "outageS": envInt("NETINV_OUTAGE_S", 30), "parallel": envInt("NETINV_PARALLEL", 3),
		"surfaces": os.Getenv("NETINV_SURFACES"),
	}
	cj, _ := json.Marshal(cfg)
	script, _ := filepath.Abs(filepath.Join("testdata", "netinv_browser.js"))
	abs, _ := filepath.Abs(out)
	cmd := exec.Command(node, script, srv.URL, mods, abs, string(cj))
	cmd.Env = os.Environ()
	cmd.Stderr = os.Stderr
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(pipe)
	for sc.Scan() {
		t.Log(sc.Text()) // progress, live: a full run is the better part of an hour
	}
	if err := cmd.Wait(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
			t.Fatal("no Chromium for playwright-core")
		}
		t.Fatalf("network inventory failed: %v", err)
	}
	t.Logf("wrote %s and %s", filepath.Join(abs, "netinv.json"), filepath.Join(abs, "netinv.md"))
}

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v >= 0 {
		return v
	}
	return def
}

// netMeter serves the rig's mux and counts what it writes, by surface (the X-Netinv-Surface header every request of a
// page carries), phase and route; and it serves the routes the browser script drives the phases with.
type netMeter struct {
	rig  *cardRig
	srv  *httptest.Server
	mu   sync.Mutex
	rows map[netKey]*netRow

	phase  atomic.Value // string
	down   atomic.Int64 // unix ms the outage ends; 0: up
	ticks  atomic.Int64
	actors []string // the gardeners whose tool calls the active phase makes
}

type netKey struct{ Surface, Phase, Method, Route string }

type netRow struct {
	Requests int   `json:"requests"`
	Bytes    int64 `json:"bytes"` // body bytes written, after the page's own gzip
	Status   map[int]int
}

func newNetMeter(rig *cardRig) *netMeter {
	m := &netMeter{rig: rig, rows: map[netKey]*netRow{}}
	m.phase.Store("load")
	for _, b := range rig.src.beads {
		if b.Type == "agent" && b.Status == "in_progress" && b.Fields["task_id"] != "" {
			m.actors = append(m.actors, b.ID)
		}
	}
	slices.Sort(m.actors)
	return m
}

// routeIDs folds the ids out of a path, so a bead's stream and another's count as one route.
var routeIDs = regexp.MustCompile(`/(gd|lb|ka)-[A-Za-z0-9]+`)

func route(r *http.Request) string {
	p := routeIDs.ReplaceAllString(r.URL.Path, "/{id}")
	if rest, ok := strings.CutPrefix(p, "/garden/c/"); ok {
		parts := strings.Split(rest, "/")
		if len(parts) >= 2 && !slices.Contains([]string{"events", "unread", "bead", "m"}, parts[0]) {
			parts[0], parts[1] = "{land}", "{channel}"
		}
		p = "/garden/c/" + strings.Join(parts, "/")
	}
	return p
}

func (m *netMeter) serve(t *testing.T) *httptest.Server {
	mux := m.rig.mux
	mux.HandleFunc("POST /test/netinv/phase", func(w http.ResponseWriter, r *http.Request) {
		m.phase.Store(r.URL.Query().Get("name"))
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /test/netinv/tick", func(w http.ResponseWriter, r *http.Request) {
		m.tick()
		w.WriteHeader(http.StatusNoContent)
	})
	// blip: every connection cut once, as a proxy restart or a phone changing networks does; the server stays up
	mux.HandleFunc("POST /test/netinv/blip", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
		go func() { time.Sleep(50 * time.Millisecond); m.srv.CloseClientConnections() }()
	})
	// outage: every connection cut, then every request but these routes answered 502 for ?s= seconds, as the edge does
	// while the viewer rolls
	mux.HandleFunc("POST /test/netinv/outage", func(w http.ResponseWriter, r *http.Request) {
		s, _ := strconv.Atoi(r.URL.Query().Get("s"))
		m.down.Store(time.Now().Add(time.Duration(s) * time.Second).UnixMilli())
		w.WriteHeader(http.StatusNoContent)
		go func() { time.Sleep(50 * time.Millisecond); m.srv.CloseClientConnections() }()
	})
	mux.HandleFunc("GET /test/netinv/bytes", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		type row struct {
			netKey
			netRow
		}
		var rows []row
		for k, v := range m.rows {
			rows = append(rows, row{k, *v})
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"rows": rows, "actors": m.actors, "ticks": m.ticks.Load()})
	})
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/test/netinv/") {
			mux.ServeHTTP(w, r)
			return
		}
		surface := r.Header.Get("X-Netinv-Surface")
		if surface == "" {
			surface = "?" // a request the page made without the header: counted, so the gap shows
		}
		k := netKey{Surface: surface, Phase: m.phase.Load().(string), Method: r.Method, Route: route(r)}
		if until := m.down.Load(); until > 0 && time.Now().UnixMilli() < until {
			m.count(k, 0, http.StatusBadGateway, true)
			http.Error(w, "502 Bad Gateway", http.StatusBadGateway)
			return
		}
		cw := &countWriter{ResponseWriter: w, m: m, surface: surface, method: r.Method, route: k.Route}
		m.count(k, 0, 0, true)
		mux.ServeHTTP(cw, r)
		m.count(netKey{surface, m.phase.Load().(string), r.Method, k.Route}, 0, cw.status(), false)
	}))
	t.Cleanup(m.srv.Close)
	return m.srv
}

// count adds a request (req) or a response status to k's row, and n body bytes.
func (m *netMeter) count(k netKey, n int64, status int, req bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := m.rows[k]
	if r == nil {
		r = &netRow{Status: map[int]int{}}
		m.rows[k] = r
	}
	if req {
		r.Requests++
	}
	if status != 0 {
		r.Status[status]++
	}
	r.Bytes += n
}

// tick is one beat of the active phase (see TestNetworkInventory).
func (m *netMeter) tick() {
	n := m.ticks.Add(1)
	now := time.Now()
	if len(m.actors) > 0 {
		a := m.actors[int(n)%len(m.actors)]
		use := "toolu_netinv_" + strconv.FormatInt(n, 10)
		for _, ev := range []string{HookPreToolUse, HookPostToolUse} {
			b, _ := json.Marshal(map[string]any{"event": ev, "ts": now.UTC().Format(time.RFC3339Nano), "agent_bead_id": a, "session_id": "netinv",
				"tool_name": "Read", "tool_use_id": use, "duration_ms": 120})
			m.rig.farm.ObserveHook(b)
		}
		m.rig.src.mutate(a, func(b *beadsapi.BeadDetail) {
			b.Fields["last_progress_at"] = now.UTC().Format(time.RFC3339)
			b.Fields["agent_state"] = map[bool]string{true: "working", false: "thinking"}[n%2 == 0]
		})
		m.rig.farm.Invalidate()
	}
	if n%2 == 0 {
		fb := m.rig.beads
		fb.mu.Lock()
		fb.comments["gd-planA"] = append(fb.comments["gd-planA"], beadsapi.Comment{Author: "flo", Text: "Netinv comment " + strconv.FormatInt(n, 10) + ".", CreatedAt: now})
		fb.mu.Unlock()
		select {
		case m.rig.hub.events <- ev("beads.comment.added", `{"comment":{"bead_id":"gd-planA"}}`):
		default:
		}
	}
	if n%3 == 0 && m.rig.commonsEvents != nil {
		d := m.rig.commons
		hello, _ := d.GetBead(beadsapi.WithUserBearer(context.Background(), commonsViewer), "gd-msgHello")
		b := &beadsapi.BeadDetail{ID: "gd-msgNetinv" + strconv.FormatInt(n, 10), Type: "message", Status: "closed",
			Title: "Netinv message " + strconv.FormatInt(n, 10), CreatedAt: now, UpdatedAt: now,
			Fields: map[string]string{"channel_id": "gd-chGeneral", "author": "oidc:flo@example.com", "text": "Netinv message " + strconv.FormatInt(n, 10) + "."}}
		if hello != nil {
			b.Labels = hello.Labels
		}
		d.Put(b)
		d.Log("beads.bead.created", b, now)
		select {
		case m.rig.commonsEvents <- beadsapi.SSEEvent{Event: "beads.bead.created", Data: commonstest.Payload(b)}:
		default:
		}
	}
}

// countWriter counts the body bytes a handler writes, into the phase current at each write, and passes Flush and
// Hijack through so a stream still streams.
type countWriter struct {
	http.ResponseWriter
	m                      *netMeter
	surface, method, route string
	code                   int
}

func (c *countWriter) WriteHeader(code int) {
	if c.code == 0 {
		c.code = code
	}
	c.ResponseWriter.WriteHeader(code)
}

func (c *countWriter) Write(p []byte) (int, error) {
	if c.code == 0 {
		c.code = http.StatusOK
	}
	n, err := c.ResponseWriter.Write(p)
	c.m.count(netKey{c.surface, c.m.phase.Load().(string), c.method, c.route}, int64(n), 0, false)
	return n, err
}

func (c *countWriter) status() int {
	if c.code == 0 {
		return http.StatusOK
	}
	return c.code
}

func (c *countWriter) Flush() {
	if f, ok := c.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (c *countWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return http.NewResponseController(c.ResponseWriter).Hijack()
}

func (c *countWriter) Unwrap() http.ResponseWriter { return c.ResponseWriter }
