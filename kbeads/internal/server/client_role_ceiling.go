package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

// clientRoleCeilingVerifier caps the gardenio ladder role carried by tokens
// that one of the listed OAuth clients (the token's azp) issued. It wraps the
// daemon's TokenVerifier so the cap applies before roles reach the request
// context, on the gRPC and HTTP paths alike, and so escalation approval,
// field write gates and every RBAC route see the capped roles.
//
// Why (gd-OZJ6H2S3xx, decision gd-uPHB7YcHuI): slack-bridge acts for a linked
// person with that person's own Keycloak token minted through the
// gardenio-slack-link client. Slack never carries gardenio-admin (decision
// gd-8PT2Qr2gEo); before linking the bridge enforced that in its own code
// (capSlackRoles). With the person's real token the ceiling must live where
// the token is honoured, here, so a stolen Slack-door credential is at most
// an operator wherever kbeads is the resource server. Realm roles are not
// composite (gardenio-admin does not include gardenio-operator), so the cap
// demotes rather than strips: a person above the ceiling gets exactly the
// ceiling role; a person at or below it keeps their own. Roles outside the
// ladder (gardenio-machine-*, gardenio-mention) pass through untouched.
type clientRoleCeilingVerifier struct {
	inner   TokenVerifier
	ceiling map[string]string
}

// NewClientRoleCeilingVerifier wraps inner so ExtractRoles caps the ladder
// roles of tokens whose azp is a key of ceiling at that key's role. Every
// value must be a ladder role (gardenio-viewer, gardenio-operator,
// gardenio-admin); an empty map returns inner unchanged.
func NewClientRoleCeilingVerifier(inner TokenVerifier, ceiling map[string]string) (TokenVerifier, error) {
	if len(ceiling) == 0 {
		return inner, nil
	}
	clean := make(map[string]string, len(ceiling))
	for client, role := range ceiling {
		client, role = strings.TrimSpace(client), strings.TrimSpace(role)
		if client == "" {
			return nil, errors.New("client role ceiling: empty client id")
		}
		if _, ok := roleRank[role]; !ok {
			return nil, fmt.Errorf("client role ceiling for %q: %q is not a gardenio ladder role", client, role)
		}
		clean[client] = role
	}
	return &clientRoleCeilingVerifier{inner: inner, ceiling: clean}, nil
}

// capRolesToCeiling returns roles with every ladder role ranked above ceiling
// removed; when something was removed and the ceiling role itself is absent,
// the ceiling role is added so the person is demoted, not stripped. Non-ladder
// roles are kept. Order is preserved (the added ceiling role goes last).
func capRolesToCeiling(roles []string, ceiling string) []string {
	max, ok := roleRank[ceiling]
	if !ok {
		return roles
	}
	out := make([]string, 0, len(roles)+1)
	demoted := false
	for _, r := range roles {
		if rank, ladder := roleRank[r]; ladder && rank > max {
			demoted = true
			continue
		}
		out = append(out, r)
	}
	if demoted && !slices.Contains(out, ceiling) {
		out = append(out, ceiling)
	}
	return out
}

// lowestCeiling returns the lowest-ranked configured ceiling role, used when a
// token's azp is unreadable.
func (c *clientRoleCeilingVerifier) lowestCeiling() string {
	lowest := ""
	for _, role := range c.ceiling {
		if lowest == "" || roleRank[role] < roleRank[lowest] {
			lowest = role
		}
	}
	return lowest
}

// Ceilings returns the configured client=role pairs, sorted, for logging.
func (c *clientRoleCeilingVerifier) Ceilings() []string {
	out := make([]string, 0, len(c.ceiling))
	for client, role := range c.ceiling {
		out = append(out, client+"="+role)
	}
	sort.Strings(out)
	return out
}

func (c *clientRoleCeilingVerifier) Verify(ctx context.Context, rawToken string) error {
	return c.inner.Verify(ctx, rawToken)
}

func (c *clientRoleCeilingVerifier) VerifyAndIdentify(ctx context.Context, rawToken string) (string, error) {
	iv, ok := c.inner.(IdentityVerifier)
	if !ok {
		return "", errors.New("wrapped oidc verifier does not support identity extraction")
	}
	return iv.VerifyAndIdentify(ctx, rawToken)
}

// ExtractRoles returns the inner roles, capped when the token's azp has a
// ceiling. It fails CLOSED: when the inner verifier cannot report the azp, or
// the token carries none, the roles are capped at the lowest configured
// ceiling, because the token might have come from a capped client and an
// uncapped read would be the bypass. (A token with a readable azp that is not
// listed is left alone.)
func (c *clientRoleCeilingVerifier) ExtractRoles(ctx context.Context, rawToken string) ([]string, error) {
	re, ok := c.inner.(RoleExtractor)
	if !ok {
		return nil, errors.New("wrapped oidc verifier does not support role extraction")
	}
	roles, err := re.ExtractRoles(ctx, rawToken)
	if err != nil {
		return nil, err
	}
	ce, ok := c.inner.(ClientExtractor)
	if !ok {
		return capRolesToCeiling(roles, c.lowestCeiling()), nil
	}
	clientID, err := ce.ExtractClientID(ctx, rawToken)
	if err != nil || clientID == "" {
		return capRolesToCeiling(roles, c.lowestCeiling()), nil
	}
	if ceiling, ok := c.ceiling[clientID]; ok {
		return capRolesToCeiling(roles, ceiling), nil
	}
	return roles, nil
}

func (c *clientRoleCeilingVerifier) ExtractGroups(ctx context.Context, rawToken string) ([]string, error) {
	ge, ok := c.inner.(GroupExtractor)
	if !ok {
		return nil, errors.New("wrapped oidc verifier does not support group extraction")
	}
	return ge.ExtractGroups(ctx, rawToken)
}

func (c *clientRoleCeilingVerifier) ExtractClientID(ctx context.Context, rawToken string) (string, error) {
	ce, ok := c.inner.(ClientExtractor)
	if !ok {
		return "", errors.New("wrapped oidc verifier does not support client extraction")
	}
	return ce.ExtractClientID(ctx, rawToken)
}

func (c *clientRoleCeilingVerifier) ExtractAudiences(ctx context.Context, rawToken string) ([]string, error) {
	av, ok := c.inner.(AudienceExtractor)
	if !ok {
		return nil, errors.New("wrapped oidc verifier does not support audience extraction")
	}
	return av.ExtractAudiences(ctx, rawToken)
}

func (c *clientRoleCeilingVerifier) ExtractGitHub(ctx context.Context, rawToken string) (int64, string, error) {
	gv, ok := c.inner.(GitHubExtractor)
	if !ok {
		return 0, "", errors.New("wrapped oidc verifier does not support github extraction")
	}
	return gv.ExtractGitHub(ctx, rawToken)
}

func (c *clientRoleCeilingVerifier) ExtractImpersonation(ctx context.Context, rawToken string) (string, string, error) {
	iv, ok := c.inner.(ImpersonationExtractor)
	if !ok {
		return "", "", errors.New("wrapped oidc verifier does not support impersonation extraction")
	}
	return iv.ExtractImpersonation(ctx, rawToken)
}

func (c *clientRoleCeilingVerifier) ExtractServiceAccount(ctx context.Context, rawToken string) (string, error) {
	se, ok := c.inner.(ServiceAccountExtractor)
	if !ok {
		return "", errors.New("wrapped oidc verifier does not support service-account extraction")
	}
	return se.ExtractServiceAccount(ctx, rawToken)
}
