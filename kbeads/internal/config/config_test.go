package config

import (
	"strings"
	"testing"
	"time"
)

// syncEnvVars lists all sync-related env vars that must be cleared between tests.
var syncEnvVars = []string{
	"BEADS_SYNC_INTERVAL", "BEADS_SYNC_S3_BUCKET", "BEADS_SYNC_S3_ENDPOINT",
	"BEADS_SYNC_S3_REGION", "BEADS_SYNC_S3_KEY", "BEADS_SYNC_GIT_REPO",
	"BEADS_SYNC_GIT_FILE", "BEADS_SYNC_GIT_BRANCH",
}

func clearAllEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"BEADS_DATABASE_URL", "BEADS_GRPC_ADDR", "BEADS_HTTP_ADDR", "BEADS_NATS_URL"} {
		t.Setenv(key, "")
	}
	for _, key := range syncEnvVars {
		t.Setenv(key, "")
	}
}

func TestLoad(t *testing.T) {
	for _, tc := range []struct {
		name         string
		env          map[string]string
		wantErr      bool
		wantGRPCAddr string
		wantHTTPAddr string
		wantNATSURL  string
	}{
		{
			name:    "MissingDatabaseURL",
			env:     map[string]string{},
			wantErr: true,
		},
		{
			name:         "DefaultAddresses",
			env:          map[string]string{"BEADS_DATABASE_URL": "postgres://localhost/beads"},
			wantGRPCAddr: ":9090",
			wantHTTPAddr: ":8080",
		},
		{
			name: "CustomAddresses",
			env: map[string]string{
				"BEADS_DATABASE_URL": "postgres://db:5432/beads",
				"BEADS_GRPC_ADDR":   ":5050",
				"BEADS_HTTP_ADDR":   ":3000",
				"BEADS_NATS_URL":    "nats://localhost:4222",
			},
			wantGRPCAddr: ":5050",
			wantHTTPAddr: ":3000",
			wantNATSURL:  "nats://localhost:4222",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAllEnv(t)
			for k, v := range tc.env {
				t.Setenv(k, v)
			}

			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.DatabaseURL != tc.env["BEADS_DATABASE_URL"] {
				t.Errorf("DatabaseURL = %q, want %q", cfg.DatabaseURL, tc.env["BEADS_DATABASE_URL"])
			}
			if cfg.GRPCAddr != tc.wantGRPCAddr {
				t.Errorf("GRPCAddr = %q, want %q", cfg.GRPCAddr, tc.wantGRPCAddr)
			}
			if cfg.HTTPAddr != tc.wantHTTPAddr {
				t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, tc.wantHTTPAddr)
			}
			if cfg.NATSURL != tc.wantNATSURL {
				t.Errorf("NATSURL = %q, want %q", cfg.NATSURL, tc.wantNATSURL)
			}
		})
	}
}

func TestLoadDatabasePasswordFile(t *testing.T) {
	// Default: unset -> empty (legacy env-var mode, no behavior change).
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabasePasswordFile != "" {
		t.Errorf("DatabasePasswordFile default = %q, want empty", cfg.DatabasePasswordFile)
	}

	// Set: threads BEADS_DATABASE_PASSWORD_FILE through to config (ka-0000004150).
	t.Setenv("BEADS_DATABASE_PASSWORD_FILE", "/var/run/db-secret/password")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.DatabasePasswordFile != "/var/run/db-secret/password" {
		t.Errorf("DatabasePasswordFile = %q, want /var/run/db-secret/password", cfg.DatabasePasswordFile)
	}
}

func TestLoadMachineClients(t *testing.T) {
	// Unset -> empty: the machine door stays closed (gd-gQ4iLIpWCm).
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	t.Setenv("BEADS_MACHINE_CLIENTS", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.MachineClients) != 0 {
		t.Errorf("MachineClients default = %v, want empty", cfg.MachineClients)
	}

	t.Setenv("BEADS_MACHINE_CLIENTS", "a, b")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := strings.Join(cfg.MachineClients, ","); got != "a,b" || len(cfg.MachineClients) != 2 {
		t.Errorf("MachineClients = %v, want [a b]", cfg.MachineClients)
	}
}

