package natsauth

import (
	"fmt"
	"net"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// startIsolationServer runs an embedded nats-server configured exactly like
// the chart's subjectIsolation authorization block (users with passwords +
// per-user permissions) and returns its client URL.
func startIsolationServer(t *testing.T) string {
	t.Helper()
	opts := &natsserver.Options{
		Host: "127.0.0.1",
		Port: -1, // random free port
		Users: []*natsserver.User{
			{
				Username: "infra",
				Password: "infra-secret",
				Permissions: &natsserver.Permissions{
					Publish:   &natsserver.SubjectPermission{Allow: []string{">"}},
					Subscribe: &natsserver.SubjectPermission{Allow: []string{">"}},
				},
			},
			{
				Username: "agent",
				Password: "agent-secret",
				Permissions: &natsserver.Permissions{
					Publish:   &natsserver.SubjectPermission{Allow: []string{"coop.events.>", "hooks.>", "_INBOX.>"}},
					Subscribe: &natsserver.SubjectPermission{Allow: []string{"_INBOX.>"}},
				},
			},
		},
	}
	srv, err := natsserver.NewServer(opts)
	if err != nil {
		t.Fatalf("embedded nats-server: %v", err)
	}
	go srv.Start()
	t.Cleanup(srv.Shutdown)
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("embedded nats-server not ready")
	}
	return srv.ClientURL()
}

// TestUserInfoAuthAgainstMultiUserServer is the live contract behind
// ka-0000000002: under the chart's subjectIsolation server config
// (multi-user authorization), only user+password auth succeeds. A client
// sending the shared secret as a bare token — the pre-fix behavior of every
// gardenio client — must be REFUSED, as must an anonymous client.
func TestUserInfoAuthAgainstMultiUserServer(t *testing.T) {
	url := startIsolationServer(t)

	nc, err := nats.Connect(url, Options("infra", "infra-secret")...)
	if err != nil {
		t.Fatalf("infra user+password connect must succeed: %v", err)
	}
	nc.Close()

	nc, err = nats.Connect(url, Options("agent", "agent-secret")...)
	if err != nil {
		t.Fatalf("agent user+password connect must succeed: %v", err)
	}
	nc.Close()

	if nc, err = nats.Connect(url, Options("", "infra-secret")...); err == nil {
		nc.Close()
		t.Fatal("bare-token connect must be refused by a multi-user server")
	}
	if nc, err = nats.Connect(url); err == nil {
		nc.Close()
		t.Fatal("anonymous connect must be refused")
	}
	if nc, err = nats.Connect(url, Options("infra", "wrong")...); err == nil {
		nc.Close()
		t.Fatal("wrong password must be refused")
	}
}

// TestOptionsShape pins the option-set selection: user+token → UserInfo,
// token only → legacy Token, neither → no options.
func TestOptionsShape(t *testing.T) {
	if got := len(Options("", "")); got != 0 {
		t.Errorf("Options(\"\",\"\") returned %d options, want 0", got)
	}
	var o nats.Options
	for _, opt := range Options("infra", "s3cret") {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	if o.User != "infra" || o.Password != "s3cret" || o.Token != "" {
		t.Errorf("user+token: got user=%q pass=%q token=%q; want UserInfo only", o.User, o.Password, o.Token)
	}
	o = nats.Options{}
	for _, opt := range Options("", "s3cret") {
		if err := opt(&o); err != nil {
			t.Fatal(err)
		}
	}
	if o.Token != "s3cret" || o.User != "" {
		t.Errorf("token only: got user=%q token=%q; want legacy token", o.User, o.Token)
	}
}

// TestStartupRetry_ServerUpAfterConnect is the boot-order case of
// gd-XTyUibxpls: a service connects and subscribes before nats-0 is ready.
// With StartupRetry, Connect succeeds on a not-yet-connected conn, and the
// subscription made then receives messages once the server comes up.
func TestStartupRetry_ServerUpAfterConnect(t *testing.T) {
	// Reserve a free port, then free it so nothing listens there yet.
	probe := natsserver.New(&natsserver.Options{Host: "127.0.0.1", Port: -1, NoLog: true, NoSigs: true})
	if probe == nil {
		t.Fatal("probe server")
	}
	go probe.Start()
	if !probe.ReadyForConnections(5 * time.Second) {
		t.Fatal("probe server not ready")
	}
	port := probe.Addr().(*net.TCPAddr).Port
	probe.Shutdown()
	probe.WaitForShutdown()
	url := fmt.Sprintf("nats://127.0.0.1:%d", port)

	connected := make(chan struct{})
	opts := append([]nats.Option{
		nats.ReconnectWait(50 * time.Millisecond),
		nats.ConnectHandler(func(*nats.Conn) { close(connected) }),
	}, StartupRetry()...)
	nc, err := nats.Connect(url, opts...)
	if err != nil {
		t.Fatalf("Connect with NATS down = %v, want a retrying conn", err)
	}
	defer nc.Close()
	if nc.IsConnected() {
		t.Fatal("conn reports connected before the server is up")
	}
	got := make(chan string, 1)
	if _, err := nc.Subscribe("boot.order", func(m *nats.Msg) { got <- string(m.Data) }); err != nil {
		t.Fatalf("Subscribe while connecting: %v", err)
	}

	srv := natsserver.New(&natsserver.Options{Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true})
	if srv == nil {
		t.Fatal("server")
	}
	go srv.Start()
	defer srv.Shutdown()
	if !srv.ReadyForConnections(5 * time.Second) {
		t.Fatal("server not ready")
	}

	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("ConnectHandler never fired after the server came up")
	}
	if err := nc.FlushTimeout(5 * time.Second); err != nil {
		t.Fatalf("flush after connect: %v", err)
	}

	pub, err := nats.Connect(url)
	if err != nil {
		t.Fatalf("publisher connect: %v", err)
	}
	defer pub.Close()
	if err := pub.Publish("boot.order", []byte("hello")); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case m := <-got:
		if m != "hello" {
			t.Fatalf("got %q, want hello", m)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("subscription made before the server was up never received")
	}
}
