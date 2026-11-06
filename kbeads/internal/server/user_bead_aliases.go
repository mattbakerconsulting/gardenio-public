package server

import (
	"encoding/json"
	"strings"
)

// userBeadAliasRefs derives the operator-alias (kind, subject) tuples a
// type:user bead's identity fields stand for. It is the daemon-side half of
// the user-bead alias bridge (gd-uAvleAZUvV): the controller bridge only says
// "resync bead X" and the daemon reads the aliases here, from the stored bead,
// so a machine caller can never choose them. The fields it reads are exactly
// the ones userIdentityFields gates behind an OIDC gardenio-admin.
//
// `email_aliases` is deliberately NOT read here (gd-L55f9EYFjN): one person is
// one identity and an email is a label, not a credential. The field survives
// on the bead as an admin-written display value, but an address listed in it
// never becomes an oidc alias, so it cannot sign in as, link to, be attributed
// as, or approve as the bead's person.
//
//   - oidc   <- email
//   - slack  <- slack_id (fallback slack_user_id)
//   - gitlab <- gitlab_username
//   - jira   <- jira_user_id (fallback jira_user)
//   - github <- github_username
//
// github is emitted LAST: deriveOperatorEmail has no github case, so a github
// alias can only attach to an operator that already exists and must never be
// the seed link-aliases creates a new operator from. An oidc subject must be an
// email ('@' present, no ':'); entries that are not are dropped. Empty values
// and duplicates are dropped; oidc, gitlab and github subjects are lowercased.
func userBeadAliasRefs(fields json.RawMessage) []OperatorAliasRef {
	if len(fields) == 0 {
		return nil
	}
	var f map[string]any
	if err := json.Unmarshal(fields, &f); err != nil {
		return nil
	}
	var refs []OperatorAliasRef
	seen := map[string]bool{}
	add := func(kind, subject string) {
		subject = canonicalAliasSubject(kind, subject)
		if kind == "oidc" && (!strings.Contains(subject, "@") || strings.Contains(subject, ":")) {
			return
		}
		if subject == "" || seen[kind+"\x00"+subject] {
			return
		}
		seen[kind+"\x00"+subject] = true
		refs = append(refs, OperatorAliasRef{Kind: kind, Subject: subject})
	}
	str := func(key string) string {
		if v, ok := f[key].(string); ok {
			return strings.TrimSpace(v)
		}
		return ""
	}
	add("oidc", str("email"))
	slack := str("slack_id")
	if slack == "" {
		slack = str("slack_user_id")
	}
	add("slack", slack)
	add("gitlab", str("gitlab_username"))
	jira := str("jira_user_id")
	if jira == "" {
		jira = str("jira_user")
	}
	add("jira", jira)
	add("github", str("github_username"))
	return refs
}