func TestLoadSyncDefaults(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SyncInterval != 3*time.Minute {
		t.Errorf("SyncInterval = %v, want 3m", cfg.SyncInterval)
	}
	if cfg.SyncS3Region != "us-east-1" {
		t.Errorf("SyncS3Region = %q, want %q", cfg.SyncS3Region, "us-east-1")
	}
	if cfg.SyncS3Key != "beads/backup.jsonl" {
		t.Errorf("SyncS3Key = %q, want %q", cfg.SyncS3Key, "beads/backup.jsonl")
	}
	if cfg.SyncGitFile != "beads.jsonl" {
		t.Errorf("SyncGitFile = %q, want %q", cfg.SyncGitFile, "beads.jsonl")
	}
	if cfg.SyncGitBranch != "main" {
		t.Errorf("SyncGitBranch = %q, want %q", cfg.SyncGitBranch, "main")
	}
}

func TestLoadSyncCustom(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	t.Setenv("BEADS_SYNC_INTERVAL", "10m")
	t.Setenv("BEADS_SYNC_S3_BUCKET", "my-bucket")
	t.Setenv("BEADS_SYNC_S3_ENDPOINT", "http://minio:9000")
	t.Setenv("BEADS_SYNC_S3_REGION", "eu-west-1")
	t.Setenv("BEADS_SYNC_S3_KEY", "custom/key.jsonl")
	t.Setenv("BEADS_SYNC_GIT_REPO", "/tmp/repo")
	t.Setenv("BEADS_SYNC_GIT_FILE", "custom.jsonl")
	t.Setenv("BEADS_SYNC_GIT_BRANCH", "backup")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SyncInterval != 10*time.Minute {
		t.Errorf("SyncInterval = %v, want 10m", cfg.SyncInterval)
	}
	if cfg.SyncS3Bucket != "my-bucket" {
		t.Errorf("SyncS3Bucket = %q", cfg.SyncS3Bucket)
	}
	if cfg.SyncS3Endpoint != "http://minio:9000" {
		t.Errorf("SyncS3Endpoint = %q", cfg.SyncS3Endpoint)
	}
	if cfg.SyncS3Region != "eu-west-1" {
		t.Errorf("SyncS3Region = %q", cfg.SyncS3Region)
	}
	if cfg.SyncS3Key != "custom/key.jsonl" {
		t.Errorf("SyncS3Key = %q", cfg.SyncS3Key)
	}
	if cfg.SyncGitRepo != "/tmp/repo" {
		t.Errorf("SyncGitRepo = %q", cfg.SyncGitRepo)
	}
	if cfg.SyncGitFile != "custom.jsonl" {
		t.Errorf("SyncGitFile = %q", cfg.SyncGitFile)
	}
	if cfg.SyncGitBranch != "backup" {
		t.Errorf("SyncGitBranch = %q", cfg.SyncGitBranch)
	}
}

func TestLoadSyncInvalidInterval(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	t.Setenv("BEADS_SYNC_INTERVAL", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid BEADS_SYNC_INTERVAL")
	}
}

func TestLoadSyncDisabled(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	t.Setenv("BEADS_SYNC_INTERVAL", "0s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.SyncInterval != 0 {
		t.Errorf("SyncInterval = %v, want 0 (disabled)", cfg.SyncInterval)
	}
}

