package bridge

import (
	"regexp"
	"strconv"
)

// prURLPattern matches a GitHub pull request URL: host, owner/repo, number.
// Trailing path/query/fragment (/files, ?diff=split, #issuecomment-1) is allowed.
// A GitLab MR URL (/-/merge_requests/) never matches.
var prURLPattern = regexp.MustCompile(`^https?://([^/]+)/([^/]+/[^/]+)/pull/(\d+)(?:[/?#].*)?$`)

// ParsePRURL extracts the host, owner/repo and pull request number from a
// GitHub PR URL, reusing MRRef (ProjectPath = owner/repo, IID = PR number).
// Returns nil if the URL is not a GitHub pull request URL.
func ParsePRURL(rawURL string) *MRRef {
	m := prURLPattern.FindStringSubmatch(rawURL)
	if m == nil {
		return nil
	}
	n, err := strconv.Atoi(m[3])
	if err != nil || n <= 0 {
		return nil
	}
	return &MRRef{Host: m[1], ProjectPath: m[2], IID: n}
}
