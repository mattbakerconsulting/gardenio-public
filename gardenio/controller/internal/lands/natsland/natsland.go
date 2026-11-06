// Package natsland gives each tenant land its own NATS identity
// (gd-sS7bSXOdGs). The server sends every connection that is not one of its
// configured fleet users (auth_callout.auth_users) to AuthSubject; Callout
// answers it. A land's pod connects as user land-<land> with the password
// landtoken.MintNATS mints at the land's epoch (EpochField), and gets a
// short-lived user that may publish only under EventPrefix(land) and
// subscribe to nothing. Bumping the epoch revokes that land's password alone
// (gd-zo4W7Q8Yno).
//
// EventPrefix is outside every fleet consumer's subscription (account
// exhaustion and terminal auth on *.*.<a>.<b>, the turn minter and the
// thinking feed on coop.events.messages.*.*), so a tenant cannot drive fleet
// account rotation or speak as a fleet actor. No fleet code consumes land
// events yet. The first consumer moves parseLandMessage (natsland_test.go)
// into this package and reads every event through it: it takes the land from
// the subject and refuses an actor that is not that land's (gd-zo4W7Q8Yno).
package natsland

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/jwt/v2"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"

	"gardenio/controller/internal/lands/landtoken"
)

const (
	// AuthSubject is where nats-server sends auth callout requests.
	AuthSubject = "$SYS.REQ.USER.AUTH"
	// queueGroup lets every controller replica serve the callout while the
	// server hears one answer.
	queueGroup = "gardenio-land-callout"
	// userPrefix prefixes the NATS user name a land's pod connects as.
	userPrefix = "land-"
	// globalAccount is the account a config-mode server keeps its users in.
	globalAccount = "$G"
	// DefaultTTL is how long an issued user lives. The server drops the
	// connection when it expires and the client reconnects through the
	// callout, so a drained land loses NATS within one TTL.
	DefaultTTL = 15 * time.Minute
	// EpochField is the project bead field holding a tenant land's NATS
	// epoch, a decimal integer ("" is 0). Bumping it revokes the land's
	// password: the callout refuses the old one at the next connect, a live
	// connection ends within TTL, and the land secret sync writes the new one
	// for the land's pods.
	EpochField = "nats_epoch"
	// refusalLogEvery bounds the Info lines refusals write; the rest go to
	// Debug, so log volume does not scale with a client looping bad connects.
	refusalLogEvery = time.Minute
)

// ParseEpoch reads an EpochField value. Anything but "" or a decimal
// integer is an error, so a mistyped epoch revokes rather than resets.
func ParseEpoch(s string) (uint64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, nil
	}
	epoch, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s %q is not a decimal integer", EpochField, s)
	}
	return epoch, nil
}

// EventPrefix is the COOP_NATS_PREFIX of land's pods and the only subject
// space its NATS user may publish to.
func EventPrefix(land string) string {
	return "coop.events.land." + land
}

// UserName is the NATS user land's pods connect as.
func UserName(land string) string {
	return userPrefix + land
}

// Callout answers auth callout requests for tenant lands.
type Callout struct {
	// Issuer is the account key the server's auth_callout.issuer names.
	Issuer nkeys.KeyPair
	// Key returns the land token key, read per request so a rotated key
	// applies to the next connect.
	Key func() ([]byte, error)
	// Land reports whether land is a tenant land now and, if so, its NATS
	// epoch. A drained land, or one whose epoch does not parse, is refused.
	Land func(land string) (epoch uint64, ok bool)
	// TTL is the issued user's lifetime; DefaultTTL when zero.
	TTL    time.Duration
	Logger *slog.Logger
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Workers bounds the requests answered at once; GOMAXPROCS when zero.
	Workers int

	mu         sync.Mutex
	lastInfo   time.Time
	suppressed int
}

// Start serves requests on conn (Subscribe) until ctx ends.
func (c *Callout) Start(ctx context.Context, conn *nats.Conn) error {
	sub, err := c.Subscribe(conn)
	if err != nil {
		return err
	}
	c.logger().Info("land nats callout: serving", "subject", AuthSubject)
	<-ctx.Done()
	return sub.Unsubscribe()
}

// Subscribe queue-subscribes conn to AuthSubject, answering up to Workers
// requests at once so a flood of bad connects does not serialise a land's
// connect behind it.
func (c *Callout) Subscribe(conn *nats.Conn) (*nats.Subscription, error) {
	workers := make(chan struct{}, c.workers())
	sub, err := conn.QueueSubscribe(AuthSubject, queueGroup, func(msg *nats.Msg) {
		workers <- struct{}{}
		go func() {
			defer func() { <-workers }()
			c.answer(msg)
		}()
	})
	if err != nil {
		return nil, fmt.Errorf("subscribing %s: %w", AuthSubject, err)
	}
	return sub, nil
}

