package server

import (
	"context"
	"errors"
	"testing"
)

// fakeTokenVerifier is a minimal TokenVerifier that never implements any of
// the optional extension interfaces (IdentityVerifier/RoleExtractor/
// GroupExtractor), used to exercise OIDCVerifierHolder's unsupported-
// extension fallback.
type fakeTokenVerifier struct{ err error }

func (f *fakeTokenVerifier) Verify(ctx context.Context, rawToken string) error { return f.err }

// fakeFullVerifier implements every optional extension so the holder's
// delegation can be exercised end to end.
type fakeFullVerifier struct {
	err    error
	email  string
	roles  []string
	groups []string
}

func (f *fakeFullVerifier) Verify(ctx context.Context, rawToken string) error { return f.err }
func (f *fakeFullVerifier) VerifyAndIdentify(ctx context.Context, rawToken string) (string, error) {
	return f.email, f.err
}
func (f *fakeFullVerifier) ExtractRoles(ctx context.Context, rawToken string) ([]string, error) {
	return f.roles, f.err
}
func (f *fakeFullVerifier) ExtractGroups(ctx context.Context, rawToken string) ([]string, error) {
	return f.groups, f.err
}

// TestOIDCVerifierHolderNotReady covers the pre-init and cleared states: a
// freshly constructed holder, or one explicitly cleared with Set(nil), must
// fail every method exactly the way a nil verifier reference used to (the
// pre-retry behavior serve.go's callers were already built against).
func TestOIDCVerifierHolderNotReady(t *testing.T) {
	h := NewOIDCVerifierHolder()
	if h.Ready() {
		t.Fatal("Ready() = true for a freshly constructed holder")
	}
	if err := h.Verify(context.Background(), "tok"); err == nil {
		t.Error("Verify() = nil error, want failure while not ready")
	}
	if _, err := h.VerifyAndIdentify(context.Background(), "tok"); err == nil {
		t.Error("VerifyAndIdentify() = nil error, want failure while not ready")
	}
	if _, err := h.ExtractRoles(context.Background(), "tok"); err == nil {
		t.Error("ExtractRoles() = nil error, want failure while not ready")
	}
	if _, err := h.ExtractGroups(context.Background(), "tok"); err == nil {
		t.Error("ExtractGroups() = nil error, want failure while not ready")
	}

	h.Set(&fakeFullVerifier{})
	if !h.Ready() {
		t.Fatal("Ready() = false after Set with a non-nil verifier")
	}
	h.Set(nil)
	if h.Ready() {
		t.Fatal("Ready() = true after Set(nil)")
	}
	if err := h.Verify(context.Background(), "tok"); err == nil {
		t.Error("Verify() = nil error, want failure after Set(nil)")
	}
}

// TestOIDCVerifierHolderDelegates verifies a holder with an installed
// verifier forwards every method's result — the retry loop's whole purpose
// is that NewGRPCServer/NewHTTPHandler keep the same holder object while
// what it delegates to is swapped from oidcVerifierRetryLoop.
func TestOIDCVerifierHolderDelegates(t *testing.T) {
	h := NewOIDCVerifierHolder()
	inner := &fakeFullVerifier{email: "user@example.com", roles: []string{"admin"}, groups: []string{"g1"}}
	h.Set(inner)

	if err := h.Verify(context.Background(), "tok"); err != nil {
		t.Errorf("Verify() = %v, want nil", err)
	}
	if email, err := h.VerifyAndIdentify(context.Background(), "tok"); err != nil || email != "user@example.com" {
		t.Errorf("VerifyAndIdentify() = (%q, %v), want (%q, nil)", email, err, "user@example.com")
	}
	if roles, err := h.ExtractRoles(context.Background(), "tok"); err != nil || len(roles) != 1 || roles[0] != "admin" {
		t.Errorf("ExtractRoles() = (%v, %v), want ([admin], nil)", roles, err)
	}
	if groups, err := h.ExtractGroups(context.Background(), "tok"); err != nil || len(groups) != 1 || groups[0] != "g1" {
		t.Errorf("ExtractGroups() = (%v, %v), want ([g1], nil)", groups, err)
	}

	wantErr := errors.New("token expired")
	inner.err = wantErr
	if err := h.Verify(context.Background(), "tok"); err != wantErr {
		t.Errorf("Verify() = %v, want %v", err, wantErr)
	}
}

// TestOIDCVerifierHolderUnsupportedExtension verifies that installing a
// verifier which only implements the base TokenVerifier interface fails
// closed (rather than panicking on a bad type assertion) for every optional
// extension.
func TestOIDCVerifierHolderUnsupportedExtension(t *testing.T) {
	h := NewOIDCVerifierHolder()
	h.Set(&fakeTokenVerifier{})

	if _, err := h.VerifyAndIdentify(context.Background(), "tok"); err == nil {
		t.Error("VerifyAndIdentify() = nil error, want unsupported-extension failure")
	}
	if _, err := h.ExtractRoles(context.Background(), "tok"); err == nil {
		t.Error("ExtractRoles() = nil error, want unsupported-extension failure")
	}
	if _, err := h.ExtractGroups(context.Background(), "tok"); err == nil {
		t.Error("ExtractGroups() = nil error, want unsupported-extension failure")
	}
}
