package server

import (
	"context"
	"net/http"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Push device registry, per person (Lands 4.7b, gd-a7U4AgwgCA).
//
// A device row is owned by the human whose OIDC token registered it: the owner
// is the token's subject, never a request field, so nobody can enrol a phone
// for someone else. The push bridge resolves an alert's addressed humans to
// their rows (GET ?user_email=...), so an alert reaches only its addressees'
// devices.
//
// Who sees what:
//   - an OIDC caller without gardenio-admin sees and deletes only their own
//     devices;
//   - an OIDC admin, and the push bridge's own machine principal
//     (machine:gardenio-push-bridge, its Keycloak service account), see and
//     delete any device;
//   - every other caller gets 403 on all three verbs: the shared static
//     token, fleet and per-agent tokens, other machine principals, edge and
//     auth-disabled dev. Not being OIDC is not trust here (gd-5f5bJPKLhC,
//     gd-ke6PC7bRto retire that shortcut), so a crew agent can never read or
//     prune a person's devices.
//
// These rows are not beads, so no bead event is recorded or published.

// pushDeviceTokenMinLen / MaxLen bound the hex APNs token. Today's tokens are
// 32 bytes (64 hex chars); Apple says the length can change, so the bound is
// loose.
const (
	pushDeviceTokenMinLen = 32
	pushDeviceTokenMaxLen = 256
	pushDeviceBundleIDMax = 255
)

// registerPushDeviceRequest is the body for POST /v1/devices. There is no
// owner field on purpose: the owner is the caller.
type registerPushDeviceRequest struct {
	Token    string `json:"token"`
	Platform string `json:"platform"`
	BundleID string `json:"bundle_id"`
}

type listPushDevicesResponse struct {
	Devices []*model.PushDevice `json:"devices"`
}

// pushBridgeMachineSubject is the push bridge's machine principal, the one
// non-human caller that reads and prunes any person's devices. The subject is
// derived server-side from the verified service-account JWT
// (machineContext), never from a header.
const pushBridgeMachineSubject = machineSubjectPrefix + "gardenio-push-bridge"

// pushDeviceScope reports whose devices the caller may read and delete. all
// is true for an OIDC admin and the push bridge's machine principal. owner is
// a non-admin OIDC caller's own lowercase email. ("", false) refuses the
// caller: every other machine, static, fleet, edge and auth-disabled caller,
// and an OIDC token with no usable subject.
func pushDeviceScope(ctx context.Context) (owner string, all bool) {
	switch authMethodFromContext(ctx) {
	case authMethodOIDC:
		if hasMinimumRole(RolesFromContext(ctx), RoleAdmin) {
			return "", true
		}
		return pushDeviceCallerEmail(ctx), false
	case authMethodMachine:
		return "", AuthIdentityFromContext(ctx).Subject == pushBridgeMachineSubject
	}
	return "", false
}

// pushDeviceCallerEmail returns the OIDC caller's lowercase email, or "".
func pushDeviceCallerEmail(ctx context.Context) string {
	id := AuthIdentityFromContext(ctx)
	if authMethodFromContext(ctx) != authMethodOIDC || id.Type != "oidc" {
		return ""
	}
	email := canonicalAliasSubject("oidc", id.Subject)
	if at := strings.IndexByte(email, '@'); at <= 0 || at == len(email)-1 {
		return ""
	}
	return email
}

// normalizePushDeviceToken lowercases a hex APNs token and reports whether it
// is well formed.
func normalizePushDeviceToken(raw string) (string, bool) {
	tok := strings.ToLower(strings.TrimSpace(raw))
	if len(tok) < pushDeviceTokenMinLen || len(tok) > pushDeviceTokenMaxLen {
		return "", false
	}
	for i := 0; i < len(tok); i++ {
		c := tok[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return tok, true
}

// handleRegisterPushDevice handles POST /v1/devices. Only a human's own OIDC
// token may register, and the row is owned by that human. Re-registering a
// token is idempotent for the same person and moves it for another.
func (s *BeadsServer) handleRegisterPushDevice(w http.ResponseWriter, r *http.Request) {
	email := pushDeviceCallerEmail(r.Context())
	if email == "" {
		writeError(w, http.StatusForbidden, "device registration requires the person's own OIDC token")
		return
	}
	var req registerPushDeviceRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	tok, ok := normalizePushDeviceToken(req.Token)
	if !ok {
		writeError(w, http.StatusBadRequest, "token: must be a hex APNs device token")
		return
	}
	platform := strings.ToLower(strings.TrimSpace(req.Platform))
	if platform == "" {
		platform = model.PushDevicePlatformIOS
	}
	if platform != model.PushDevicePlatformIOS {
		writeError(w, http.StatusBadRequest, "platform: must be \"ios\"")
		return
	}
	bundleID := strings.TrimSpace(req.BundleID)
	if len(bundleID) > pushDeviceBundleIDMax || strings.ContainsAny(bundleID, " \t\r\n") {
		writeError(w, http.StatusBadRequest, "bundle_id: must be a bundle identifier")
		return
	}

	d := &model.PushDevice{Token: tok, Platform: platform, BundleID: bundleID, UserEmail: email}
	if err := s.store.UpsertPushDevice(r.Context(), d); err != nil {
		writeServerError(r.Context(), w, "handleRegisterPushDevice", err, "failed to register device")
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// handleListPushDevices handles GET /v1/devices[?user_email=a&user_email=b,c]
// [&platform=ios]. A caller limited to their own devices gets 403 for asking
// about anyone else, and their own rows when they name nobody.
func (s *BeadsServer) handleListPushDevices(w http.ResponseWriter, r *http.Request) {
	owner, all := pushDeviceScope(r.Context())
	if !all && owner == "" {
		writeError(w, http.StatusForbidden, "listing devices requires the person's own OIDC token, an admin, or the push bridge")
		return
	}
	var emails []string
	for _, v := range r.URL.Query()["user_email"] {
		for _, e := range strings.Split(v, ",") {
			if e = canonicalAliasSubject("oidc", e); e != "" {
				emails = append(emails, e)
			}
		}
	}
	if !all {
		for _, e := range emails {
			if e != owner {
				writeError(w, http.StatusForbidden, "you may list only your own devices")
				return
			}
		}
		emails = []string{owner}
	}
	devices, err := s.store.ListPushDevices(r.Context(), model.PushDeviceFilter{
		UserEmails: emails,
		Platform:   strings.ToLower(strings.TrimSpace(r.URL.Query().Get("platform"))),
	})
	if err != nil {
		writeServerError(r.Context(), w, "handleListPushDevices", err, "failed to list devices")
		return
	}
	if devices == nil {
		devices = []*model.PushDevice{}
	}
	writeJSON(w, http.StatusOK, listPushDevicesResponse{Devices: devices})
}

// handleDeletePushDevice handles DELETE /v1/devices/{token}. Idempotent: a
// miss, including another person's token for a caller limited to their own,
// returns 204, so the response never says whose a token is.
func (s *BeadsServer) handleDeletePushDevice(w http.ResponseWriter, r *http.Request) {
	owner, all := pushDeviceScope(r.Context())
	if !all && owner == "" {
		writeError(w, http.StatusForbidden, "deleting a device requires the person's own OIDC token, an admin, or the push bridge")
		return
	}
	tok, ok := normalizePushDeviceToken(r.PathValue("token"))
	if !ok {
		writeError(w, http.StatusBadRequest, "token: must be a hex APNs device token")
		return
	}
	if _, err := s.store.DeletePushDevice(r.Context(), tok, owner); err != nil {
		writeServerError(r.Context(), w, "handleDeletePushDevice", err, "failed to delete device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
