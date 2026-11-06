package server

import (
	"context"
	"net"
	"strings"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
)

// The cross-land matrix, gRPC half (gd-c1DOWNYy7W). The daemon's gRPC port
// is reachable from a tenant pod as its HTTP port is, so every BeadsService
// RPC is called as each of A's identities that gRPC admits, with every bead
// id in the request one of B's. Every call must hold three things, as on HTTP:
// never B's data in the reply, never a write to B, never success on an RPC
// that names B's bead. And an RPC whose HTTP twin (rpcScopeTwins) a tenant may
// not reach answers PermissionDenied; one it may reach answers B's bead as it
// answers a missing one (spec §5.6, gd-7APTAIhdC5). A land principal is
// HTTP-only and is refused every RPC.

// tokScopedViewerDrift is A's member who also holds gardenio-viewer, the
// realm-role drift of 10-25 under which RBACInterceptor's rank check alone
// passed GetConfig and ListConfigs.
const tokScopedViewerDrift = "cl-viewer-drift-tok"

// crossLandGRPC serves f's BeadsServer over an in-memory gRPC listener with the
// production interceptor chain, and returns a connection to it.
func crossLandGRPC(t *testing.T, f *scopedFixture) *grpc.ClientConn {
	t.Helper()
	f.keycloak.tokens[tokScopedViewerDrift] = tokenInfo{email: "clv@example.com", roles: []string{RoleProjectMember, RoleViewer}, groups: []string{"/project-" + crossLandA}}
	verifier := &multiIssuerMock{issuers: []*oidcIssuerMock{f.keycloak}}
	lis := bufconn.Listen(1 << 20)
	srv := NewGRPCServer(f.srv, verifier, f.fc)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatalf("grpc client: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// crossLandRPCRequest builds method's request with every bead id one of B's,
// and reports whether it names one.
func crossLandRPCRequest(m protoreflect.MethodDescriptor) (*dynamicpb.Message, bool) {
	in := dynamicpb.NewMessage(m.Input())
	namesB := false
	fills := map[protoreflect.Name]string{
		"id": bvTask, "bead_id": bvTask, "depends_on_id": bvDecision,
		"key": "bravo", "namespace": "bridge_token", "label": "project:" + crossLandB,
		"title": crossLandA + " probe", "text": crossLandA + " probe", "type": "task",
		"operator_id": bravoEmail, "query": crossLandB,
	}
	fields := m.Input().Fields()
	for i := range fields.Len() {
		fd := fields.Get(i)
		v, ok := fills[fd.Name()]
		if !ok || fd.Kind() != protoreflect.StringKind || fd.IsList() {
			continue
		}
		in.Set(fd, protoreflect.ValueOfString(v))
		namesB = namesB || v == bvTask
	}
	return in, namesB
}

// crossLandMissingRequest is in with B's bead id swapped for one that does
// not exist, so a call on B's bead can be compared with a call on a missing one.
func crossLandMissingRequest(in *dynamicpb.Message) *dynamicpb.Message {
	out := dynamicpb.NewMessageType(in.Descriptor()).New().Interface().(*dynamicpb.Message)
	in.Range(func(fd protoreflect.FieldDescriptor, v protoreflect.Value) bool {
		if fd.Kind() == protoreflect.StringKind && !fd.IsList() && v.String() == bvTask {
			v = protoreflect.ValueOfString("bv-missing")
		}
		out.Set(fd, v)
		return true
	})
	return out
}

// crossLandRPC calls method as token and returns the status code and the reply
// as JSON.
func crossLandRPC(t *testing.T, conn *grpc.ClientConn, token string, m protoreflect.MethodDescriptor, in *dynamicpb.Message) (codes.Code, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), crossLandStreamWait)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+token)
	out := dynamicpb.NewMessage(m.Output())
	full := "/" + beadsv1.BeadsService_ServiceDesc.ServiceName + "/" + string(m.Name())
	err := conn.Invoke(ctx, full, in, out)
	raw, _ := protojson.Marshal(out)
	return status.Code(err), status.Convert(err).Message() + " " + string(raw)
}

func beadsServiceMethods(t *testing.T) protoreflect.MethodDescriptors {
	t.Helper()
	svc := beadsv1.File_beads_v1_service_proto.Services().ByName("BeadsService")
	if svc == nil {
		t.Fatal("BeadsService descriptor not found")
	}
	return svc.Methods()
}

