package mailer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
)

// Config is the mailer's configuration. The zero value is off.
type Config struct {
	// Enabled turns mail on. Off by default: New returns Disabled.
	Enabled bool
	// From is the sender, an address on the operator's verified SES identity,
	// optionally named ("Gardenio Lands <lands@example.com>"). Required when
	// Enabled.
	From string
	// ReplyTo, when set, is where replies go (the support@ address, §6.1).
	ReplyTo string
	// Region is the SES region. Empty uses the AWS default chain.
	Region string
	// ConfigurationSet, when set, names the SES configuration set that
	// routes bounce and complaint events.
	ConfigurationSet string
	// BaseURL is the public origin links in mail point at, for example
	// "https://gardenio.clonk.tv". Required when Enabled.
	BaseURL string
	// CredentialsDir, when set, is a directory holding the mailer's own SES
	// keys as the files accessKeyId and secretAccessKey (a mounted Secret).
	// They sign SES calls in place of the AWS default chain, which on k3s
	// holds the in-box MinIO keys (AWS_ACCESS_KEY_ID/SECRET) for S3. Empty
	// uses the default chain.
	CredentialsDir string
}

// Environment variables ConfigFromEnv reads.
const (
	EnvEnabled          = "MAILER_ENABLED"
	EnvFrom             = "MAILER_FROM"
	EnvReplyTo          = "MAILER_REPLY_TO"
	EnvRegion           = "MAILER_SES_REGION"
	EnvConfigurationSet = "MAILER_SES_CONFIGURATION_SET"
	EnvBaseURL          = "MAILER_BASE_URL"
	EnvCredentialsDir   = "MAILER_SES_CREDENTIALS_DIR"
)

// Files read from Config.CredentialsDir.
const (
	AccessKeyIDFile     = "accessKeyId"
	SecretAccessKeyFile = "secretAccessKey"
)

// ConfigFromEnv reads Config from the MAILER_* environment. An unset or
// unparsable MAILER_ENABLED is off.
func ConfigFromEnv() Config {
	enabled, _ := strconv.ParseBool(strings.TrimSpace(os.Getenv(EnvEnabled)))
	return Config{
		Enabled:          enabled,
		From:             strings.TrimSpace(os.Getenv(EnvFrom)),
		ReplyTo:          strings.TrimSpace(os.Getenv(EnvReplyTo)),
		Region:           strings.TrimSpace(os.Getenv(EnvRegion)),
		ConfigurationSet: strings.TrimSpace(os.Getenv(EnvConfigurationSet)),
		BaseURL:          strings.TrimRight(strings.TrimSpace(os.Getenv(EnvBaseURL)), "/"),
		CredentialsDir:   strings.TrimSpace(os.Getenv(EnvCredentialsDir)),
	}
}

// Validate reports whether an enabled Config can send. A disabled Config is
// always valid.
func (c Config) Validate() error {
	if !c.Enabled {
		return nil
	}
	if c.From == "" {
		return fmt.Errorf("mailer: %s is required when %s is on", EnvFrom, EnvEnabled)
	}
	if err := validateAddress(c.From); err != nil {
		return fmt.Errorf("mailer: %s %q: %v", EnvFrom, c.From, err)
	}
	if c.ReplyTo != "" {
		if err := validateAddress(c.ReplyTo); err != nil {
			return fmt.Errorf("mailer: %s %q: %v", EnvReplyTo, c.ReplyTo, err)
		}
	}
	if !strings.HasPrefix(c.BaseURL, "https://") {
		return fmt.Errorf("mailer: %s must be an https:// origin when %s is on, got %q", EnvBaseURL, EnvEnabled, c.BaseURL)
	}
	return nil
}

// awsConfigTimeout bounds loading the AWS credential chain, so a missing
// IRSA token cannot hang a binary's startup.
const awsConfigTimeout = 10 * time.Second

// New returns the Mailer cfg asks for: Disabled when mail is off, SES when it
// is on. It fails only for an enabled Config that is invalid or whose AWS
// config cannot load.
func New(ctx context.Context, cfg Config) (Mailer, error) {
	if !cfg.Enabled {
		return Disabled{}, nil
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	awsCfg, err := loadAWSConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return NewSES(sesv2.NewFromConfig(awsCfg), cfg), nil
}

// loadAWSConfig is the AWS config SES calls use: the default chain, with the
// static keys from cfg.CredentialsDir in place of its credentials when set.
func loadAWSConfig(ctx context.Context, cfg Config) (aws.Config, error) {
	loadCtx, cancel := context.WithTimeout(ctx, awsConfigTimeout)
	defer cancel()
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	if cfg.CredentialsDir != "" {
		provider, err := staticCredentials(cfg.CredentialsDir)
		if err != nil {
			return aws.Config{}, err
		}
		opts = append(opts, awsconfig.WithCredentialsProvider(provider))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(loadCtx, opts...)
	if err != nil {
		return aws.Config{}, fmt.Errorf("mailer: load AWS config: %w", err)
	}
	return awsCfg, nil
}

// staticCredentials reads the SES key pair from dir. Both files must be
// present and non-empty, so a half-mounted Secret fails startup instead of
// falling back to the default chain's keys.
func staticCredentials(dir string) (aws.CredentialsProvider, error) {
	read := func(name string) (string, error) {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "", fmt.Errorf("mailer: %s: %w", EnvCredentialsDir, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("mailer: %s: %s is empty", EnvCredentialsDir, filepath.Join(dir, name))
		}
		return v, nil
	}
	id, err := read(AccessKeyIDFile)
	if err != nil {
		return nil, err
	}
	secret, err := read(SecretAccessKeyFile)
	if err != nil {
		return nil, err
	}
	return credentials.NewStaticCredentialsProvider(id, secret, ""), nil
}
