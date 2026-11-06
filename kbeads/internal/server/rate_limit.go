package server

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// RateLimitConfig controls per-identity token-bucket rate limits for HTTP
// mutation and read endpoints. Streaming paths and GET /v1/health are always
// exempt. There is no identity-based exemption: the primary static token that
// used to be exempt was retired (gd-ke6PC7bRto), so intra-cluster machine
// principals are limited like every other caller.
//
// Defaults come from DefaultRateLimitConfig; env overrides are parsed by
// RateLimitConfigFromEnv.
type RateLimitConfig struct {
	Enabled     bool
	MutateRPS   float64
	MutateBurst int
	ReadRPS     float64
	ReadBurst   int
	// MachineMultiplier scales both budgets for a machine principal's bucket
	// (key id:machine:<client>). Machine principals are the fleet backbone's
	// replacement for the rate-limit-exempt static token (gd-5QYwXtV7IA), and
	// a controller or trigger-engine sweep fans out per bead far past a user's
	// budget. They stay per-identity bounded on purpose (machine-gates table,
	// design §4.1): a misbehaving service account is limited, just higher.
	// 0 means defaultMachineRateMultiplier; 1 gives no boost.
	MachineMultiplier float64
	// IdleEviction is how long a per-identity limiter can sit unused before
	// it is evicted from the map. Keeps memory bounded under identity churn.
	IdleEviction time.Duration
}

// DefaultRateLimitConfig returns the defaults used when no env overrides are
// supplied. Tuned for a small-daemon deployment: mutations at ~20 rps
// sustained with burst 40 per identity; reads 5x that.
func DefaultRateLimitConfig() RateLimitConfig {
	return RateLimitConfig{
		Enabled:      true,
		MutateRPS:    20,
		MutateBurst:  40,
		ReadRPS:      100,
		ReadBurst:    200,
		IdleEviction: 10 * time.Minute,
	}
}

// machineRateLimitKeyPrefix is the bucket-key prefix rateLimitIdentityKey
// gives a machine principal; defaultMachineRateMultiplier is the budget scale
// such a bucket gets when MachineMultiplier is unset. On 2026-10-17 the
// trigger-engine, newly on its machine principal, took ~150 429s/h at 1x on
// per-bead dependency reads (orphan sweeps skipped, gardener
// finalize-iterations failed).
const (
	machineRateLimitKeyPrefix    = "id:machine:"
	defaultMachineRateMultiplier = 10
)

// rateLimitClass classifies a request for the rate limiter. Returns
// "mutate" for unsafe methods, "read" for safe methods, and "" for paths
// that should bypass the limiter entirely (health, SSE, streaming).
func rateLimitClass(method, path string) string {
	if isProbeHealthPath(method, path) {
		return ""
	}
	if isStreamingPath(path) {
		return ""
	}
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return "read"
	default:
		return "mutate"
	}
}

type limiterEntry struct {
	mutate   *rate.Limiter
	read     *rate.Limiter
	lastSeen time.Time
}

// rateLimiter is a per-identity token-bucket pool. Safe for concurrent use.
type rateLimiter struct {
	cfg     RateLimitConfig
	mu      sync.Mutex
	buckets map[string]*limiterEntry
	now     func() time.Time // overridable for tests
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	if cfg.IdleEviction <= 0 {
		cfg.IdleEviction = 10 * time.Minute
	}
	return &rateLimiter{
		cfg:     cfg,
		buckets: make(map[string]*limiterEntry),
		now:     time.Now,
	}
}

// bucket returns the limiter entry for the given identity key, creating
// it on first use. Stale entries are evicted lazily on fresh insertions
// so memory stays bounded without a dedicated reaper goroutine.
func (rl *rateLimiter) bucket(key string) *limiterEntry {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	entry, ok := rl.buckets[key]
	if !ok {
		mult := 1.0
		if strings.HasPrefix(key, machineRateLimitKeyPrefix) {
			mult = rl.cfg.MachineMultiplier
			if mult <= 0 {
				mult = defaultMachineRateMultiplier
			}
		}
		entry = &limiterEntry{
			mutate:   rate.NewLimiter(rate.Limit(rl.cfg.MutateRPS*mult), int(float64(rl.cfg.MutateBurst)*mult)),
			read:     rate.NewLimiter(rate.Limit(rl.cfg.ReadRPS*mult), int(float64(rl.cfg.ReadBurst)*mult)),
			lastSeen: now,
		}
		rl.buckets[key] = entry
		rl.evictLocked(now)
		return entry
	}
	entry.lastSeen = now
	return entry
}

func (rl *rateLimiter) evictLocked(now time.Time) {
	horizon := now.Add(-rl.cfg.IdleEviction)
	for k, v := range rl.buckets {
		if v.lastSeen.Before(horizon) {
			delete(rl.buckets, k)
		}
	}
}

// rateLimitIdentityKey returns the bucket key for a request. For
// authenticated requests we key on type+subject so two fleet tokens get
// independent buckets. For anonymous requests (rare — normally rejected
// by AuthMiddleware first) we fall back to client IP.
func rateLimitIdentityKey(r *http.Request, tp *TrustedProxyConfig) string {
	id := AuthIdentityFromContext(r.Context())
	if id.Subject != "" {
		return "id:" + id.Type + ":" + id.Subject
	}
	return "ip:" + clientIP(r, tp)
}

// isLandDoorAuthCheck reports whether r is the land door's admission check
// for an opaque land token (Door D.18a gd-WTyoLCSMUp): GET /v1/me naming a
// land door and carrying LandDoorCheckHeader. The edge's
// /oauth2/auth-land-token and landgate's admitLandToken each send one ahead
// of every request a land principal makes through the door, so counting them
// would spend two or three of the land's reads per call and cut its read
// budget to a third. The check is exempt: handleMe reads only the request
// context, AuthMiddleware has already refused an unknown token, and the
// request it admits is still counted. A caller's own GET /v1/me through the
// door is counted: the edge and landgate blank LandDoorCheckHeader on every
// request they forward (gd-Utbyy4xJRX).
func isLandDoorAuthCheck(r *http.Request) bool {
	return r.Method == http.MethodGet && r.URL.Path == "/v1/me" && len(r.Header.Values(LandDoorHeader)) > 0 &&
		r.Header.Get(LandDoorCheckHeader) == "1"
}

// RateLimitMiddleware enforces per-identity token-bucket limits on
// mutation and read endpoints. Streaming paths, /v1/health and the land
// door's admission check are exempt, as is the primary-static-token fleet
// backbone. On breach it responds with 429 and Retry-After: 1.
func RateLimitMiddleware(cfg RateLimitConfig, tp *TrustedProxyConfig, next http.Handler) http.Handler {
	if !cfg.Enabled || (cfg.MutateRPS <= 0 && cfg.ReadRPS <= 0) {
		return next
	}
	rl := newRateLimiter(cfg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		class := rateLimitClass(r.Method, r.URL.Path)
		if class == "" || isLandDoorAuthCheck(r) {
			next.ServeHTTP(w, r)
			return
		}
		key := rateLimitIdentityKey(r, tp)
		if key == "" {
			next.ServeHTTP(w, r)
			return
		}
		entry := rl.bucket(key)
		var lim *rate.Limiter
		if class == "mutate" {
			lim = entry.mutate
		} else {
			lim = entry.read
		}
		if lim == nil || lim.Allow() {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Retry-After", "1")
		slog.Warn("rate limit exceeded",
			"method", r.Method,
			"path", r.URL.Path,
			"class", class,
			"identity_key", key,
		)
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded")
	})
}
