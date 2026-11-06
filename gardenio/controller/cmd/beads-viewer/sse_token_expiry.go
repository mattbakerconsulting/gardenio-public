package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// A long-lived SSE page reads the daemon on the access token that was on the
// request when the stream opened. That token lasts minutes and is not refreshed
// mid-stream, so once it expires every re-render read is refused and the page
// goes quietly stale (gd-HMMeTk5yVz). The fix is to end the stream when the
// token does: the browser's EventSource reconnects through the proxy, which
// hands the new request a fresh token.

// bearerExpiry returns the exp of a compact JWT, decoded WITHOUT verifying it.
// That is safe here: the OIDC middleware already verified the token before the
// handler ran, and the result only decides when to hang up on our own stream.
// ok is false for an opaque token, one with no exp, or anything malformed.
func bearerExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

// ctxUntilBearerExpiry derives a context that is cancelled when the user bearer
// on ctx expires. With no bearer, or one carrying no exp (dev bypass, AVA edge,
// opaque token), the stream is not time-limited and the parent is returned with
// a plain cancel.
func ctxUntilBearerExpiry(ctx context.Context) (context.Context, context.CancelFunc) {
	if exp, ok := bearerExpiry(beadsapi.UserBearerFromContext(ctx)); ok {
		return context.WithDeadline(ctx, exp)
	}
	return context.WithCancel(ctx)
}

// sseRequestUntilTokenExpiry returns r rebound to a context that ends when the
// request's user token does, so every r.Context() read in the handler (the
// event stream, fragment re-renders) stops with it. Callers must defer cancel.
func sseRequestUntilTokenExpiry(r *http.Request) (*http.Request, context.CancelFunc) {
	ctx, cancel := ctxUntilBearerExpiry(r.Context())
	return r.WithContext(ctx), cancel
}
