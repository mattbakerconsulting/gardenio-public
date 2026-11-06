// Package natsauth builds NATS client auth options for the gardenio
// deployment's two auth modes:
//
//   - legacy single-token auth (nats.auth.enabled, no subjectIsolation):
//     the secret is sent as a bare token (nats.Token).
//   - subject isolation (nats.auth.subjectIsolation): the server declares
//     multi-user authorization{users:[...]}, whose entries only support
//     user/password/nkey — a bare token can NEVER match one (the server
//     rejects per-user `token` fields at startup, ka-0000000002). The shared
//     secret is therefore delivered together with a username ("infra" or
//     "agent") and sent as that user's PASSWORD (nats.UserInfo).
//
// Callers thread the optional username through from their *_NATS_USER env
// var; an empty username preserves the legacy token behavior byte-for-byte.
//
// StartupRetry is the boot-order half: a long-running service that starts
// before the NATS server keeps retrying instead of running without its
// listeners until the next restart (gd-XTyUibxpls).
package natsauth

import "github.com/nats-io/nats.go"

// Options returns the auth option set for an optional username + secret
// pair. Empty token → no auth options; token only → legacy nats.Token;
// user+token → nats.UserInfo with the token as the password.
func Options(user, token string) []nats.Option {
	switch {
	case user != "" && token != "":
		return []nats.Option{nats.UserInfo(user, token)}
	case token != "":
		return []nats.Option{nats.Token(token)}
	default:
		return nil
	}
}

// StartupRetry returns the options a long-running service adds so a NATS
// server that is not up yet at boot is retried in the background instead of
// failing Connect. Connect then returns a conn in the RECONNECTING state:
// publishes and plain subscriptions are buffered and sent once it connects,
// but a Flush or a JetStream call made before then fails. A caller with
// such a setup step runs it when conn.IsConnected() is already true, or else
// from a nats.ConnectHandler, which fires once on the first connect.
//
// CLIs that should fail fast (gb hook relay) do not use it.
func StartupRetry() []nats.Option {
	return []nats.Option{nats.RetryOnFailedConnect(true), nats.MaxReconnects(-1)}
}
