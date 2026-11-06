package server

import (
	"encoding/json"
	"net/url"
	"sort"
	"strings"

	"gardenio/kbeads/internal/model"
)

// Operator orgs are never Lands pins (gd-72m3STiqya, review of #737). A
// project that mints with the Lands App (landRepoPinned: a tenant land, or
// github_app=lands) must never be bound to a repo on one of the operator's
// own GitHub accounts, or the Lands App, installed there, would hand a land a
// token for operator code. Only an admin or the Lands provisioner can write
// the pin (land_switch_authz.go), so this guards against their mistake, not a
// tenant: it refuses everyone, admin and approved replay included.
//
// The daemon cannot see an installation's account without asking GitHub, so
// it refuses on the repo owner: git_url and every repos entry. The controller
// requires the installation to be on the repo owner's account and refuses an
// operator org again at mint and refresh time (verifyLandsInstallation,
// cmd/controller/github_app_credentials.go). The list is
// BEADS_LANDS_OPERATOR_GITHUB_ORGS (chart agents.lands.operatorGithubOrgs);
// empty checks nothing.

// landOperatorOrgKeys are the project fields that can bind a Lands-pinned
// project to a repo, or make a project Lands-pinned.
var landOperatorOrgKeys = map[string]bool{
	"github_app":             true,
	"github_installation_id": true,
	"git_url":                true,
	"repos":                  true,
	"land_kind":              true,
}

// SetLandsOperatorGitHubOrgs sets the operator's GitHub accounts, matched
// case-insensitively.
func (s *BeadsServer) SetLandsOperatorGitHubOrgs(orgs []string) {
	set := make(map[string]bool, len(orgs))
	for _, o := range orgs {
		if o = strings.ToLower(strings.TrimSpace(o)); o != "" {
			set[o] = true
		}
	}
	s.landsOperatorOrgs = set
}

// denyOperatorOrgPin refuses a type:project create (existing == nil) or
// update whose resulting fields are Lands-pinned on a repo an operator org
// owns. It runs only when the patch changes one of landOperatorOrgKeys, so a
// land already in that state can still be frozen or edited otherwise.
func (s *BeadsServer) denyOperatorOrgPin(action string, beadType model.BeadType, patch, existing json.RawMessage) *AuthzDenial {
	if beadType != "project" || len(s.landsOperatorOrgs) == 0 || len(patch) == 0 {
		return nil
	}
	var p, merged map[string]any
	if json.Unmarshal(patch, &p) != nil {
		return nil
	}
	if len(existing) > 0 {
		_ = json.Unmarshal(existing, &merged)
	}
	if merged == nil {
		merged = map[string]any{}
	}
	touched := false
	for k, v := range p {
		if landOperatorOrgKeys[k] && channelBindingValue(v) != channelBindingValue(merged[k]) {
			touched = true
		}
		if v == nil { // merge-patch: null deletes (updateBead)
			delete(merged, k)
			continue
		}
		merged[k] = v
	}
	if !touched || !landRepoPinned(merged) {
		return nil
	}
	if owners := s.operatorOrgRepoOwners(merged); len(owners) > 0 {
		return operatorOrgPinDenial(action, beadType, "its git_url or repos are on operator GitHub account "+strings.Join(owners, ", "))
	}
	return nil
}

// denyOperatorOrgPinKey is denyOperatorOrgPin for the single-field mutation
// endpoints and the extra-fields merge, where only the key is known: a key
// that could bind a Lands-pinned project to a repo is refused there for
// everyone while an operator-org list is set. Write it with an update, whose
// value is checked.
func (s *BeadsServer) denyOperatorOrgPinKey(action string, bead *model.Bead, keys ...string) *AuthzDenial {
	if bead == nil || bead.Type != "project" || len(s.landsOperatorOrgs) == 0 {
		return nil
	}
	var stored map[string]any
	_ = json.Unmarshal(bead.Fields, &stored)
	pinned := landRepoPinned(stored)
	var hit []string
	for _, k := range keys {
		if landMintFields[k] || k == "land_kind" || (landRepoFields[k] && pinned) {
			hit = append(hit, k)
		}
	}
	if len(hit) == 0 {
		return nil
	}
	sort.Strings(hit)
	return operatorOrgPinDenial(action, bead.Type, "set "+strings.Join(hit, ", ")+" without an update (the value cannot be checked against the operator GitHub orgs)")
}

func operatorOrgPinDenial(action string, beadType model.BeadType, why string) *AuthzDenial {
	return &AuthzDenial{
		Action:   action,
		BeadType: string(beadType),
		Message: "refused: " + action + " on type " + string(beadType) + " would bind a land that mints with the Lands GitHub App to an operator org: " + why +
			"; a Lands installation on an operator org is never pinned (BEADS_LANDS_OPERATOR_GITHUB_ORGS)",
		AdvisoryText: "A tenant land, or a project with github_app=lands, may not use a repo on the operator's own GitHub accounts.",
	}
}

// operatorOrgRepoOwners returns the sorted operator orgs that own the
// project's git_url or any repos entry.
func (s *BeadsServer) operatorOrgRepoOwners(fields map[string]any) []string {
	urls := []string{channelBindingValue(fields["git_url"])}
	urls = append(urls, projectRepoURLs(fields["repos"])...)
	seen := map[string]bool{}
	var owners []string
	for _, u := range urls {
		o := githubRepoOwner(u)
		if o != "" && s.landsOperatorOrgs[o] && !seen[o] {
			seen[o] = true
			owners = append(owners, o)
		}
	}
	sort.Strings(owners)
	return owners
}

// projectRepoURLs reads the url of each repos entry ([{url, role, branch}]),
// stored as a JSON array or as a string holding one.
func projectRepoURLs(v any) []string {
	if s, ok := v.(string); ok {
		var parsed any
		if json.Unmarshal([]byte(s), &parsed) != nil {
			return nil
		}
		v = parsed
	}
	list, _ := v.([]any)
	var urls []string
	for _, e := range list {
		switch t := e.(type) {
		case map[string]any:
			urls = append(urls, channelBindingValue(t["url"]))
		case string:
			urls = append(urls, t)
		}
	}
	return urls
}

// githubHosts are the hosts git reaches a github.com repo on: the web host,
// its www alias and ssh.github.com (SSH over port 443).
var githubHosts = map[string]bool{"github.com": true, "www.github.com": true, "ssh.github.com": true}

// githubRepoOwner returns the lower-cased owner of a github.com repo URL, or
// "" when u is not a github.com repo. It reads every form git accepts: a URL
// with a scheme (https://, ssh://, git://, git+ssh://), scp-style
// [user@]github.com:owner/repo and scheme-less github.com/owner/repo, on any
// of githubHosts. A form it misread as "" would skip the operator-org guard
// (gd-i74esUHCTg).
func githubRepoOwner(u string) string {
	u = strings.TrimSpace(u)
	var host, path string
	before, after, colon := strings.Cut(u, ":")
	switch {
	case strings.Contains(u, "://"):
		pu, err := url.Parse(u)
		if err != nil {
			return ""
		}
		host, path = pu.Hostname(), pu.Path
	case colon && !strings.Contains(before, "/"):
		// scp-style: git treats host:path as SSH when no slash precedes the colon.
		host, path = before, after
	default:
		host, path, _ = strings.Cut(u, "/")
	}
	if i := strings.LastIndex(host, "@"); i >= 0 {
		host = host[i+1:]
	}
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if !githubHosts[host] {
		return ""
	}
	owner, _, _ := strings.Cut(strings.TrimLeft(path, "/"), "/")
	return strings.ToLower(owner)
}
