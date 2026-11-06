package main

import (
	"github.com/spf13/cobra"
)

// fleetTokenCmd is the DEPRECATED alias for `kd bridge token` (decision
// ka-0000000201 = dual-accept). It forwards to the canonical bridge-token
// implementation in bridge_token.go and shares the same flag globals. It is
// kept working through the compat window; removal is the cleanup bead
// ka-0000004906.
//
// NOTE: this lives under `kd fleet` (the tenancy fleet command) only for
// backward compatibility — the underlying concept is the bridge CREDENTIAL
// token, NOT the tenancy fleet. Use `kd bridge token` going forward.
var fleetTokenCmd = &cobra.Command{
	Use:        "token",
	Short:      "Bridge token management (admin-only) [deprecated: use 'kd bridge token']",
	Deprecated: "use 'kd bridge token' instead",
}

var fleetTokenRegisterCmd = &cobra.Command{
	Use:        "register",
	Short:      "Register a bridge token via POST /v1/bridge-tokens [deprecated: use 'kd bridge token register']",
	Deprecated: "use 'kd bridge token register' instead",
	RunE:       runBridgeTokenRegister,
}

func init() {
	registerBridgeTokenRegisterFlags(fleetTokenRegisterCmd)
	fleetTokenCmd.AddCommand(fleetTokenRegisterCmd)
	fleetCmd.AddCommand(fleetTokenCmd)
}
