package client

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"
)

func TestNewZitiTransportNoConfigUsesDefault(t *testing.T) {
	t.Parallel()

	inner, ztx, err := newZitiTransport(ZitiConfig{})
	if err != nil {
		t.Fatalf("newZitiTransport error: %v", err)
	}
	if ztx != nil {
		t.Fatalf("ziti dialer = %v, want nil when unconfigured", ztx)
	}
	if inner != http.DefaultTransport {
		t.Fatalf("transport = %v, want http.DefaultTransport when unconfigured", inner)
	}
}

func TestNewZitiTransportDialerError(t *testing.T) {
	oldNewZitiDialer := newZitiDialer
	newZitiDialer = func(string) (zitiDialer, error) {
		return nil, errors.New("boom")
	}
	t.Cleanup(func() { newZitiDialer = oldNewZitiDialer })

	_, _, err := newZitiTransport(ZitiConfig{IdentityPath: "id.json", Service: "svc"})
	if err == nil || !strings.Contains(err.Error(), "initializing ziti transport") || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want wrapped initializing-ziti-transport/boom", err)
	}
}

func TestNewZitiTransportSetsTLSServerName(t *testing.T) {
	oldNewZitiDialer := newZitiDialer
	fake := &fakeZitiDialer{}
	newZitiDialer = func(string) (zitiDialer, error) { return fake, nil }
	t.Cleanup(func() { newZitiDialer = oldNewZitiDialer })

	inner, ztx, err := newZitiTransport(ZitiConfig{
		IdentityPath:  "id.json",
		Service:       "svc",
		TLSServerName: "daemon.internal",
	})
	if err != nil {
		t.Fatalf("newZitiTransport error: %v", err)
	}
	if ztx == nil {
		t.Fatal("ziti dialer = nil, want configured dialer")
	}
	tr, ok := inner.(*http.Transport)
	if !ok {
		t.Fatalf("transport type = %T, want *http.Transport", inner)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.ServerName != "daemon.internal" {
		t.Fatalf("TLS server name = %v, want daemon.internal", tr.TLSClientConfig)
	}
	ztx.Close()
}

type fakeZitiDialer struct {
	serviceName string
	closed      bool
}

func (d *fakeZitiDialer) DialContext(_ context.Context, serviceName string) (net.Conn, error) {
	d.serviceName = serviceName
	return nil, errors.New("fake ziti dial")
}

func (d *fakeZitiDialer) Close() {
	d.closed = true
}

func TestNewHTTPClientWithZitiRequiresIdentityAndService(t *testing.T) {
	t.Parallel()

	if _, err := NewHTTPClientWithZiti("http://localhost:8080", "", ZitiConfig{IdentityPath: "id.json"}); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("identity without service error = %v, want set-together error", err)
	}
	if _, err := NewHTTPClientWithZiti("http://localhost:8080", "", ZitiConfig{Service: "svc"}); err == nil || !strings.Contains(err.Error(), "must be set together") {
		t.Fatalf("service without identity error = %v, want set-together error", err)
	}
}

func TestNewHTTPClientWithZitiDialsConfiguredService(t *testing.T) {
	oldNewZitiDialer := newZitiDialer
	fake := &fakeZitiDialer{}
	newZitiDialer = func(identityPath string) (zitiDialer, error) {
		if identityPath != "gb-client.json" {
			t.Fatalf("identity path = %q, want gb-client.json", identityPath)
		}
		return fake, nil
	}
	t.Cleanup(func() {
		newZitiDialer = oldNewZitiDialer
	})

	c, err := NewHTTPClientWithZiti("http://127.0.0.1:8080", "", ZitiConfig{
		IdentityPath: "gb-client.json",
		Service:      "gardenio-beads",
	})
	if err != nil {
		t.Fatalf("NewHTTPClientWithZiti error: %v", err)
	}

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:8080/v1/beads", nil)
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	_, err = c.httpClient.Do(req)
	if err == nil || !strings.Contains(err.Error(), "fake ziti dial") {
		t.Fatalf("Do error = %v, want fake ziti dial", err)
	}
	if fake.serviceName != "gardenio-beads" {
		t.Fatalf("dialed service = %q, want gardenio-beads", fake.serviceName)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close error: %v", err)
	}
	if !fake.closed {
		t.Fatal("Close did not close ziti context")
	}
}
