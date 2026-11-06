package server

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// beadsapiFloorSrc is the read side's copy of the owner floor
// (beadsapi.EscalationAudienceForced), in the sibling gardenio/controller
// module. CI checks out the whole monorepo.
const beadsapiFloorSrc = "../../../gardenio/controller/internal/beadsapi/escalation_audience.go"

// readSideFloor parses beadsapi's escalationFloorOperationTypes map keys and
// escalationFloorPrefixes list from source: the two modules cannot import each
// other, so the source is the only place both are visible.
func readSideFloor(t *testing.T) (types map[string]bool, prefixes []string) {
	t.Helper()
	abs, err := filepath.Abs(beadsapiFloorSrc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(abs); os.IsNotExist(err) {
		t.Skipf("%s not checked out; the floor check runs where the controller module is present", beadsapiFloorSrc)
	}
	file, err := parser.ParseFile(token.NewFileSet(), abs, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", beadsapiFloorSrc, err)
	}
	str := func(e ast.Expr) string {
		lit, ok := e.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return ""
		}
		v, _ := strconv.Unquote(lit.Value)
		return v
	}
	types = map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
			return true
		}
		cl, ok := vs.Values[0].(*ast.CompositeLit)
		if !ok {
			return true
		}
		switch vs.Names[0].Name {
		case "escalationFloorOperationTypes":
			for _, e := range cl.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					types[str(kv.Key)] = true
				}
			}
		case "escalationFloorPrefixes":
			for _, e := range cl.Elts {
				prefixes = append(prefixes, str(e))
			}
		}
		return true
	})
	if len(types) == 0 || len(prefixes) == 0 {
		t.Fatalf("found no escalationFloorOperationTypes / escalationFloorPrefixes in %s; this check must be revisited", beadsapiFloorSrc)
	}
	return types, prefixes
}

// TestEscalationFloorMatchesReadSide keeps the owner floor's two copies equal
// (P10.F, gd-JJpjOjyvIL): kbeads stamps operator_only from its copy, and every
// owner surface reads beadsapi's. If they disagree, an escalation the daemon
// keeps on the floor shows as "in hand", or the reverse. A unit test on either
// side would pin only its own list.
func TestEscalationFloorMatchesReadSide(t *testing.T) {
	readTypes, readPrefixes := readSideFloor(t)
	if !slices.Equal(readPrefixes, escalationFloorPrefixes) {
		t.Errorf("beadsapi escalationFloorPrefixes = %v, kbeads %v", readPrefixes, escalationFloorPrefixes)
	}
	prefixed := func(op string) bool {
		return slices.ContainsFunc(escalationFloorPrefixes, func(p string) bool { return len(op) >= len(p) && op[:len(p)] == p })
	}
	// Every registered spelling the daemon puts on the floor by type is
	// spelled out on the read side, and nothing else is.
	for name := range operationTypeClass {
		onFloor := escalationFloorOperation(name) != ""
		if onFloor && !prefixed(name) && !readTypes[name] {
			t.Errorf("%q is on kbeads' floor but missing from beadsapi.escalationFloorOperationTypes", name)
		}
		if !onFloor && readTypes[name] {
			t.Errorf("%q is in beadsapi.escalationFloorOperationTypes but not on kbeads' floor", name)
		}
	}
	for name := range readTypes {
		if operationClassOf(name) == "" {
			t.Errorf("beadsapi floor type %q is not registered (escalation_op_registry.go)", name)
		}
	}
}

// TestEscalationFloorCategories pins the owner's five categories (ruling
// gd-CJUlVSMzEe) to sample types, and that work off the floor is not on it.
func TestEscalationFloorCategories(t *testing.T) {
	s := NewBeadsServer(newMockStore(), &events.NoopPublisher{})
	reason := func(op string) string {
		raw, _ := json.Marshal(map[string]string{"operation_type": op})
		return s.escalationForcedReason(context.Background(), &model.Bead{Type: model.TypeEscalation, Fields: raw})
	}
	for _, op := range []string{
		// secrets and credentials
		"litellm_key_rotation", "k8s_secret_create", "secret_anything",
		// global roles and identity
		"grant", "aws_iam", "k8s_rbac", "kcadm_write", "github_token_scope", "kbeads_role_grant", "ziti_identity_management",
		// gardenio's own policy
		"escalation_policy_write", "kbeads_authz_change", "github_repo_settings", "trigger_prompt_install",
		// money and billing
		"billing_change", "billing_anything", "spend_limit_change",
		// irreversible data deletion, and publishing outside the farm (P10.F4, gd-QiDj0JvkFr)
		"data_deletion", "terraform_destroy", "pvc_delete", "git_push_public",
	} {
		if got := reason(op); got != op || !isEscalationFloorReason(got) {
			t.Errorf("%s: forced reason %q, want the floor reason %q", op, got, op)
		}
	}
	for _, op := range []string{
		"gitlab_mr_merge", "helm_deploy", "helm_deploy_values", "kbeads_admin_write", "kbeads_bead_close",
		"kbeads_trigger_update", "github_actions_rerun", "github_workflow_dispatch", "close",
		"terraform_apply", "kcadm_read_only", "k8s_pod_delete", "edge_apply", "github_repo_create",
	} {
		if got := reason(op); got != "" {
			t.Errorf("%s is off the owner floor, got forced reason %q", op, got)
		}
	}
}
