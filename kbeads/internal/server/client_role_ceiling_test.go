package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// ceilingFakeVerifier is a RoleExtractor + ClientExtractor whose answers are
// fixed per raw token: "<azp>|<role>,<role>".
type ceilingFakeVerifier struct {
	clientErr error
}

func (f *ceilingFakeVerifier) Verify(context.Context, string) error { return nil }

func (f *ceilingFakeVerifier) ExtractRoles(_ context.Context, raw string) ([]string, error) {
	_, roles, _ := splitCeilingToken(raw)
	return roles, nil
}

func (f *ceilingFakeVerifier) ExtractClientID(_ context.Context, raw string) (string, error) {
	if f.clientErr != nil {
		return "", f.clientErr
	}
	azp, _, _ := splitCeilingToken(raw)
	return azp, nil
}

func splitCeilingToken(raw string) (string, []string, bool) {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '|' {
			var roles []string
			start := i + 1
			for j := start; j <= len(raw); j++ {
				if j == len(raw) || raw[j] == ',' {
					if j > start {
						roles = append(roles, raw[start:j])
					}
					start = j + 1
				}
			}
			return raw[:i], roles, true
		}
	}
	return "", nil, false
}

func TestCapRolesToCeiling(t *testing.T) {
	cases := []struct {
		name    string
		roles   []string
		ceiling string
		want    []string
	}{
		{"admin demoted to operator", []string{RoleAdmin}, RoleOperator, []string{RoleOperator}},
		{"admin+operator keeps operator once", []string{RoleAdmin, RoleOperator}, RoleOperator, []string{RoleOperator}},
		{"operator unchanged", []string{RoleOperator}, RoleOperator, []string{RoleOperator}},
		{"viewer unchanged", []string{RoleViewer}, RoleOperator, []string{RoleViewer}},
		{"no ladder role stays empty", []string{"gardenio-mention"}, RoleOperator, []string{"gardenio-mention"}},
		{"non-ladder roles kept on demotion", []string{"gardenio-mention", RoleAdmin}, RoleOperator, []string{"gardenio-mention", RoleOperator}},
		{"viewer ceiling demotes operator", []string{RoleOperator}, RoleViewer, []string{RoleViewer}},
		{"empty roles stay empty", nil, RoleOperator, []string{}},
		{"unknown ceiling is a no-op", []string{RoleAdmin}, "bogus", []string{RoleAdmin}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := capRolesToCeiling(tc.roles, tc.ceiling)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("capRolesToCeiling(%v, %q) = %v, want %v", tc.roles, tc.ceiling, got, tc.want)
			}
		})
	}
}

func TestClientRoleCeilingVerifier_CapsOnlyListedClient(t *testing.T) {
	v, err := NewClientRoleCeilingVerifier(&ceilingFakeVerifier{}, map[string]string{"gardenio-slack-link": RoleOperator})
	if err != nil {
		t.Fatal(err)
	}
	re := v.(RoleExtractor)
	got, err := re.ExtractRoles(context.Background(), "gardenio-slack-link|gardenio-admin")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{RoleOperator}) {
		t.Fatalf("slack-link admin should be capped to operator, got %v", got)
	}
	got, err = re.ExtractRoles(context.Background(), "gardenio-traefik-forwardauth|gardenio-admin")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{RoleAdmin}) {
		t.Fatalf("browser client must not be capped, got %v", got)
	}
	// The ceiling never widens: an operator through the capped client stays
	// an operator, a viewer stays a viewer.
	got, _ = re.ExtractRoles(context.Background(), "gardenio-slack-link|gardenio-viewer")
	if !reflect.DeepEqual(got, []string{RoleViewer}) {
		t.Fatalf("viewer must stay viewer, got %v", got)
	}
	// azp passes through so the escalation approver allowlist still sees it.
	if id, _ := v.(ClientExtractor).ExtractClientID(context.Background(), "gardenio-slack-link|x"); id != "gardenio-slack-link" {
		t.Fatalf("ExtractClientID = %q", id)
	}
}

// The ceiling fails closed: a token whose azp cannot be read is capped at the
// lowest configured ceiling rather than passed through as issued.
func TestClientRoleCeilingVerifier_UnreadableAzpFailsClosed(t *testing.T) {
	ceiling := map[string]string{"gardenio-slack-link": RoleOperator}
	v, err := NewClientRoleCeilingVerifier(&ceilingFakeVerifier{clientErr: errors.New("no azp")}, ceiling)
	if err != nil {
		t.Fatal(err)
	}
	got, err := v.(RoleExtractor).ExtractRoles(context.Background(), "gardenio-slack-link|gardenio-admin")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{RoleOperator}) {
		t.Fatalf("unreadable azp must be capped, got %v", got)
	}

	// An empty azp is the same case.
	v, _ = NewClientRoleCeilingVerifier(&ceilingFakeVerifier{}, ceiling)
	got, _ = v.(RoleExtractor).ExtractRoles(context.Background(), "|gardenio-admin")
	if !reflect.DeepEqual(got, []string{RoleOperator}) {
		t.Fatalf("empty azp must be capped, got %v", got)
	}

	// An inner verifier that cannot report the client at all is capped too.
	v, _ = NewClientRoleCeilingVerifier(&noClientRoleVerifier{}, ceiling)
	got, _ = v.(RoleExtractor).ExtractRoles(context.Background(), "x")
	if !reflect.DeepEqual(got, []string{RoleOperator}) {
		t.Fatalf("a verifier without ClientExtractor must be capped, got %v", got)
	}

	// With several ceilings the lowest wins.
	v, _ = NewClientRoleCeilingVerifier(&ceilingFakeVerifier{clientErr: errors.New("no azp")},
		map[string]string{"a": RoleOperator, "b": RoleViewer})
	got, _ = v.(RoleExtractor).ExtractRoles(context.Background(), "a|gardenio-admin")
	if !reflect.DeepEqual(got, []string{RoleViewer}) {
		t.Fatalf("lowest ceiling must win, got %v", got)
	}
}

// noClientRoleVerifier implements RoleExtractor but not ClientExtractor.
type noClientRoleVerifier struct{}

func (noClientRoleVerifier) Verify(context.Context, string) error { return nil }
func (noClientRoleVerifier) ExtractRoles(context.Context, string) ([]string, error) {
	return []string{RoleAdmin}, nil
}

func TestNewClientRoleCeilingVerifier_Validation(t *testing.T) {
	inner := &ceilingFakeVerifier{}
	if v, err := NewClientRoleCeilingVerifier(inner, nil); err != nil || v != TokenVerifier(inner) {
		t.Fatalf("empty ceiling must return inner unchanged: %v %v", v, err)
	}
	if _, err := NewClientRoleCeilingVerifier(inner, map[string]string{"c": "gardenio-machine-accounts"}); err == nil {
		t.Fatal("a non-ladder ceiling role must be refused")
	}
	if _, err := NewClientRoleCeilingVerifier(inner, map[string]string{"": RoleOperator}); err == nil {
		t.Fatal("an empty client id must be refused")
	}
}
