package server

import (
	"encoding/json"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gardenio/kbeads/internal/model"
)

// identityKeysForAlias lists the type:user field keys that can produce an
// alias of the given kind (the inverse of userBeadAliasRefs).
func identityKeysForAlias(kind string) []string {
	switch kind {
	case "oidc":
		return []string{"email"}
	case "slack":
		return []string{"slack_id", "slack_user_id"}
	case "gitlab":
		return []string{"gitlab_username"}
	case "jira":
		return []string{"jira_user_id", "jira_user"}
	case "github":
		return []string{"github_username"}
	}
	return nil
}

// decodeMutationFields parses a mutation_history old/new value of a type:user
// bead's "fields" blob. The old value is the JSON text; the new value was
// recorded with fmt's %v on a json.RawMessage, so it reads back as
// "[123 34 ...]" (a byte-slice dump). Both forms are accepted.
func decodeMutationFields(v string) map[string]any {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil
	}
	raw := []byte(v)
	if strings.HasPrefix(v, "[") && !strings.Contains(v, ",") && !strings.Contains(v, "\"") {
		parts := strings.Fields(strings.Trim(v, "[]"))
		b := make([]byte, 0, len(parts))
		ok := len(parts) > 0
		for _, p := range parts {
			n, err := strconv.Atoi(p)
			if err != nil || n < 0 || n > 255 {
				ok = false
				break
			}
			b = append(b, byte(n))
		}
		if ok {
			raw = b
		}
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return m
}

// userIdentityWriters returns, for every identity field currently set on a
// type:user bead, the actor that last wrote that field's value, reconstructed
// from mutation_history (muts: the bead's rows in any order). A field present
// in the stored bead whose writer cannot be established maps to "".
//
// Writers are attributed from three row shapes: the "*" create row (the
// creator wrote every field the bead was born with), whole-blob "fields" rows
// (the actor wrote every identity key whose value differs old->new), and
// per-key "fields.<key>" rows.
func userIdentityWriters(current json.RawMessage, muts []*model.MutationRow) map[string]string {
	sorted := append([]*model.MutationRow(nil), muts...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	writers := map[string]string{}
	creator := ""
	for _, m := range sorted {
		if m.Field == "*" && m.MutationType == model.MutationTypeCreate {
			creator = m.Actor
		}
	}
	for _, m := range sorted {
		switch {
		case m.Field == "fields":
			oldF, newF := decodeMutationFields(m.OldValue), decodeMutationFields(m.NewValue)
			for k := range userIdentityFields {
				ov, had := oldF[k]
				nv, has := newF[k]
				if had && !reflect.DeepEqual(ov, nv) || !had && has && !isEmptyFieldValue(nv) {
					writers[k] = m.Actor
				} else if had && !isEmptyFieldValue(ov) {
					if _, set := writers[k]; !set {
						writers[k] = creator
					}
				}
			}
		case strings.HasPrefix(m.Field, "fields."):
			if k := strings.TrimPrefix(m.Field, "fields."); userIdentityFields[k] {
				writers[k] = m.Actor
			}
		}
	}
	var cur map[string]any
	_ = json.Unmarshal(current, &cur)
	out := map[string]string{}
	for k, v := range cur {
		if !userIdentityFields[k] || isEmptyFieldValue(v) {
			continue
		}
		w, ok := writers[k]
		if !ok {
			w = creator
		}
		out[k] = w
	}
	return out
}

// isHumanActor reports whether a mutation actor looks like an OIDC human
// (the audit actor for an OIDC caller is its email). Agent names, "static",
// fleet names and trigger/escalation actors never contain '@'.
//
// This is attribution, not authentication: a static-token caller can send
// X-Actor, so a pre-gd-uAvleAZUvV write could carry a forged email actor. It
// is a second factor on top of the email match, and the read-only audit of the
// stored beads (gd-0DMfxQdIow) is what vouches for existing data.
func isHumanActor(actor string) bool {
	return strings.Contains(actor, "@") && !strings.ContainsAny(actor, " :")
}