func TestCrossLandMatrix_GRPC(t *testing.T) {
	f := seedCrossLands(t)
	conn := crossLandGRPC(t, f)
	before := crossLandSnapshot(f)
	methods := beadsServiceMethods(t)

	for _, c := range []struct {
		name, token string
		tenant      bool // refused every RPC whose HTTP twin is admin-only
		refusedAll  bool
	}{
		{name: "A member", token: tokScopedWriter, refusedAll: true},
		{name: "A member with viewer", token: tokScopedViewerDrift, tenant: true},
		{name: "A land principal", token: tokLandClaimlayer, refusedAll: true},
		{name: "A agent", token: tokClaimAgent, tenant: true},
	} {
		for i := range methods.Len() {
			m := methods.Get(i)
			full := "/" + beadsv1.BeadsService_ServiceDesc.ServiceName + "/" + string(m.Name())
			in, namesB := crossLandRPCRequest(m)
			code, resp := crossLandRPC(t, conn, c.token, m, in)
			where := c.name + " " + string(m.Name())

			if strings.Contains(resp, crossLandMarker) {
				t.Errorf("%s: %s, the reply carries B's data: %.300s", where, code, resp)
			}
			if after := crossLandSnapshot(f); after != before {
				t.Errorf("%s: %s, B's beads changed:\nbefore %s\nafter  %s", where, code, before, after)
				f = seedCrossLands(t)
				conn = crossLandGRPC(t, f)
				before = crossLandSnapshot(f)
				continue
			}
			if namesB && code == codes.OK {
				t.Errorf("%s: OK on an RPC naming B's bead: %.300s", where, resp)
			}
			if m.Name() == "Health" {
				continue
			}
			class, ok := scopedRouteClasses[rpcScopeTwins[full]]
			adminOnly := !ok || class == classAdminOnly
			if (c.refusedAll || (c.tenant && adminOnly)) && code != codes.PermissionDenied {
				t.Errorf("%s: %s %.200s, want PermissionDenied", where, code, resp)
			}
			if namesB && !c.refusedAll && !adminOnly {
				if missing, _ := crossLandRPC(t, conn, c.token, m, crossLandMissingRequest(in)); missing != code {
					t.Errorf("%s: %s %.200s for B's bead but %s for a missing one, want the same (spec §5.6)", where, code, resp, missing)
				}
			}
		}
	}
}

// The gRPC positive control: B's own agent reads B's marker through GetBead
// with the same fill, so the walk's fills reach B's data; and an agent on an
// operator project still reaches the config RPCs a tenant is refused.
func TestCrossLandMatrix_GRPCControl(t *testing.T) {
	f := seedCrossLands(t)
	f.seedLandAgent("gd-agent-1", "crew", "gardenio", tokGardenioAgent)
	conn := crossLandGRPC(t, f)
	methods := beadsServiceMethods(t)

	get := methods.ByName("GetBead")
	in, _ := crossLandRPCRequest(get)
	if code, resp := crossLandRPC(t, conn, tokBravoAgent, get, in); code != codes.OK || !strings.Contains(resp, crossLandMarker) {
		t.Errorf("B agent GetBead(%s): %s %.300s, want OK with B's marker", bvTask, code, resp)
	}
	for _, name := range []protoreflect.Name{"GetConfig", "ListConfigs"} {
		m := methods.ByName(name)
		in, _ := crossLandRPCRequest(m)
		if code, resp := crossLandRPC(t, conn, tokGardenioAgent, m, in); strings.Contains(resp, tenantRPCRefusal) {
			t.Errorf("fleet agent %s: %s %.200s, want past the tenant gate", name, code, resp)
		}
	}
}

// Every BeadsService RPC has an HTTP twin in scopedRouteClasses, so the gRPC
// gate cannot fall back to its fail-closed default by omission.
func TestRPCScopeTwins_CoverEveryRPC(t *testing.T) {
	for _, m := range beadsv1.BeadsService_ServiceDesc.Methods {
		full := "/" + beadsv1.BeadsService_ServiceDesc.ServiceName + "/" + m.MethodName
		twin, ok := rpcScopeTwins[full]
		if !ok {
			t.Errorf("rpcScopeTwins has no entry for %s", full)
			continue
		}
		if _, ok := scopedRouteClasses[twin]; !ok {
			t.Errorf("rpcScopeTwins[%s] = %q, which scopedRouteClasses does not classify", full, twin)
		}
	}
	if len(beadsv1.BeadsService_ServiceDesc.Streams) != 0 {
		t.Errorf("BeadsService has streams: give each an rpcScopeTwins entry and extend this test")
	}
}
