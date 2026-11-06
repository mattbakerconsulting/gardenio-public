package bridge

import (
	"context"
	"testing"

	"gardenio/controller/internal/beadsapi"
	"gardenio/controller/internal/conversation/surfacetest"
)

type fixedMachineToken string

func (f fixedMachineToken) Token(context.Context) (string, error) { return string(f), nil }

// type:permission is admin-gated (gd-RuI4Ze6vAg): the bridge files the request
// bead and closes it with the verdict as its machine principal, which holds the
// grant, never on its fleet token.
func TestPermissionBead_WritesRideTheMachinePrincipal(t *testing.T) {
	b, daemon := newDecisionCardsBot(t, surfacetest.NewSlackTwin())
	machine := fixedMachineToken("machine-jwt")
	b.SetMachinePrincipal(machine)
	ctx := context.Background()

	id, land := b.writePermissionBead(ctx, channelPermissionCallback{
		Type: "permission_request", RequestID: "req-1", ToolName: "Bash", Agent: "crew-x",
	})
	if id == "" || land != "gardenio" {
		t.Fatalf("writePermissionBead = (%q, %q)", id, land)
	}
	if got := beadsapi.MachinePrincipalFromContext(daemon.lastCreateBeadCtx); got != machine {
		t.Fatalf("permission create carried %v, want the machine principal", got)
	}

	b.closePermissionBead(ctx, id, "allow", permissionAnswerer{SlackID: "UMATT", Principal: "oidc:matt@example.com"}, nil)
	if len(daemon.getClosed()) != 1 || daemon.getClosed()[0].BeadID != id {
		t.Fatalf("closed = %+v, want %s", daemon.getClosed(), id)
	}
	if got := beadsapi.MachinePrincipalFromContext(daemon.lastCloseBeadCtx); got != machine {
		t.Fatalf("permission close carried %v, want the machine principal", got)
	}
}

// Without a machine principal the writes go out on the daemon's own token, as
// before (kbeads refuses them; the prompt is still posted).
func TestPermissionBead_NoMachinePrincipal(t *testing.T) {
	b, daemon := newDecisionCardsBot(t, surfacetest.NewSlackTwin())
	if id, _ := b.writePermissionBead(context.Background(), channelPermissionCallback{
		Type: "permission_request", RequestID: "req-1", ToolName: "Bash", Agent: "crew-x",
	}); id == "" {
		t.Fatal("no permission bead written")
	}
	if got := beadsapi.MachinePrincipalFromContext(daemon.lastCreateBeadCtx); got != nil {
		t.Fatalf("create carried a machine principal %v with none set", got)
	}
}
