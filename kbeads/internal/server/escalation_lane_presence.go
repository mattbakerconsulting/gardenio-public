package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"gardenio/kbeads/internal/model"
)

// Presence-based staffing for machine coordinator lanes (gd-t41kYFbLUt,
// owner decision gd-NPlx6B9fpF). A presence-tracked machine client staffs its
// lane only while it has made an authenticated request within the presence
// window, so coordinator:laptop is unstaffed while no laptop session runs.
// coordinator:cluster is not tracked: it is spawned on demand, so a quiet
// half hour is its normal state, not an absence.
//
// Last-seen is a config row (escalationLaneSeenKeyPrefix + client), so every
// daemon replica and a restarted one read the same value. It is written at
// most once per escalationLaneSeenWriteEvery per client, and read through a
// cache of escalationLaneSeenReadTTL.

const (
	escalationLaneSeenKeyPrefix     = "escalation_lane_seen:"
	escalationLaneSeenWriteEvery    = time.Minute
	escalationLaneSeenReadTTL       = 30 * time.Second
	defaultEscalationPresenceWindow = 30 * time.Minute
)

// laneSeenState is the per-replica write throttle and read cache.
type laneSeenState struct {
	mu      sync.Mutex
	written map[string]time.Time // client -> last write by this replica
	read    map[string]laneSeenRead
}

type laneSeenRead struct {
	seen time.Time // zero: never seen
	at   time.Time // when this replica read it
}

// escalationPresenceClients is BEADS_ESCALATION_PRESENCE_CLIENTS: the machine
// coordinator clients whose lanes are staffed by presence. Unset means
// gardenio-laptop-operator; "-" means none.
func escalationPresenceClients() []string {
	v, ok := os.LookupEnv("BEADS_ESCALATION_PRESENCE_CLIENTS")
	if !ok {
		return []string{"gardenio-laptop-operator"}
	}
	return parseCommaList(v)
}

// escalationPresenceWindow is BEADS_ESCALATION_LANE_PRESENCE_WINDOW: how
// recent a presence-tracked client's last request must be for its lane to be
// staffed. Default 30m; 0 turns presence off (the allowlist alone staffs the
// lane); an unparseable value keeps the default, with a WARN.
func escalationPresenceWindow() time.Duration {
	v := strings.TrimSpace(os.Getenv("BEADS_ESCALATION_LANE_PRESENCE_WINDOW"))
	if v == "" {
		return defaultEscalationPresenceWindow
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		slog.Warn("BEADS_ESCALATION_LANE_PRESENCE_WINDOW unparseable; using the default", "value", v, "default", defaultEscalationPresenceWindow, "error", err)
		return defaultEscalationPresenceWindow
	}
	return d
}

func parseCommaList(v string) []string {
	var out []string
	for _, c := range strings.Split(v, ",") {
		if c = strings.TrimSpace(c); c != "" && c != "-" {
			out = append(out, c)
		}
	}
	return out
}

// presenceTracked reports whether client's lane is staffed by presence.
func presenceTracked(client string) bool {
	if escalationPresenceWindow() <= 0 {
		return false
	}
	for _, c := range escalationPresenceClients() {
		if c == client {
			return true
		}
	}
	return false
}

// coordinatorPresenceMiddleware records an authenticated request from a
// presence-tracked, allowlisted machine coordinator client. A failed write
// never fails the request.
func (s *BeadsServer) coordinatorPresenceMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.recordCoordinatorSeen(r.Context(), time.Now().UTC())
		next.ServeHTTP(w, r)
	})
}

// recordCoordinatorSeen stamps the caller's last-seen when it is a
// presence-tracked coordinator client on its machine principal.
func (s *BeadsServer) recordCoordinatorSeen(ctx context.Context, now time.Time) {
	if authMethodFromContext(ctx) != authMethodMachine {
		return
	}
	client := AuthIdentityFromContext(ctx).ClientID
	if !s.coordinatorClientAllowed(client) || !presenceTracked(client) {
		return
	}
	st := &s.laneSeen
	st.mu.Lock()
	if st.written == nil {
		st.written, st.read = map[string]time.Time{}, map[string]laneSeenRead{}
	}
	if now.Sub(st.written[client]) < escalationLaneSeenWriteEvery {
		st.mu.Unlock()
		return
	}
	prev := st.written[client]
	st.written[client] = now
	st.read[client] = laneSeenRead{seen: now, at: now}
	st.mu.Unlock()
	val, _ := json.Marshal(map[string]string{"at": now.Format(time.RFC3339)})
	if err := s.store.SetConfig(ctx, &model.Config{Key: escalationLaneSeenKeyPrefix + client, Value: val}); err != nil {
		slog.Warn("escalation lane presence: failed to record last-seen", "client", client, "error", err)
		st.mu.Lock()
		st.written[client] = prev // retry on the next request
		st.mu.Unlock()
	}
}

