package beadsapi

import (
	"context"
	"fmt"
	"strings"
)

// Per-call machine principal (gd-Hc34EENbO9, decision gd-ZsfxvXRiyv "tight").
//
// A component that forwards the signed-in human's own bearer for every
// human-driven write (SetRequireUserBearerForWrites) still has a few writes no
// human triggers — the beads-viewer's plan-label and plan-step reactors run off
// the daemon SSE stream. Those must not fall back to the shared static token
// (that is the identity the machine principals retire) and must not fail
// closed either. WithMachinePrincipal attaches the component's own Keycloak
// service-account TokenSource to the context of exactly those calls, so the
// daemon authenticates them as machine:<clientId> and grants only what the
// client's gardenio-machine-* roles allow. It is a per-call credential, like
// WithUserBearer, so one shared Client can serve human-attributed and
// machine-attributed writes without a second connection pool.
//
// Precedence on the wire (Client.wireCredential):
//  0. the WithUserBearerOnly bearer, for every method; an empty one fails the
//     call closed;
//  1. a present user bearer on a mutating call under write-bearer enforcement
//     (the human's identity is the top priority: a ctx that carries both uses
//     the human's token);
//  2. a present user bearer on a read under read-bearer enforcement
//     (SetRequireUserBearerForReads), same reasoning as 1;
//  3. the machine principal on ctx, for every method — reads on a reactor's
//     ctx go out as the machine too, so an audit sees one consistent actor;
//  4. the client's system principal (SetSystemPrincipal) on a read marked
//     WithSystemRead — the viewer's startup caches and feed hub, which have no
//     human behind them;
//  5. otherwise the client's own credential, with the write-bearer gate
//     failing a bearer-less write closed when enforcement is on, and the
//     read-bearer gate failing a bearer-less read closed unless it carries
//     the WithSystemRead marker.
//
// A machine token fetch failure is returned as an error (auth_unavailable in
// the text); the request is not sent and the static token is never substituted.

type machinePrincipalKeyType struct{}

// WithMachinePrincipal returns ctx carrying ts as the per-call machine
// credential. A nil ts returns ctx unchanged.
func WithMachinePrincipal(ctx context.Context, ts TokenSource) context.Context {
	if ts == nil {
		return ctx
	}
	return context.WithValue(ctx, machinePrincipalKeyType{}, ts)
}

// MachinePrincipalFromContext returns the TokenSource attached via
// WithMachinePrincipal, or nil.
func MachinePrincipalFromContext(ctx context.Context) TokenSource {
	v, _ := ctx.Value(machinePrincipalKeyType{}).(TokenSource)
	return v
}

// wireCredential picks the Authorization bearer for one request: "" means
// "use the client's own token". See the package comment above for the order.
func (c *Client) wireCredential(ctx context.Context, method, path string) (string, error) {
	if b, only := userBearerOnlyFromContext(ctx); only {
		if b == "" {
			return "", ErrUserBearerRequired
		}
		return b, nil
	}
	if (c.requireUserBearerForWrites || c.preferUserBearerForWrites) && isMutatingMethod(method) && !isReadOnlyPath(path) {
		if b := UserBearerFromContext(ctx); b != "" {
			return b, nil
		}
	}
	if c.requireUserBearerForReads && (!isMutatingMethod(method) || isReadOnlyPath(path)) {
		if b := UserBearerFromContext(ctx); b != "" {
			return b, nil
		}
	}
	if ts := MachinePrincipalFromContext(ctx); ts != nil {
		tok, err := ts.Token(ctx)
		if err != nil {
			return "", fmt.Errorf("machine principal credential for %s %s: %w", method, path, err)
		}
		if tok == "" {
			return "", fmt.Errorf("machine principal credential for %s %s: %w (empty token)", method, path, ErrMachineAuthUnavailable)
		}
		return tok, nil
	}
	if c.systemPrincipal != nil && IsSystemRead(ctx) && (!isMutatingMethod(method) || isReadOnlyPath(path)) {
		return c.systemPrincipalToken(ctx, method, path)
	}
	if _, err := c.applyUserBearerReadGate(ctx, method, path); err != nil {
		return "", err
	}
	return c.applyUserBearerWriteGate(ctx, method, path)
}

// SetSystemPrincipal installs the credential the client uses for traffic with
// no human behind it: reads marked WithSystemRead and every EventStream. It
// exists so a service that holds a machine principal can retire its static
// daemon token entirely (gd-glMVEH8phE). A fetch failure fails the call closed
// (ErrMachineAuthUnavailable); the static token is never substituted. nil
// restores the static-token behaviour.
func (c *Client) SetSystemPrincipal(ts TokenSource) {
	c.systemPrincipal = ts
}

// systemPrincipalToken returns the bearer from the client's system principal.
func (c *Client) systemPrincipalToken(ctx context.Context, what ...string) (string, error) {
	tok, err := c.systemPrincipal.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("system principal credential for %s: %w", strings.Join(what, " "), err)
	}
	if tok == "" {
		return "", fmt.Errorf("system principal credential for %s: %w (empty token)", strings.Join(what, " "), ErrMachineAuthUnavailable)
	}
	return tok, nil
}