func TestLoadOIDCAudienceRequired(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     map[string]string
		wantErr bool
	}{
		{
			name: "IssuerURL without audience fails",
			env: map[string]string{
				"BEADS_DATABASE_URL":      "postgres://localhost/beads",
				"BEADS_OIDC_ISSUER_URL":   "https://keycloak.example.com/realms/test",
			},
			wantErr: true,
		},
		{
			name: "IssuerURLs without audience fails",
			env: map[string]string{
				"BEADS_DATABASE_URL":       "postgres://localhost/beads",
				"BEADS_OIDC_ISSUER_URLS":   "https://keycloak.example.com/realms/test",
			},
			wantErr: true,
		},
		{
			name: "IssuerURL with audience succeeds",
			env: map[string]string{
				"BEADS_DATABASE_URL":      "postgres://localhost/beads",
				"BEADS_OIDC_ISSUER_URL":   "https://keycloak.example.com/realms/test",
				"BEADS_OIDC_AUDIENCE":     "kbeads",
			},
			wantErr: false,
		},
		{
			name: "No OIDC config succeeds without audience",
			env: map[string]string{
				"BEADS_DATABASE_URL": "postgres://localhost/beads",
			},
			wantErr: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAllEnv(t)
			// Clear OIDC env vars too.
			for _, key := range []string{
				"BEADS_OIDC_ISSUER_URL", "BEADS_OIDC_ISSUER_URLS",
				"BEADS_OIDC_AUDIENCE", "BEADS_OIDC_ALLOWED_EMAIL_DOMAINS",
			} {
				t.Setenv(key, "")
			}
			for k, v := range tc.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				if !strings.Contains(err.Error(), "BEADS_OIDC_AUDIENCE") {
					t.Errorf("error = %q, want it to mention BEADS_OIDC_AUDIENCE", err.Error())
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadEmbedDefaults(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	for _, key := range []string{"BEADS_EMBED_BACKEND", "BEADS_EMBED_URL", "BEADS_EMBED_API_KEY", "BEADS_EMBED_MODEL"} {
		t.Setenv(key, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EmbedBackend != "noop" {
		t.Errorf("EmbedBackend = %q, want %q", cfg.EmbedBackend, "noop")
	}
	if cfg.EmbedURL != "http://localhost:11434" {
		t.Errorf("EmbedURL = %q, want %q", cfg.EmbedURL, "http://localhost:11434")
	}
	if cfg.EmbedAPIKey != "" {
		t.Errorf("EmbedAPIKey = %q, want empty", cfg.EmbedAPIKey)
	}
	if cfg.EmbedModel != "" {
		t.Errorf("EmbedModel = %q, want empty", cfg.EmbedModel)
	}
}

func TestLoadEmbedCustom(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
	t.Setenv("BEADS_EMBED_BACKEND", "openai")
	t.Setenv("BEADS_EMBED_URL", "http://litellm.svc:4000")
	t.Setenv("BEADS_EMBED_API_KEY", "secret-key")
	t.Setenv("BEADS_EMBED_MODEL", "bedrock-titan-embed-v2")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.EmbedBackend != "openai" {
		t.Errorf("EmbedBackend = %q", cfg.EmbedBackend)
	}
	if cfg.EmbedURL != "http://litellm.svc:4000" {
		t.Errorf("EmbedURL = %q", cfg.EmbedURL)
	}
	if cfg.EmbedAPIKey != "secret-key" {
		t.Errorf("EmbedAPIKey = %q", cfg.EmbedAPIKey)
	}
	if cfg.EmbedModel != "bedrock-titan-embed-v2" {
		t.Errorf("EmbedModel = %q", cfg.EmbedModel)
	}
}

func TestEnvOrDefault(t *testing.T) {
	for _, tc := range []struct {
		name     string
		key      string
		envVal   string
		fallback string
		want     string
	}{
		{"EmptyUsesDefault", "TEST_ENVDEFAULT_EMPTY", "", "default-val", "default-val"},
		{"SetUsesEnv", "TEST_ENVDEFAULT_SET", "custom", "default-val", "custom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(tc.key, tc.envVal)
			got := envOrDefault(tc.key, tc.fallback)
			if got != tc.want {
				t.Errorf("envOrDefault(%q, %q) = %q, want %q", tc.key, tc.fallback, got, tc.want)
			}
		})
	}
}

// TestLoadServiceFieldPolicy pins the env plumbing for the trigger/schedule
// service-managed-field bypass policy (ka-0000006237).
func TestLoadServiceFieldPolicy(t *testing.T) {
	clearAllEnv(t)
	t.Setenv("BEADS_DATABASE_URL", "postgres://test")

	t.Run("defaults", func(t *testing.T) {
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(c.ServiceFieldFleets) != 0 {
			t.Errorf("ServiceFieldFleets default = %v, want empty (permissive)", c.ServiceFieldFleets)
		}
		if c.ServiceFieldBypassRequireProject {
			t.Error("ServiceFieldBypassRequireProject must default off")
		}
	})

	t.Run("set", func(t *testing.T) {
		t.Setenv("BEADS_SERVICE_FIELD_FLEETS", "jira-bridge, slack-bridge ,")
		t.Setenv("BEADS_SERVICE_FIELD_BYPASS_REQUIRE_PROJECT", "true")
		c, err := Load()
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if len(c.ServiceFieldFleets) != 2 || c.ServiceFieldFleets[0] != "jira-bridge" || c.ServiceFieldFleets[1] != "slack-bridge" {
			t.Errorf("ServiceFieldFleets = %v, want [jira-bridge slack-bridge]", c.ServiceFieldFleets)
		}
		if !c.ServiceFieldBypassRequireProject {
			t.Error("ServiceFieldBypassRequireProject must parse true")
		}
	})
}

func TestLoadLandsOperatorGitHubOrgs(t *testing.T) {
	t.Setenv("BEADS_DATABASE_URL", "postgres://x")
	t.Setenv("BEADS_LANDS_OPERATOR_GITHUB_ORGS", "")
	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(c.LandsOperatorGitHubOrgs) != 0 {
		t.Errorf("LandsOperatorGitHubOrgs default = %v, want empty", c.LandsOperatorGitHubOrgs)
	}
	t.Setenv("BEADS_LANDS_OPERATOR_GITHUB_ORGS", "mattbakerconsulting, claimlayer ,")
	if c, err = Load(); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := c.LandsOperatorGitHubOrgs; len(got) != 2 || got[0] != "mattbakerconsulting" || got[1] != "claimlayer" {
		t.Errorf("LandsOperatorGitHubOrgs = %v, want [mattbakerconsulting claimlayer]", got)
	}
}

func TestLoadHistoryRetention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		want    int
		wantErr bool
	}{
		{"default", "", 14, false},
		{"custom", "30", 30, false},
		{"disabled", "0", 0, false},
		{"invalid", "abc", 0, true},
		{"negative", "-5", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAllEnv(t)
			t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
			if tc.env != "" {
				t.Setenv("BEADS_HISTORY_RETENTION_DAYS", tc.env)
			}
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.HistoryRetentionDays != tc.want {
				t.Errorf("HistoryRetentionDays = %d, want %d", cfg.HistoryRetentionDays, tc.want)
			}
		})
	}
}

func TestLoadSlowToolCallsRetention(t *testing.T) {
	for _, tc := range []struct {
		name    string
		env     string
		want    int
		wantErr bool
	}{
		{"default", "", 60, false},
		{"custom", "30", 30, false},
		{"disabled", "0", 0, false},
		{"invalid", "abc", 0, true},
		{"negative", "-5", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearAllEnv(t)
			t.Setenv("BEADS_DATABASE_URL", "postgres://localhost/beads")
			if tc.env != "" {
				t.Setenv("BEADS_SLOW_TOOL_CALLS_RETENTION_DAYS", tc.env)
			}
			cfg, err := Load()
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.SlowToolCallsRetentionDays != tc.want {
				t.Errorf("SlowToolCallsRetentionDays = %d, want %d", cfg.SlowToolCallsRetentionDays, tc.want)
			}
		})
	}
}