// coordinatorLastSeen is client's last recorded request, zero if never.
func (s *BeadsServer) coordinatorLastSeen(ctx context.Context, client string) (time.Time, error) {
	now := time.Now()
	st := &s.laneSeen
	st.mu.Lock()
	if r, ok := st.read[client]; ok && now.Sub(r.at) < escalationLaneSeenReadTTL {
		st.mu.Unlock()
		return r.seen, nil
	}
	st.mu.Unlock()
	var seen time.Time
	cfg, err := s.store.GetConfig(ctx, escalationLaneSeenKeyPrefix+client)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && cfg == nil):
	case err != nil:
		return time.Time{}, err
	default:
		var v struct {
			At string `json:"at"`
		}
		if json.Unmarshal(cfg.Value, &v) == nil {
			seen, _ = time.Parse(time.RFC3339, v.At)
		}
	}
	st.mu.Lock()
	if st.read == nil {
		st.written, st.read = map[string]time.Time{}, map[string]laneSeenRead{}
	}
	st.read[client] = laneSeenRead{seen: seen, at: now}
	st.mu.Unlock()
	return seen, nil
}

// machinePresent reports whether an allowlisted machine client staffs its lane
// now: always for an untracked client, else seen within the window.
func (s *BeadsServer) machinePresent(ctx context.Context, client string) (bool, error) {
	if !presenceTracked(client) {
		return true, nil
	}
	seen, err := s.coordinatorLastSeen(ctx, client)
	if err != nil {
		return false, err
	}
	return !seen.IsZero() && time.Since(seen) < escalationPresenceWindow(), nil
}

// lanePresence is a presence-tracked lane's state, as the escalation JSON
// exposes it (held_by_presence).
type lanePresence struct {
	Lane       string `json:"lane"`
	Client     string `json:"client"`
	Online     bool   `json:"online"`
	LastSeenAt string `json:"last_seen_at,omitempty"`
	Window     string `json:"window"`
}

// lanePresenceFor returns ref's presence when it is a coordinator lane whose
// machine principal is presence-tracked and allowlisted, else nil. A lane
// with a role principal is staffed without presence, so it has none.
func (s *BeadsServer) lanePresenceFor(ctx context.Context, ref string, p escalationPolicy) *lanePresence {
	lane, err := parseLaneRef(strings.TrimSpace(ref))
	if err != nil || lane.Kind != laneCoordinator {
		return nil
	}
	var client string
	for _, pr := range p.Coordinators[lane.Name].Principals {
		kind, name, _ := strings.Cut(pr, ":")
		switch {
		case kind == "role":
			return nil
		case kind != "machine" || !s.coordinatorClientAllowed(name):
		case !presenceTracked(name):
			return nil
		case client == "":
			client = name
		}
	}
	if client == "" {
		return nil
	}
	seen, err := s.coordinatorLastSeen(ctx, client)
	if err != nil {
		slog.Warn("escalation lane presence: last-seen unreadable", "lane", lane.String(), "error", err)
	}
	lp := &lanePresence{Lane: lane.String(), Client: client, Window: escalationPresenceWindow().String()}
	if !seen.IsZero() {
		lp.LastSeenAt = seen.UTC().Format(time.RFC3339)
		lp.Online = time.Since(seen) < escalationPresenceWindow()
	}
	return lp
}

// noteOfflineHolder stamps held_by_offline_at (once per escalation) and
// held_by_last_seen_at into fields when fields["held_by"] is a presence lane
// that is offline, so the bridge tells the operator thread once that the lane
// holds it while away. It never moves held_by.
func (s *BeadsServer) noteOfflineHolder(ctx context.Context, esc *model.Bead, fields map[string]any, p escalationPolicy) {
	held, _ := fields["held_by"].(string)
	lp := s.lanePresenceFor(ctx, held, p)
	if lp == nil || lp.Online {
		return
	}
	if had, _, _ := exactFieldString(esc.Fields, heldByOfflineAtField); strings.TrimSpace(had) != "" {
		return
	}
	fields[heldByOfflineAtField] = time.Now().UTC().Format(time.RFC3339)
	fields["held_by_last_seen_at"] = lp.LastSeenAt
}

// heldByOfflineAtField is the bridge's once-per-escalation offline notice key.
const heldByOfflineAtField = "held_by_offline_at"

// escalationPresenceJSON adds held_by_presence to an escalation's JSON when
// its holder is a presence lane.
func (s *BeadsServer) escalationPresenceJSON(ctx context.Context, b *model.Bead, esc map[string]any, p escalationPolicy) {
	if lp := s.lanePresenceFor(ctx, escalationHeldBy(b), p); lp != nil {
		esc["held_by_presence"] = lp
	}
}
