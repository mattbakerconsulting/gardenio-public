package bridge

import "testing"

func TestParsePRURL(t *testing.T) {
	tests := []struct {
		url      string
		wantPath string
		wantIID  int
		wantNil  bool
	}{
		{url: "https://github.com/org/repo/pull/42", wantPath: "org/repo", wantIID: 42},
		{url: "https://github.com/org/repo/pull/42/files", wantPath: "org/repo", wantIID: 42},
		{url: "https://github.com/org/repo/pull/42#issuecomment-1", wantPath: "org/repo", wantIID: 42},
		{url: "https://gitlab.com/org/repo/-/merge_requests/42", wantNil: true},
		{url: "https://github.com/org/repo/issues/42", wantNil: true},
		{url: "https://github.com/org/repo/pull/0", wantNil: true},
		{url: "https://github.com/org/repo/pull/abc", wantNil: true},
		{url: "", wantNil: true},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			ref := ParsePRURL(tt.url)
			if tt.wantNil {
				if ref != nil {
					t.Fatalf("expected nil, got %+v", ref)
				}
				return
			}
			if ref == nil {
				t.Fatal("expected non-nil ref")
			}
			if ref.Host != "github.com" || ref.ProjectPath != tt.wantPath || ref.IID != tt.wantIID {
				t.Errorf("got %+v", ref)
			}
		})
	}
}
