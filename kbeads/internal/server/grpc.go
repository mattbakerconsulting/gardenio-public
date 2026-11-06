package server

import (
	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/reflection"
)

// DefaultGRPCMaxRecvBytes is the explicit inbound message size ceiling applied
// to the gRPC server. Matches the grpc-go default (4 MiB) but bounded
// explicitly to document intent and prevent future library changes from
// raising the limit silently.
const DefaultGRPCMaxRecvBytes = 4 * 1024 * 1024

// GRPCServerOptions configures NewGRPCServer.
type GRPCServerOptions struct {
	// EnableReflection registers gRPC server reflection. Reflection leaks the
	// full service schema to any caller that can reach the port and should be
	// disabled in production. Defaults to false; set via BEADS_GRPC_REFLECTION.
	EnableReflection bool
	// MaxRecvBytes caps inbound message size. 0 uses DefaultGRPCMaxRecvBytes.
	MaxRecvBytes int
}

// NewGRPCServer creates a gRPC server with the standard interceptor chain
// (recovery → revocation → auth → RBAC → tenant scope → logging), registers the BeadsService,
// and returns it ready to serve. Reflection is OFF by default per §C2 of the
// 2026-04-18 audit; enable it only in development via GRPCServerOptions.
//
// When fleet tokens are configured or verifier is non-nil, the auth interceptor
// is inserted into the chain. RBAC enforces per-RPC role minimums for
// OIDC-authenticated callers (fleet-token callers bypass, matching HTTP
// requireRole semantics); scopedRPCGate then refuses a tenant every RPC whose
// HTTP twin scopedRouteGate refuses it. There is no shared static bearer: a token that is
// neither a fleet token nor a verifiable JWT is Unauthenticated (gd-ke6PC7bRto).
func NewGRPCServer(beadsServer *BeadsServer, verifier TokenVerifier, fleetCfg *FleetConfig, opts ...GRPCServerOptions) *grpc.Server {
	var o GRPCServerOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	if o.MaxRecvBytes <= 0 {
		o.MaxRecvBytes = DefaultGRPCMaxRecvBytes
	}

	unary := []grpc.UnaryServerInterceptor{RecoveryInterceptor}
	stream := []grpc.StreamServerInterceptor{RecoveryStreamInterceptor}

	if beadsServer.revocation != nil && beadsServer.revocation.Size() > 0 {
		unary = append(unary, RevocationInterceptor(beadsServer.revocation))
		stream = append(stream, RevocationStreamInterceptor(beadsServer.revocation))
	}
	if fleetCfg.HasTokens() || verifier != nil {
		unary = append(unary, windowAuthInterceptor(beadsServer, fleetCfg, verifier))
		stream = append(stream, windowAuthStreamInterceptor(beadsServer, fleetCfg, verifier))
	}
	unary = append(unary, RBACInterceptor, beadsServer.scopedRPCGate, beadsServer.impersonationAuditInterceptor)
	stream = append(stream, RBACStreamInterceptor, beadsServer.scopedRPCStreamGate, RequestCountStreamInterceptor)
	unary = append(unary, LoggingInterceptor)

	srv := grpc.NewServer(
		grpc.StatsHandler(otelgrpc.NewServerHandler()),
		grpc.ChainUnaryInterceptor(unary...),
		grpc.ChainStreamInterceptor(stream...),
		grpc.MaxRecvMsgSize(o.MaxRecvBytes),
	)

	beadsv1.RegisterBeadsServiceServer(srv, beadsServer)
	if o.EnableReflection {
		reflection.Register(srv)
	}

	return srv
}
