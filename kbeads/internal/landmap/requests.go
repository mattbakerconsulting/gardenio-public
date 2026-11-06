package landmap

import (
	"context"
	"net/http"
	"reflect"
	"runtime"
	"strings"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
)

// Requests counts every response on a Lands API path, by land, op, code and
// caller (gd-AQQaiE8IMd; the v1-beta exit's criterion 2, a land's weekly use).
// Guard counts it, so it sees the answers the map never writes (auth's 401,
// the limiter's 429, the door's 403, the mux's 404 and 405) as well as the
// map's own.
//
//   - land is the land the map admitted the caller to, and "" otherwise: the
//     path's land is the caller's to choose, so an unadmitted or unknown one
//     would mint a series per guess.
//   - op is the contract operationId of the route that answered, and "" for a
//     request that never reached one.
//   - code is the answer's problem code, or "ok".
//   - caller is the caller's class (Caller), and "" for a request that never
//     reached a route or was refused as an applicant.
var Requests = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "gardenio_lands_api_requests_total",
	Help: "Responses on Lands API paths, by admitted land, contract op, problem code (or ok) and caller class.",
}, []string{"land", "op", "code", "caller"})

// codeOK is Requests' code for an answer that is not a problem.
const codeOK = "ok"

// Caller is the class of a Lands API caller.
type Caller string

const (
	// CallerMember is a person of a land: a land-scoped OIDC sign-in.
	CallerMember Caller = "member"
	// CallerAgent is a land's agent, holding the land's own token (a land
	// principal, D.18a).
	CallerAgent Caller = "agent"
	// CallerOperator is everyone else: the fleet's operators and its service
	// tokens. Their reads are not a land's use.
	CallerOperator Caller = "operator"
)

// tally is what one request's Requests labels are, filled in as the request
// goes: Guard puts it on the request's context, the route sets op and caller
// (registrarMux) and admit sets land. Guard counts it once the response is
// done. A request is served on one goroutine, so it needs no lock.
type tally struct {
	op, land string
	caller   Caller
}

type tallyKey struct{}

func tallyOf(ctx context.Context) *tally {
	t, _ := ctx.Value(tallyKey{}).(*tally)
	return t
}

func (t *tally) admitted(land string) {
	if t != nil {
		t.land = land
	}
}

func (t *tally) opLand() (op, land string) {
	if t == nil {
		return "", ""
	}
	return t.op, t.land
}

// tallyWriter carries a request's tally to writeBody and the feed, which are
// handed only the response writer.
type tallyWriter struct {
	http.ResponseWriter
	t *tally
}

// Unwrap lets http.ResponseController reach the connection (the feed
// flushes through it).
func (w *tallyWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func tallyFromWriter(w http.ResponseWriter) *tally {
	if tw, ok := w.(*tallyWriter); ok {
		return tw.t
	}
	return nil
}

// operationID is the contract operationId a generated route handler serves:
// the generated wrapper names each handler after its ServerInterface method
// ("...(*ServerInterfaceWrapper).GetLand-fm"), and the operationId is that
// name with a lower-case first letter, as TestVerbTable_CoversContract reads
// it. TestOperationID_EveryRouteNamesAVerb holds every registered route to a
// row of Verbs.
func operationID(handler func(http.ResponseWriter, *http.Request)) string {
	name := runtime.FuncForPC(reflect.ValueOf(handler).Pointer()).Name()
	name = strings.TrimSuffix(name, "-fm")
	n := []rune(name[strings.LastIndex(name, ".")+1:])
	if len(n) > 0 {
		n[0] = unicode.ToLower(n[0])
	}
	return string(n)
}
