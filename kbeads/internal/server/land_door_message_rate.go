package server

import (
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// The Commons message cap at the land door (Track B §5.4 step 10.4,
// gd-i74esUHCTg). beads-viewer holds each member of each land to
// COMMONS_MESSAGES_PER_MINUTE messages (commons/write.go rated). The land door
// reaches POST /v1/beads directly, past the viewer, where only the generic
// per-identity limit (rate_limit.go, 20 rps) applied. So a type=message
// create that came through a land's door takes one message from a bucket
// keyed by the land and the caller: landDoorMessagesPerMinuteEnv per minute,
// landDoorMessagesPerMinuteDefault when unset, 0 off. The Lands map's
// in-process creates carry the door in their context, so they are counted
// too. The buckets are in memory, like the viewer's: a restart refills them.

const (
	landDoorMessagesPerMinuteEnv     = "BEADS_LANDS_DOOR_MESSAGES_PER_MINUTE"
	landDoorMessagesPerMinuteDefault = 20
	// landDoorMessageIdle is how long an unused bucket is kept.
	landDoorMessageIdle = 10 * time.Minute
)

// landDoorMessagesPerMinute reads the cap; anything unparseable or negative
// is the default, so a typo never turns it off.
func landDoorMessagesPerMinute(getenv func(string) string) int {
	v := strings.TrimSpace(getenv(landDoorMessagesPerMinuteEnv))
	if v == "" {
		return landDoorMessagesPerMinuteDefault
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return landDoorMessagesPerMinuteDefault
	}
	return n
}

type landDoorMessageBucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

// landDoorMessageRate is the per-(land, caller) message cap. Safe for
// concurrent use; a nil or zero-cap rate admits everything.
type landDoorMessageRate struct {
	perMinute int
	now       func() time.Time

	mu      sync.Mutex
	buckets map[string]*landDoorMessageBucket
}

func newLandDoorMessageRate(perMinute int) *landDoorMessageRate {
	return &landDoorMessageRate{perMinute: perMinute, now: time.Now, buckets: map[string]*landDoorMessageBucket{}}
}

// allow takes one message from key's bucket: perMinute at once, then one
// every minute/perMinute.
func (l *landDoorMessageRate) allow(key string) bool {
	if l == nil || l.perMinute <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		for k, old := range l.buckets {
			if now.Sub(old.lastSeen) > landDoorMessageIdle {
				delete(l.buckets, k)
			}
		}
		b = &landDoorMessageBucket{lim: rate.NewLimiter(rate.Every(time.Minute/time.Duration(l.perMinute)), l.perMinute)}
		l.buckets[key] = b
	}
	b.lastSeen = now
	return b.lim.AllowN(now, 1)
}

// retryAfter is the seconds until a capped caller earns its next message.
func (l *landDoorMessageRate) retryAfter() int {
	return int(math.Ceil(60 / float64(l.perMinute)))
}

// landDoorMessageCapped answers 429 when r, a create of beadType, is a
// message through a land's door from a caller past its cap, and reports
// whether it did.
func (s *BeadsServer) landDoorMessageCapped(w http.ResponseWriter, r *http.Request, beadType string) bool {
	land, ok := landDoorFromContext(r.Context())
	if !ok || beadType != string(typeMessage) {
		return false
	}
	s.landDoorMessagesOnce.Do(func() {
		if s.landDoorMessages == nil {
			s.landDoorMessages = newLandDoorMessageRate(landDoorMessagesPerMinute(os.Getenv))
		}
	})
	id := AuthIdentityFromContext(r.Context())
	if s.landDoorMessages.allow(land + "\x00" + id.Type + ":" + id.Subject) {
		return false
	}
	slog.Warn("land door: message cap reached", "land", land, "identity", id.Type+":"+id.Subject)
	w.Header().Set("Retry-After", strconv.Itoa(s.landDoorMessages.retryAfter()))
	writeError(w, http.StatusTooManyRequests, "posting too fast: "+strconv.Itoa(s.landDoorMessages.perMinute)+
		" messages a minute at most in a land; wait a moment, then send it again")
	return true
}
