package server

import (
	"context"
	"errors"
	"sync/atomic"
)

// errOIDCVerifierNotReady is returned by every OIDCVerifierHolder method
// while no underlying verifier is installed yet. It is never compared by
// callers — it just needs to make Verify/ExtractRoles/etc. fail the same way
// a nil TokenVerifier would have.
var errOIDCVerifierNotReady = errors.New("oidc verifier not yet initialized")

// OIDCVerifierHolder is a TokenVerifier whose underlying verifier can be
// swapped after construction. NewGRPCServer/NewHTTPHandler capture their
// verifier argument once, in the interceptor closures built at server-startup
// time — so promoting OIDC auth from "disabled" to "enabled" without a pod
// restart (the retry loop in cmd/kd/serve.go) requires the object handed to
// them to stay the same while what it delegates to changes underneath.
//
// It implements every optional verifier extension (IdentityVerifier,
// RoleExtractor, GroupExtractor) itself, delegating to whatever verifier is
// currently installed. Before the first successful init, or whenever the
// installed verifier doesn't support a given extension, each method fails
// closed with errOIDCVerifierNotReady/an unsupported-extension error — the
// auth interceptors already treat any verifier error as "try the next auth
// method, then reject", so this reproduces exactly the pre-retry behavior of
// a nil verifier until Set is first called with a working one.
type OIDCVerifierHolder struct {
	v atomic.Pointer[TokenVerifier]
}

// NewOIDCVerifierHolder returns a holder with no verifier installed.
func NewOIDCVerifierHolder() *OIDCVerifierHolder {
	return &OIDCVerifierHolder{}
}

// Set installs v as the active verifier. Passing nil clears it back to
// "not ready".
func (h *OIDCVerifierHolder) Set(v TokenVerifier) {
	if v == nil {
		h.v.Store(nil)
		return
	}
	h.v.Store(&v)
}

// Ready reports whether a verifier is currently installed.
func (h *OIDCVerifierHolder) Ready() bool {
	return h.current() != nil
}

func (h *OIDCVerifierHolder) current() TokenVerifier {
	p := h.v.Load()
	if p == nil {
		return nil
	}
	return *p
}

func (h *OIDCVerifierHolder) Verify(ctx context.Context, rawToken string) error {
	v := h.current()
	if v == nil {
		return errOIDCVerifierNotReady
	}
	return v.Verify(ctx, rawToken)
}

func (h *OIDCVerifierHolder) VerifyAndIdentify(ctx context.Context, rawToken string) (string, error) {
	v := h.current()
	if v == nil {
		return "", errOIDCVerifierNotReady
	}
	iv, ok := v.(IdentityVerifier)
	if !ok {
		return "", errors.New("installed oidc verifier does not support identity extraction")
	}
	return iv.VerifyAndIdentify(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractRoles(ctx context.Context, rawToken string) ([]string, error) {
	v := h.current()
	if v == nil {
		return nil, errOIDCVerifierNotReady
	}
	re, ok := v.(RoleExtractor)
	if !ok {
		return nil, errors.New("installed oidc verifier does not support role extraction")
	}
	return re.ExtractRoles(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractGroups(ctx context.Context, rawToken string) ([]string, error) {
	v := h.current()
	if v == nil {
		return nil, errOIDCVerifierNotReady
	}
	ge, ok := v.(GroupExtractor)
	if !ok {
		return nil, errors.New("installed oidc verifier does not support group extraction")
	}
	return ge.ExtractGroups(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractClientID(ctx context.Context, rawToken string) (string, error) {
	v := h.current()
	if v == nil {
		return "", errOIDCVerifierNotReady
	}
	cv, ok := v.(ClientExtractor)
	if !ok {
		return "", errors.New("installed oidc verifier does not support client extraction")
	}
	return cv.ExtractClientID(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractServiceAccount(ctx context.Context, rawToken string) (string, error) {
	v := h.current()
	if v == nil {
		return "", errOIDCVerifierNotReady
	}
	sv, ok := v.(ServiceAccountExtractor)
	if !ok {
		return "", errors.New("installed oidc verifier does not support service-account extraction")
	}
	return sv.ExtractServiceAccount(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractAudiences(ctx context.Context, rawToken string) ([]string, error) {
	v := h.current()
	if v == nil {
		return nil, errOIDCVerifierNotReady
	}
	av, ok := v.(AudienceExtractor)
	if !ok {
		return nil, errors.New("installed oidc verifier does not support audience extraction")
	}
	return av.ExtractAudiences(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractImpersonation(ctx context.Context, rawToken string) (string, string, error) {
	v := h.current()
	if v == nil {
		return "", "", errOIDCVerifierNotReady
	}
	iv, ok := v.(ImpersonationExtractor)
	if !ok {
		return "", "", errors.New("installed oidc verifier does not support impersonation extraction")
	}
	return iv.ExtractImpersonation(ctx, rawToken)
}

func (h *OIDCVerifierHolder) ExtractGitHub(ctx context.Context, rawToken string) (int64, string, error) {
	v := h.current()
	if v == nil {
		return 0, "", errOIDCVerifierNotReady
	}
	gv, ok := v.(GitHubExtractor)
	if !ok {
		return 0, "", errors.New("installed oidc verifier does not support github extraction")
	}
	return gv.ExtractGitHub(ctx, rawToken)
}
