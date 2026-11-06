package server

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// Machine principals (gd-gQ4iLIpWCm, design: gardenio/docs/machine-principal-design.md
// §4.1). A platform service (controller, bridge, viewer) authenticates with a
// Keycloak service-account JWT from a client registered in BEADS_MACHINE_CLIENTS.
// The daemon classifies it as authMethodMachine, which behaves like the static /
// fleet token everywhere (no roles, no OIDC-human carve-outs) except that
// authorizeWriteTitled consults machineCapabilityGrants for the token's
// gardenio-machine-* realm roles, and the token-mint routes accept the
// token-mint capability. With the registry empty none of this runs.

// authMethodMachine marks a request authenticated as a registered machine
// client's service account. It is neither authMethodOIDC (no human, no roles)
// nor a bearer-secret method (the credential is a signed, expiring JWT).
const authMethodMachine authMethod = "machine"

// machineRolesContextKey holds a machine principal's realm roles. They are
// deliberately NOT stored with WithRoles: RolesFromContext stays empty for a
// machine, so every existing role check (hasMinimumRole in authorizeWriteTitled,
// authorizeOwnership, requireRole) sees what it sees for the static token
// today. A machine token that carries gardenio-admin therefore gets NO admin.
const machineRolesContextKey contextKey = "machine.roles"

const (
	// machineServiceAccountPrefix is the preferred_username prefix Keycloak
	// gives a client's service-account user.
	machineServiceAccountPrefix = "service-account-"
	// machineSubjectPrefix prefixes AuthIdentity.Subject for a machine
	// principal ("machine:<clientId>").
	machineSubjectPrefix = "machine:"
	// machineRolePrefix is the namespace of capability roles in the grants table.
	machineRolePrefix = "gardenio-machine-"
)

var (
	errMachineNotServiceAccount = errors.New("machine client token is not a service-account token")
	errMachineUnregistered      = errors.New("unregistered machine client")
	errMachineClaims            = errors.New("machine client token claims could not be read")
)

// machinePrincipal is a classified machine caller.
type machinePrincipal struct {
	ClientID string
	Roles    []string
}

// Subject is the audit subject, "machine:<clientId>".
func (m machinePrincipal) Subject() string { return machineSubjectPrefix + m.ClientID }

// WithMachineRoles returns ctx carrying a machine principal's realm roles.
func WithMachineRoles(ctx context.Context, roles []string) context.Context {
	return context.WithValue(ctx, machineRolesContextKey, roles)
}

// machineRolesFromContext returns the machine realm roles, nil when absent.
func machineRolesFromContext(ctx context.Context) []string {
	v, _ := ctx.Value(machineRolesContextKey).([]string)
	return v
}

// classifyMachineToken returns (principal, true, nil) when raw is a
// service-account token from a client in fleetCfg's registry;
// (zero, false, nil) when the registry is empty or the token is neither a
// service-account token nor from a registered client (the caller then runs
// today's OIDC path unchanged); and an error (→ 401) when the token is a
// service-account token whose azp is NOT registered, a registered azp whose
// token is not that client's service account, or a registered azp whose claims
// cannot be read (never a fall-through to the human OIDC path).
//
// It verifies the signature through the extractors (go-oidc's verifier), not
// oidcVerifier.Verify, so an email-less service-account token is not refused by
// BEADS_OIDC_ALLOWED_EMAIL_DOMAINS, and a human token's domain check is
// untouched.
func classifyMachineToken(ctx context.Context, fleetCfg *FleetConfig, v TokenVerifier, raw string) (machinePrincipal, bool, error) {
	if !fleetCfg.HasMachineClients() || v == nil {
		return machinePrincipal{}, false, nil
	}
	cv, ok := v.(ClientExtractor)
	if !ok {
		return machinePrincipal{}, false, nil
	}
	sv, ok := v.(ServiceAccountExtractor)
	if !ok {
		return machinePrincipal{}, false, nil
	}
	// An extraction error means the token did not verify (or is not a JWT):
	// not a machine token. The caller's existing path rejects it.
	azp, err := cv.ExtractClientID(ctx, raw)
	if err != nil {
		return machinePrincipal{}, false, nil
	}
	registered := fleetCfg.IsMachineClient(azp)
	username, err := sv.ExtractServiceAccount(ctx, raw)
	if err != nil {
		if registered {
			// Fail closed: the azp already verified as a machine client, so
			// falling through would let the token take the human OIDC path,
			// where the X-Auth-Request-Email trust acts as any email.
			return machinePrincipal{}, false, errMachineClaims
		}
		return machinePrincipal{}, false, nil
	}
	isServiceAccount := strings.HasPrefix(username, machineServiceAccountPrefix)
	switch {
	case registered && username == machineServiceAccountPrefix+azp:
		var roles []string
		if re, ok := v.(RoleExtractor); ok {
			roles, err = re.ExtractRoles(ctx, raw)
			if err != nil {
				// Fail closed, as above: a registered client never falls
				// back to the human OIDC path.
				return machinePrincipal{}, false, errMachineClaims
			}
		}
		logUnknownMachineRoles(azp, roles)
		return machinePrincipal{ClientID: azp, Roles: roles}, true, nil
	case registered:
		return machinePrincipal{}, false, errMachineNotServiceAccount
	case isServiceAccount:
		return machinePrincipal{}, false, errMachineUnregistered
	}
	return machinePrincipal{}, false, nil
}

// unknownMachineRolesLogged remembers client+role pairs already logged.
var unknownMachineRolesLogged sync.Map

// logUnknownMachineRoles logs, once per client and role, a gardenio-machine-*
// role the grants table does not know. Such a role grants nothing.
func logUnknownMachineRoles(clientID string, roles []string) {
	for _, r := range roles {
		if !strings.HasPrefix(r, machineRolePrefix) {
			continue
		}
		if _, known := machineCapabilityGrants[r]; known {
			continue
		}
		if _, seen := unknownMachineRolesLogged.LoadOrStore(clientID+"\x00"+r, struct{}{}); seen {
			continue
		}
		slog.Debug("machine client holds a role with no grants entry; it grants nothing",
			"client", clientID, "role", r)
	}
}

// machineContext stamps a classified machine principal onto ctx. No request
// header is consulted: the identity is server-derived from the verified JWT.
func machineContext(ctx context.Context, fleetCfg *FleetConfig, m machinePrincipal) context.Context {
	ctx = withAuthMethod(ctx, authMethodMachine)
	ctx = WithMachineRoles(ctx, m.Roles)
	ctx = WithAuthIdentity(ctx, AuthIdentity{Type: "machine", Subject: m.Subject(), ClientID: m.ClientID})
	if fleetCfg != nil {
		ctx = WithFleet(ctx, fleetCfg.DefaultFleet)
	}
	return ctx
}

// machineAuthHTTP is the HTTP side of the classifier hook in AuthMiddleware.
// It returns (ctx, handled=true) on a machine principal, writes a 401 and
// returns handled=true on a classification error, and handled=false when the
// caller should continue with the OIDC path.
func machineAuthHTTP(w http.ResponseWriter, r *http.Request, fleetCfg *FleetConfig, v TokenVerifier, raw string) (context.Context, bool) {
	m, ok, err := classifyMachineToken(r.Context(), fleetCfg, v, raw)
	if err != nil {
		writeError(w, http.StatusUnauthorized, err.Error())
		return nil, true
	}
	if !ok {
		return nil, false
	}
	recordHeaderArrival("machine", false)
	return machineContext(r.Context(), fleetCfg, m), true
}
