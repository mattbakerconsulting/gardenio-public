package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// maxBodyBytes caps a webhook body. GitHub documents 25 MB as the largest
	// payload it will send; anything bigger is not from GitHub.
	maxBodyBytes = 25 << 20

	headerSignature = "X-Hub-Signature-256"
	headerDelivery  = "X-GitHub-Delivery"
	headerEvent     = "X-GitHub-Event"

	signaturePrefix = "sha256="
)

// Delivery outcomes, the label values of the deliveries counter.
const (
	outcomeAccepted      = "accepted"
	outcomeDuplicate     = "duplicate"
	outcomePing          = "ping"
	outcomeBadSignature  = "bad_signature"
	outcomeBadRequest    = "bad_request"
	outcomeNoSecret      = "no_secret"
	outcomeHandlerFailed = "handler_failed"
	outcomeCacheFull     = "replay_cache_full"
)

// Event is the minimal, already-authenticated view of a delivery handed to an
// EventSink. Payload is the verbatim body; sinks decode what they need.
type Event struct {
	DeliveryID     string
	Type           string // X-GitHub-Event
	Action         string // payload.action, may be empty
	Repo           string // payload.repository.full_name, empty for app-level events
	InstallationID int64
	Sender         string
	Payload        []byte
}

// EventSink consumes verified, de-duplicated events. Returning an error makes
// the receiver answer 5xx and forget the delivery id, so GitHub's redelivery of
// the same GUID is processed rather than swallowed as a replay. Sinks must be
// idempotent: the replay cache is in memory and does not survive a restart.
type EventSink interface {
	Handle(ctx context.Context, ev Event) error
}

// logSink is the sink until the bead updater lands: it records that an event
// arrived (type, action, repo only — never the payload) and does nothing else.
type logSink struct{ logger *slog.Logger }

func (s logSink) Handle(_ context.Context, ev Event) error {
	s.logger.Info("github event received",
		"delivery", ev.DeliveryID, "event", ev.Type, "action", ev.Action,
		"repo", ev.Repo, "installation", ev.InstallationID)
	return nil
}

// verifySignature checks an X-Hub-Signature-256 header value against the body.
// The comparison is constant-time over the decoded MACs; a missing prefix, bad
// hex, wrong length or empty secret all fail.
func verifySignature(secret, body []byte, header string) bool {
	if len(secret) == 0 || !strings.HasPrefix(header, signaturePrefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(header, signaturePrefix))
	if err != nil || len(got) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}

// replayCache remembers X-GitHub-Delivery ids for ttl. Ids are inserted only
// after the signature verified, so an unauthenticated caller cannot fill it.
// When it is full of unexpired ids it refuses new ones rather than evicting a
// live id, because eviction would reopen the replay window it exists to close.
type replayCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	now  func() time.Time
	seen map[string]time.Time // id -> expiry
}

func newReplayCache(ttl time.Duration, max int, now func() time.Time) *replayCache {
	if now == nil {
		now = time.Now
	}
	return &replayCache{ttl: ttl, max: max, now: now, seen: make(map[string]time.Time)}
}

type claimResult int

const (
	claimed claimResult = iota
	claimDuplicate
	claimFull
)

// claim records id as seen. It reports claimDuplicate when id is still within
// its TTL (including while another request is still processing it).
func (c *replayCache) claim(id string) claimResult {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if exp, ok := c.seen[id]; ok && now.Before(exp) {
		return claimDuplicate
	}
	if len(c.seen) >= c.max {
		for k, exp := range c.seen {
			if !now.Before(exp) {
				delete(c.seen, k)
			}
		}
		if len(c.seen) >= c.max {
			return claimFull
		}
	}
	c.seen[id] = now.Add(c.ttl)
	return claimed
}

// release forgets id so a redelivery after a failed attempt is processed.
func (c *replayCache) release(id string) {
	c.mu.Lock()
	delete(c.seen, id)
	c.mu.Unlock()
}

type webhookConfig struct {
	Secret []byte
	Sink   EventSink
	Replay *replayCache
	Logger *slog.Logger
	// Observe records one terminal outcome per request; nil is allowed.
	Observe func(outcome string)
}

// newWebhookHandler returns the POST /webhook handler. Order matters: method,
// secret configured, body read (size-capped), signature, headers, replay claim,
// dispatch. Nothing unauthenticated reaches the replay cache or the sink, and
// every rejection is a bare status with no detail for a prober.
func newWebhookHandler(cfg webhookConfig) http.Handler {
	observe := cfg.Observe
	if observe == nil {
		observe = func(string) {}
	}
	logger := cfg.Logger
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			observe(outcomeBadRequest)
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}
		// Fail closed: with no secret every request is unauthenticated.
		if len(cfg.Secret) == 0 {
			observe(outcomeNoSecret)
			logger.Error("webhook: no webhook secret configured, rejecting request")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			observe(outcomeBadRequest)
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "Payload Too Large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if !verifySignature(cfg.Secret, body, r.Header.Get(headerSignature)) {
			observe(outcomeBadSignature)
			logger.Warn("webhook: invalid signature")
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}

		deliveryID := r.Header.Get(headerDelivery)
		eventType := r.Header.Get(headerEvent)
		if deliveryID == "" || eventType == "" || len(deliveryID) > 128 {
			observe(outcomeBadRequest)
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			observe(outcomeBadRequest)
			http.Error(w, "Unsupported Media Type", http.StatusUnsupportedMediaType)
			return
		}

		switch cfg.Replay.claim(deliveryID) {
		case claimDuplicate:
			observe(outcomeDuplicate)
			logger.Info("webhook: duplicate delivery ignored", "delivery", deliveryID, "event", eventType)
			// 2xx: the original was (or is being) handled; a non-2xx would make
			// GitHub flag the delivery failed and retry it.
			w.WriteHeader(http.StatusOK)
			return
		case claimFull:
			observe(outcomeCacheFull)
			logger.Error("webhook: replay cache full, shedding load", "delivery", deliveryID)
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Service Unavailable", http.StatusServiceUnavailable)
			return
		}

		if eventType == "ping" {
			observe(outcomePing)
			w.WriteHeader(http.StatusOK)
			return
		}

		ev := Event{DeliveryID: deliveryID, Type: eventType, Payload: body}
		fillEnvelope(&ev)
		if err := cfg.Sink.Handle(r.Context(), ev); err != nil {
			cfg.Replay.release(deliveryID)
			observe(outcomeHandlerFailed)
			logger.Error("webhook: handler failed", "delivery", deliveryID, "event", eventType, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		observe(outcomeAccepted)
		w.WriteHeader(http.StatusAccepted)
	})
}

// fillEnvelope extracts the routing fields common to every GitHub event. A
// payload that does not decode still dispatches with empty fields: the
// signature already proved it came from GitHub, and the sink decides.
func fillEnvelope(ev *Event) {
	var env struct {
		Action     string `json:"action"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Sender struct {
			Login string `json:"login"`
		} `json:"sender"`
	}
	if json.Unmarshal(ev.Payload, &env) != nil {
		return
	}
	ev.Action = env.Action
	ev.Repo = env.Repository.FullName
	ev.InstallationID = env.Installation.ID
	ev.Sender = env.Sender.Login
}