func (c *Callout) answer(msg *nats.Msg) {
	resp, err := c.Respond(msg.Data)
	if err != nil {
		c.logger().Warn("land nats callout: cannot answer", "error", err)
		return
	}
	if err := msg.Respond([]byte(resp)); err != nil {
		c.logger().Warn("land nats callout: respond failed", "error", err)
	}
}

// Respond turns an encoded authorization request into the encoded response:
// a land user, or an error the server reports to the client as an
// authorization failure. It errs only when it cannot sign a response.
func (c *Callout) Respond(request []byte) (string, error) {
	req, err := jwt.DecodeAuthorizationRequestClaims(string(request))
	if err != nil {
		return "", fmt.Errorf("decoding request: %w", err)
	}
	resp := jwt.NewAuthorizationResponseClaims(req.UserNkey)
	resp.Audience = req.Server.ID
	land, err := c.authorize(req.ConnectOptions)
	if err != nil {
		c.logRefusal(req.ConnectOptions.Username, err)
		resp.Error = "not authorized"
	} else if resp.Jwt, err = c.user(req.UserNkey, land); err != nil {
		return "", err
	}
	return resp.Encode(c.Issuer)
}

var errUnknownLand = errors.New("not a tenant land")

// authorize returns the land opts authenticates as: user land-<land> with
// that land's password at the epoch Land reports.
func (c *Callout) authorize(opts jwt.ConnectOptions) (string, error) {
	land, ok := strings.CutPrefix(opts.Username, userPrefix)
	if !ok {
		return "", fmt.Errorf("user %q is not a land user", opts.Username)
	}
	if c.Land == nil {
		return "", errUnknownLand
	}
	epoch, ok := c.Land(land)
	if !ok {
		return "", errUnknownLand
	}
	key, err := c.Key()
	if err != nil {
		return "", fmt.Errorf("land token key: %w", err)
	}
	if err := landtoken.VerifyNATS(key, opts.Password, land, epoch); err != nil {
		return "", err
	}
	return land, nil
}

// user is the signed user JWT for land's connection as userNkey.
func (c *Callout) user(userNkey, land string) (string, error) {
	uc := jwt.NewUserClaims(userNkey)
	uc.Name = UserName(land)
	uc.Audience = globalAccount
	uc.Expires = c.now().Add(c.ttl()).Unix()
	uc.Pub.Allow.Add(EventPrefix(land) + ".>")
	uc.Sub.Deny.Add(">")
	// No uc.Limits: nats-server (2.14) applies only a callout user's
	// permissions, connection types and expiry, so a payload or subs cap
	// here would not hold; the server-wide max_payload bounds a message.
	return uc.Encode(c.Issuer)
}

// logRefusal logs a refused connect at Info at most once per
// refusalLogEvery, counting the refusals logged at Debug in between.
func (c *Callout) logRefusal(user string, reason error) {
	now := c.now()
	c.mu.Lock()
	info := c.lastInfo.IsZero() || now.Sub(c.lastInfo) >= refusalLogEvery
	suppressed := c.suppressed
	if info {
		c.lastInfo, c.suppressed = now, 0
	} else {
		c.suppressed++
	}
	c.mu.Unlock()
	if info {
		c.logger().Info("land nats callout: refused", "user", user, "reason", reason, "suppressed", suppressed)
		return
	}
	c.logger().Debug("land nats callout: refused", "user", user, "reason", reason)
}

func (c *Callout) workers() int {
	if c.Workers > 0 {
		return c.Workers
	}
	return runtime.GOMAXPROCS(0)
}

func (c *Callout) ttl() time.Duration {
	if c.TTL > 0 {
		return c.TTL
	}
	return DefaultTTL
}

func (c *Callout) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Callout) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

// LoadIssuer reads an account seed (nk -gen account) from path.
func LoadIssuer(path string) (nkeys.KeyPair, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	kp, err := nkeys.FromSeed([]byte(strings.TrimSpace(string(raw))))
	if err != nil {
		return nil, fmt.Errorf("issuer seed %s: %w", path, err)
	}
	if pub, err := kp.PublicKey(); err != nil || !nkeys.IsValidPublicAccountKey(pub) {
		return nil, fmt.Errorf("issuer seed %s is not an account seed", path)
	}
	return kp, nil
}
