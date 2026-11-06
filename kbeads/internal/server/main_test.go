package server

import (
	"os"
	"testing"
)

// TestMain runs the package without the agent pod's picture bucket. Agent
// pods set COOP_S3_BUCKET with file-mounted keys the AWS SDK does not read,
// so every server built in a test would fall through to instance metadata,
// which pods cannot reach, and wait out its retries (gd-Juj9k0iaEe). No test
// here reads pictures; landmap's own tests cover PicturesFromEnv.
func TestMain(m *testing.M) {
	os.Unsetenv("COOP_S3_BUCKET")
	os.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	os.Exit(m.Run())
}
