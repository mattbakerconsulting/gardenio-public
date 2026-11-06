package server

import "testing"

func TestNewTrustedProxyConfig(t *testing.T) {
	tests := []struct {
		name    string
		cidrs   []string
		wantNil bool
		wantErr bool
	}{
		{name: "nil input", cidrs: nil, wantNil: true},
		{name: "empty input", cidrs: []string{}, wantNil: true},
		{name: "valid CIDR", cidrs: []string{"10.0.0.0/8"}, wantNil: false},
		{name: "multiple CIDRs", cidrs: []string{"10.0.0.0/8", "172.16.0.0/12"}, wantNil: false},
		{name: "bare IPv4", cidrs: []string{"10.0.0.1"}, wantNil: false},
		{name: "bare IPv6", cidrs: []string{"::1"}, wantNil: false},
		{name: "invalid CIDR", cidrs: []string{"not-a-cidr"}, wantErr: true},
		{name: "invalid in list", cidrs: []string{"10.0.0.0/8", "bad"}, wantErr: true},
		{name: "blank entries skipped", cidrs: []string{"", " "}, wantNil: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tp, err := NewTrustedProxyConfig(tt.cidrs)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantNil && tp != nil {
				t.Errorf("expected nil, got %+v", tp)
			}
			if !tt.wantNil && tp == nil {
				t.Error("expected non-nil, got nil")
			}
		})
	}
}

func TestTrustedProxyConfig_IsTrusted(t *testing.T) {
	tp, err := NewTrustedProxyConfig([]string{"10.0.0.0/8", "172.16.0.0/12", "::1/128"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		addr string
		want bool
	}{
		{"10.0.0.1:12345", true},
		{"10.255.255.255:1", true},
		{"172.16.0.1:80", true},
		{"172.31.255.255:443", true},
		{"192.168.1.1:12345", false},
		{"8.8.8.8:53", false},
		{"[::1]:12345", true},
		{"10.0.0.1", true},       // bare IP without port
		{"invalid", false},        // unparseable
	}

	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			got := tp.IsTrusted(tt.addr)
			if got != tt.want {
				t.Errorf("IsTrusted(%q) = %v, want %v", tt.addr, got, tt.want)
			}
		})
	}
}

func TestTrustedProxyConfig_NilTrustsAll(t *testing.T) {
	var tp *TrustedProxyConfig
	if !tp.IsTrusted("1.2.3.4:80") {
		t.Error("nil config should trust all addresses")
	}
	if !tp.IsTrusted("anything") {
		t.Error("nil config should trust all addresses")
	}
}
