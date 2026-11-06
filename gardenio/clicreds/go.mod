// Module gardenio/clicreds holds the CLI credential code shared between the
// gardenio CLI (gb) and the kbeads CLI (kd): oidclogin (the Keycloak device
// grant behind `gb login` and `kd login`, its session cache and refresh), the
// credential store, and the AVA cookie re-auth helpers. It also holds
// agentname, the escalation executor naming the controller spawns under and
// the daemon binds a per-agent token's subject to (gd-Ti5JGqdhpd), and
// beadswire, the beads daemon HTTP/JSON wire protocol (error types, retry,
// refresh exchange) shared by beadsapi and kbeads/internal/client
// (gd-VEsyCxkCkp).
//
// It deliberately depends only on the stdlib + go-keyring — NO controller,
// K8s, or kbeads deps — so importing it into kbeads does not drag the
// controller dependency tree along. Both modules link it via the repo-root
// go.work file plus a `replace … => ../clicreds` directive (the require uses
// the placeholder v0.0.0; replace pins it to the local path so the dotless
// workspace member resolves without a remote fetch).
module gardenio/clicreds

go 1.25.12

require github.com/zalando/go-keyring v0.2.8

require (
	github.com/danieljoos/wincred v1.2.3 // indirect
	github.com/godbus/dbus/v5 v5.2.2 // indirect
	golang.org/x/sys v0.30.0 // indirect
)
