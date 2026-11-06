package server

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"gardenio/kbeads/internal/model"
)

// TestBuiltinGitLabMentionFieldsDeclared guards the same unknown-field 400
// class as TestBuiltinThreadFieldsDeclared, but for the GitLab @mention spawn
// path — the one that makes "@gardenio <instruction>" on an MR produce an agent.
//
// The bridge's spawnMentionAgent (bridge/gitlab_mentions.go) marshals a FIXED
// field map onto the mention TASK bead. One undeclared key 400s the create, so
// the task bead never exists and NO agent is ever spawned — the mention is
// detected and logged, then silently dropped, and the human sees @gardenio
// simply not respond. This bit on mention_instruction (ka-0000003933), added
// by the catchup watchdog (ka-0000000279) without a matching schema entry.
//
// The schema MUST be a superset of the fields spawnMentionAgent writes.
func TestBuiltinGitLabMentionFieldsDeclared(t *testing.T) {
	c, ok := builtinConfigs["type:task"]
	if !ok {
		t.Fatal("builtinConfigs missing \"type:task\"")
	}
	var tc model.TypeConfig
	if err := json.Unmarshal(c.Value, &tc); err != nil {
		t.Fatalf("type:task: unmarshal config value: %v", err)
	}
	have := make(map[string]struct{}, len(tc.Fields))
	for _, f := range tc.Fields {
		have[f.Name] = struct{}{}
	}
	// The union of every field spawnMentionAgent stamps on the task bead.
	for _, want := range []string{
		"mr_url",              // MR binding
		"gitlab_mr_iid",       //
		"mention_author",      // GitLab username of the commenter
		"mention_instruction", // raw instruction; catchup rebuilds the prompt from it
		"spawn_source",        // "gitlab-mention"
		"operated_by",         // originating human
		"slack_user_id",       // resolved identity (optional at write time)
		"user_bead_id",        //
		"gitlab_username",     //
	} {
		if _, ok := have[want]; !ok {
			t.Errorf("type:task schema missing field %q (spawnMentionAgent stamps it; absence 400s the task create and NO agent spawns for a GitLab @mention)", want)
		}
	}
}

// TestBuiltinMRConflictFieldsDeclared: github-bridge writes the PR conflict
// fields (docs/pr-conflicts.md, plan gd-nTCENSwMOH) on whatever issue-kind bead
// carries mr_url. Issue-kind types reject unknown fields, so a type that took
// mr_url but not these would 400 the bridge's whole field write and the
// conflict triggers would never see the state.
func TestBuiltinMRConflictFieldsDeclared(t *testing.T) {
	conflictFields := []string{
		"mr_merge_state",
		"mr_conflict_files",
		"mr_conflict_kind",
		"mr_conflict_since",
		"mr_conflict_base",
		"mr_conflict_cause",
		"mr_overlaps",
	}
	var checked []string
	for key, c := range builtinConfigs {
		typ, ok := strings.CutPrefix(key, "type:")
		if !ok {
			continue
		}
		var tc model.TypeConfig
		if err := json.Unmarshal(c.Value, &tc); err != nil {
			t.Fatalf("%s: unmarshal config value: %v", key, err)
		}
		have := make(map[string]struct{}, len(tc.Fields))
		for _, f := range tc.Fields {
			have[f.Name] = struct{}{}
		}
		if _, ok := have["mr_url"]; tc.Kind != model.KindIssue || !ok {
			continue
		}
		checked = append(checked, typ)
		for _, want := range conflictFields {
			if _, ok := have[want]; !ok {
				t.Errorf("type:%s declares mr_url but not %s", typ, want)
			}
		}
	}
	for _, typ := range []string{"task", "bug", "feature", "chore", "epic", "merge-request"} {
		if !slices.Contains(checked, typ) {
			t.Errorf("type:%s is not an issue-kind type carrying mr_url (checked %v)", typ, checked)
		}
	}
}
