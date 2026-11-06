package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"

	"gardenio/controller/internal/oidcauth"
)

// entraVerifier verifies a Gardenio-audience Microsoft Entra delegated access
// token and returns the identity that edge-auth stamps into its existing HMAC
// envelope. The interface keeps proxy and ForwardAuth tests independent of OIDC
// discovery/JWKS fixtures.
type entraVerifier interface {
	verify(ctx context.Context, r *http.Request) (entraIdentity, error)
	ready() bool
}

type entraIdentity struct {
	Email string
	Roles []string
}

var (
	errEntraMissing     = errors.New("entra: bearer token missing")
	errEntraInvalid     = errors.New("entra: token invalid")
	errEntraForbidden   = errors.New("entra: token forbidden")
	errEntraUnavailable = errors.New("entra: verifier unavailable")
)

type entraHandleVerifier struct {
	h *oidcauth.Handle
}

func newEntraHandleVerifier(cfg *config, logger *slog.Logger) (entraVerifier, error) {
	if cfg.entraIssuerURL == "" {
		return nil, nil
	}
	issuer, err := url.Parse(cfg.entraIssuerURL)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" {
		return nil, fmt.Errorf("EDGE_AUTH_ENTRA_ISSUER_URL must be an https URL")
	}
	switch {
	case cfg.entraAudience == "":
		return nil, errors.New("EDGE_AUTH_ENTRA_AUDIENCE is required when Entra auth is enabled")
	case cfg.entraTenantID == "":
		return nil, errors.New("EDGE_AUTH_ENTRA_TENANT_ID is required when Entra auth is enabled")
	case cfg.entraRequiredScope == "":
		return nil, errors.New("EDGE_AUTH_ENTRA_REQUIRED_SCOPE is required when Entra auth is enabled")
	case cfg.entraRequiredRole == "":
		return nil, errors.New("EDGE_AUTH_ENTRA_REQUIRED_ROLE is required when Entra auth is enabled")
	case len(cfg.entraAllowedClientIDs) == 0:
		return nil, errors.New("EDGE_AUTH_ENTRA_ALLOWED_CLIENT_IDS is required when Entra auth is enabled")
	}

	h := oidcauth.New(context.Background(), oidcauth.Config{
		IssuerURL:                cfg.entraIssuerURL,
		ClientID:                 cfg.entraAudience,
		ExpectedTenantID:         cfg.entraTenantID,
		RequiredRole:             cfg.entraRequiredRole,
		RequiredScopes:           []string{cfg.entraRequiredScope},
		AllowedAuthorizedParties: cfg.entraAllowedClientIDs,
		AllowedEmailDomains:      cfg.allowedEmailDomains,
	}, logger)
	return &entraHandleVerifier{h: h}, nil
}

func (v *entraHandleVerifier) ready() bool {
	return v != nil && v.h != nil && v.h.Ready()
}

func (v *entraHandleVerifier) verify(ctx context.Context, r *http.Request) (entraIdentity, error) {
	cred := authorizationCredentialFromRequest(r)
	if cred.scheme != "bearer" || cred.token == "" {
		return entraIdentity{}, errEntraMissing
	}

	var (
		id  entraIdentity
		hit bool
	)
	inner := http.HandlerFunc(func(_ http.ResponseWriter, ir *http.Request) {
		hit = true
		id.Email = oidcauth.EmailFromContext(ir.Context())
		id.Roles = oidcauth.RolesFromContext(ir.Context())
	})
	rec := &statusRecorder{ResponseWriter: discardResponseWriter{}, suppress: true}
	v.h.Middleware(inner).ServeHTTP(rec, r.WithContext(ctx))

	if hit {
		if id.Email == "" {
			return entraIdentity{}, errEntraInvalid
		}
		return id, nil
	}
	switch rec.status {
	case http.StatusUnauthorized:
		return entraIdentity{}, errEntraInvalid
	case http.StatusForbidden:
		return entraIdentity{}, errEntraForbidden
	case http.StatusServiceUnavailable:
		return entraIdentity{}, errEntraUnavailable
	default:
		return entraIdentity{}, errEntraUnavailable
	}
}
