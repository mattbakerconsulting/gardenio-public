package client

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/openziti/sdk-golang/ziti"
)

// ZitiConfig configures optional OpenZiti dialing for the HTTP client.
// BaseURL remains the logical HTTP origin; Ziti only replaces TCP dialing.
type ZitiConfig struct {
	IdentityPath  string
	Service       string
	TLSServerName string
}

type zitiDialer interface {
	DialContext(ctx context.Context, serviceName string) (net.Conn, error)
	Close()
}

var newZitiDialer = func(identityPath string) (zitiDialer, error) {
	cfg, err := ziti.NewConfigFromFile(identityPath)
	if err != nil {
		return nil, err
	}
	ztx, err := ziti.NewContext(cfg)
	if err != nil {
		return nil, err
	}
	return zitiContextAdapter{Context: ztx}, nil
}

type zitiContextAdapter struct {
	ziti.Context
}

func (a zitiContextAdapter) DialContext(ctx context.Context, serviceName string) (net.Conn, error) {
	return a.Context.DialContext(ctx, serviceName)
}

// NewHTTPClientWithZiti creates an HTTP client that reaches baseURL through an
// OpenZiti service while preserving baseURL as the logical request origin.
func NewHTTPClientWithZiti(baseURL, token string, cfg ZitiConfig) (*HTTPClient, error) {
	inner, ztx, err := newZitiTransport(cfg)
	if err != nil {
		return nil, err
	}
	return newHTTPClient(baseURL, token, inner, ztx), nil
}

func newZitiTransport(cfg ZitiConfig) (http.RoundTripper, zitiDialer, error) {
	identityPath := strings.TrimSpace(cfg.IdentityPath)
	serviceName := strings.TrimSpace(cfg.Service)
	tlsServerName := strings.TrimSpace(cfg.TLSServerName)
	if identityPath == "" && serviceName == "" {
		return http.DefaultTransport, nil, nil
	}
	if identityPath == "" || serviceName == "" {
		return nil, nil, fmt.Errorf("ziti identity and ziti service must be set together")
	}

	ztx, err := newZitiDialer(identityPath)
	if err != nil {
		return nil, nil, fmt.Errorf("initializing ziti transport: %w", err)
	}

	base, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		ztx.Close()
		return nil, nil, fmt.Errorf("initializing ziti transport: default transport has type %T", http.DefaultTransport)
	}
	tr := base.Clone()
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		return ztx.DialContext(ctx, serviceName)
	}
	if tlsServerName != "" {
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		tr.TLSClientConfig.ServerName = tlsServerName
	}
	return tr, ztx, nil
}
