//go:build darwin

package credstore

import "testing"

func TestParseDefaultKeychainPath(t *testing.T) {
	tests := []struct {
		name   string
		output string
		want   string
	}{
		{
			name:   "security quoted output",
			output: "    \"/Users/operator/Library/Keychains/login.keychain-db\"\n",
			want:   "/Users/operator/Library/Keychains/login.keychain-db",
		},
		{
			name:   "unquoted output",
			output: "/Users/operator/Library/Keychains/login.keychain-db\n",
			want:   "/Users/operator/Library/Keychains/login.keychain-db",
		},
		{name: "empty"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseDefaultKeychainPath(tc.output); got != tc.want {
				t.Errorf("parseDefaultKeychainPath(%q)=%q, want %q", tc.output, got, tc.want)
			}
		})
	}
}
