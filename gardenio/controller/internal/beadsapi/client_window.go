package beadsapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// The Window (Lands: the Window, plan gd-pz87vf3xsB, PW.3 gd-IZ9mjbOxD0): a
// capability URL /w/gw_<token> whose token kbeads opens exactly one route
// for, GET /v1/windows/self/view (kbeads/internal/server/window_principal.go).
// beads-viewer's /w/ page reads it on the visitor's window token and nothing
// else.

// windowViewPath is the route a window token opens.
const windowViewPath = "/v1/windows/self/view"

// windowLivePath is that route's live stream (PW.7 gd-nBkInNpy3r), the
// token's only other route.
const windowLivePath = "/v1/windows/self/live"

// windowViewsHeader tells kbeads how many page views one read stands for;
// the viewer's cache serves the rest. 0 is a read no visitor opened the page
// for (kbeads' windowViewsHeader).
const windowViewsHeader = "X-Window-Views"

// windowToken is a window bearer as a TokenSource, so WindowView can hand it
// to WithMachinePrincipal.
type windowToken string

func (t windowToken) Token(context.Context) (string, error) { return string(t), nil }

type windowViewsKeyType struct{}

// windowViewsFromContext returns the views WindowView put on ctx for sendOnce.
func windowViewsFromContext(ctx context.Context) (int64, bool) {
	n, ok := ctx.Value(windowViewsKeyType{}).(int64)
	return n, ok
}

// WindowView reads GET /v1/windows/self/view as the window whose token is
// given and returns the body as the daemon sent it (planwindow.Decode reads
// it). views is how many page views this read stands for.
//
// The token goes out as the per-call machine principal
// (WithMachinePrincipal), not as a user bearer: the user-bearer path is
// gated by SetRequireUserBearerForReads (off by default in the chart), while a
// machine principal is the wire credential whichever way the client's bearer
// settings point. The request runs on a context that keeps ctx's
// cancellation and none of its values, so no user bearer, actor or edge-auth
// identity the caller's ctx carries can go out beside the window token, and
// the client's own token never does. A refusal is an *APIError: 401 for an
// unknown, expired or revoked window.
func (c *Client) WindowView(ctx context.Context, token string, views int64) (json.RawMessage, error) {
	if token == "" {
		return nil, ErrMachineAuthUnavailable
	}
	wctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer context.AfterFunc(ctx, cancel)()
	wctx = WithMachinePrincipal(wctx, windowToken(token))
	wctx = context.WithValue(wctx, windowViewsKeyType{}, max(views, 0))
	var body json.RawMessage
	if err := c.doJSON(wctx, http.MethodGet, windowViewPath, nil, &body); err != nil {
		return nil, err
	}
	return body, nil
}

// setWindowViews stamps windowViewsHeader on a WindowView request.
func setWindowViews(ctx context.Context, req *http.Request) {
	if n, ok := windowViewsFromContext(ctx); ok {
		req.Header.Set(windowViewsHeader, strconv.FormatInt(n, 10))
	}
}

// WindowLive opens GET /v1/windows/self/live as the window whose token is
// given: an event stream whose "view" frames carry the same body WindowView
// returns, sent again each time it changes. The caller reads the body and
// closes it; it ends when ctx does, or when the daemon ends the stream.
//
// The request carries the window token as its only credential, set on the
// request itself: not the client's own token, a user bearer, an actor or an
// edge-auth envelope, and no value of ctx but its cancellation. It goes out
// on the client's stream transport (no overall timeout). A refusal is an
// *APIError: 401 for an unknown, expired or revoked window, 404 for a land
// window (which has no stream), 429 when the window has as many streams as
// it may, 410 when the window no longer streams (its plan gone or too large).
func (c *Client) WindowLive(ctx context.Context, token string) (io.ReadCloser, error) {
	if token == "" {
		return nil, ErrMachineAuthUnavailable
	}
	wctx, cancel := context.WithCancel(context.Background())
	stop := context.AfterFunc(ctx, cancel)
	req, err := http.NewRequestWithContext(wctx, http.MethodGet, c.baseURL+windowLivePath, nil)
	if err != nil {
		stop()
		cancel()
		return nil, fmt.Errorf("creating window stream request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.sseClient.Do(req)
	if err != nil {
		stop()
		cancel()
		return nil, fmt.Errorf("opening window stream: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		stop()
		cancel()
		return nil, &APIError{StatusCode: resp.StatusCode, Message: strings.TrimSpace(string(msg))}
	}
	return &windowStream{ReadCloser: resp.Body, stop: stop, cancel: cancel}, nil
}

// windowStream is a window stream's body: closing it also releases the
// request's context.
type windowStream struct {
	io.ReadCloser
	stop   func() bool
	cancel context.CancelFunc
}

func (s *windowStream) Close() error {
	err := s.ReadCloser.Close()
	s.stop()
	s.cancel()
	return err
}
