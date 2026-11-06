package beadsapi

import "strings"

// AwaitsReleaseField marks a task whose PR has merged and which the deployment
// record of the first release that carries it closes (kbeads
// releases_awaits.go, gd-ZM7NEBPAqZ). It is a gate the reopen paths read, not a
// reopen-accounting field they write, so it lives outside reopen.go.
const AwaitsReleaseField = "awaits_release"

// AwaitsRelease reports whether a task is parked behind its release. Such a
// task is done, not stranded: neither reopen path returns it to the ready
// queue when its agent dies (gd-aaGqLb2cUk).
func AwaitsRelease(fields map[string]string) bool {
	return strings.EqualFold(strings.TrimSpace(fields[AwaitsReleaseField]), "true")
}
