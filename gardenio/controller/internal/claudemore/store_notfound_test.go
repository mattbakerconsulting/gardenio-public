package claudemore

import (
	"errors"
	"testing"

	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// apiErr is a minimal smithy.APIError for exercising the code-based not-found
// branch (some S3-compatible endpoints surface NoSuchKey/NotFound as a bare
// error code rather than a typed exception).
type apiErr struct{ code string }

func (e apiErr) Error() string                 { return e.code }
func (e apiErr) ErrorCode() string             { return e.code }
func (e apiErr) ErrorMessage() string          { return e.code }
func (e apiErr) ErrorFault() smithy.ErrorFault { return smithy.FaultServer }

func TestIsNotFound(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"typed NoSuchKey", &s3types.NoSuchKey{}, true},
		{"typed NotFound", &s3types.NotFound{}, true},
		{"code NoSuchKey", apiErr{code: "NoSuchKey"}, true},
		{"code NotFound", apiErr{code: "NotFound"}, true},
		{"code AccessDenied", apiErr{code: "AccessDenied"}, false},
		{"generic error", errors.New("boom"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isNotFound(c.err); got != c.want {
				t.Errorf("isNotFound(%v) = %v, want %v", c.err, got, c.want)
			}
		})
	}
}

// TestSanitizeModelSegment locks the model→path-segment mapping.
func TestSanitizeModelSegment(t *testing.T) {
	cases := map[string]string{
		"":                        "_unversioned",
		"   ":                     "_unversioned",
		"claude-sonnet-5":         "claude-sonnet-5",
		"anthropic/claude-x:beta": "anthropic_claude-x_beta",
		"a b c":                   "a_b_c",
		"v1.2_3":                  "v1.2_3",
	}
	for in, want := range cases {
		if got := sanitizeModelSegment(in); got != want {
			t.Errorf("sanitizeModelSegment(%q) = %q, want %q", in, got, want)
		}
	}
}
