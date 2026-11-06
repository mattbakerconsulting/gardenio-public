package oidclogin

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// machineIssuer is a fake Keycloak that issues "machine-tok" for client
// "coord" with secret "s3cret" and refuses any other pair.
func machineIssuer(t *testing.T) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": srv.URL, "token_endpoint": srv.URL + "/token"})
		case "/token":
			_ = r.ParseForm()
			if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("client_id") != "coord" || r.PostForm.Get("client_secret") != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized_client"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "machine-tok", "expires_in": 300})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func setMachineEnv(t *testing.T, issuer, clientID, secret string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(secret+"\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv(IssuerEnv, issuer)
	t.Setenv(MachineClientIDEnv, clientID)
	t.Setenv(MachineClientSecretFileEnv, path)
}

func TestMachineTokenMintsFromSecretFile(t *testing.T) {
	srv := machineIssuer(t)
	setMachineEnv(t, srv.URL, "coord", "s3cret")
	tok, err := MachineToken(context.Background(), srv.Client())
	if err != nil || tok != "machine-tok" {
		t.Fatalf("MachineToken = %q, %v; want machine-tok", tok, err)
	}
}

// A refused grant is an error that names the client but carries neither the
// secret nor any token.
func TestMachineTokenRefusedKeepsSecretOut(t *testing.T) {
	srv := machineIssuer(t)
	setMachineEnv(t, srv.URL, "coord", "wrong-secret")
	tok, err := MachineToken(context.Background(), srv.Client())
	if err == nil || tok != "" {
		t.Fatalf("MachineToken = %q, %v; want a refusal", tok, err)
	}
	if strings.Contains(err.Error(), "wrong-secret") {
		t.Fatalf("error leaks the secret: %v", err)
	}
	if !strings.Contains(err.Error(), "coord") {
		t.Fatalf("error %q should name the client", err)
	}
}

func TestMachineTokenNeedsEnv(t *testing.T) {
	t.Setenv(MachineClientIDEnv, "")
	t.Setenv(MachineClientSecretFileEnv, "")
	if _, err := MachineToken(context.Background(), http.DefaultClient); err == nil || !strings.Contains(err.Error(), MachineClientIDEnv) {
		t.Fatalf("MachineToken without env = %v; want an error naming %s", err, MachineClientIDEnv)
	}
}

// An agent pod cannot dial the public issuer, so it fetches discovery at the
// in-cluster Service (DiscoveryURLEnv) and posts to the token endpoint that
// document names; the document must still name the public issuer.
func TestMachineTokenDiscoversAtServiceURL(t *testing.T) {
	const public = "https://auth.example.test/auth/realms/gardenio"
	srv := servedIssuer(t, httptest.NewServer, public)
	setMachineEnv(t, public, "coord", "s3cret")
	t.Setenv(DiscoveryURLEnv, srv.URL)
	tok, err := MachineToken(context.Background(), srv.Client())
	if err != nil || tok != "machine-tok" {
		t.Fatalf("MachineToken = %q, %v; want machine-tok", tok, err)
	}
}

func TestMachineTokenRejectsServiceDocForOtherIssuer(t *testing.T) {
	srv := servedIssuer(t, httptest.NewServer, "https://evil.example.test/realms/x")
	setMachineEnv(t, "https://auth.example.test/auth/realms/gardenio", "coord", "s3cret")
	t.Setenv(DiscoveryURLEnv, srv.URL)
	var de *DiscoveryDocError
	if _, err := MachineToken(context.Background(), srv.Client()); !errors.As(err, &de) {
		t.Fatalf("MachineToken = %v; want a DiscoveryDocError", err)
	}
}

// CAFileEnv adds the Service's CA to an otherwise default client, as on a pod
// that mounts only the keycloak-runtime ca.crt.
func TestMachineTokenTrustsCAFile(t *testing.T) {
	const public = "https://auth.example.test/auth/realms/gardenio"
	srv := servedIssuer(t, httptest.NewTLSServer, public)
	setMachineEnv(t, public, "coord", "s3cret")
	t.Setenv(DiscoveryURLEnv, srv.URL)
	if _, err := MachineToken(context.Background(), &http.Client{}); err == nil {
		t.Fatal("MachineToken trusted the test CA without CAFileEnv")
	}
	ca := filepath.Join(t.TempDir(), "ca.crt")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o444); err != nil {
		t.Fatal(err)
	}
	t.Setenv(CAFileEnv, ca)
	tok, err := MachineToken(context.Background(), &http.Client{})
	if err != nil || tok != "machine-tok" {
		t.Fatalf("MachineToken with %s = %q, %v; want machine-tok", CAFileEnv, tok, err)
	}
}

// servedIssuer is machineIssuer behind a Service URL: its discovery document
// names issuer, not the URL it is served at, and points at its own /token.
func servedIssuer(t *testing.T, start func(http.Handler) *httptest.Server, issuer string) *httptest.Server {
	t.Helper()
	var srv *httptest.Server
	srv = start(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": issuer, "token_endpoint": srv.URL + "/token"})
		case "/token":
			_ = r.ParseForm()
			if r.PostForm.Get("client_id") != "coord" || r.PostForm.Get("client_secret") != "s3cret" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "machine-tok", "expires_in": 300})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}
