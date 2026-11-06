package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"

	"gardenio/controller/internal/beadsapi"

	"github.com/spf13/cobra"
)

// ── escalation create ───────────────────────────────────────────────────

var escalationCreateCmd = &cobra.Command{
	Use:   "create",
	Short: "Create a structured escalation request",
	Long: `Create a structured escalation: a concrete, audit-worthy action a
privileged operator must run for you (AWS IAM, K8s RBAC, secret rotation,
GitLab admin, Vault) — not a judgment fork (use 'gb decision create') or a
progress note (use 'gb squawk').

Flag gotchas the per-flag help won't stress:
  --requesting-task    your current task bead id; writes the blocks-edge so
                       the blocked work (and its /plans plan) reads as blocked
                       and the bridge can nudge you on closure.
  --expected-outputs   per-output sensitivity is REQUIRED for any credential
                       or secret, or it lands in plaintext on the bead + Slack.
  --operation-spec     for kbeads_bead_close / bead_close, name the beads:
                       '{"bead_ids":["gd-AAA","gd-BBB"]}'. The approved
                       executor may close exactly those and nothing else.
  --audience owner     send it to the owner. The daemon decides who an
                       escalation is for: desk for a merge (gitlab_mr_merge),
                       operator for everything else, and owner for one that
                       needs --credentials, is --sensitivity secret or names
                       an approver. Say --audience owner for a product or
                       policy choice, anything only the owner can do
                       (passkey/MFA, GitHub sudo, legal), or spending money.
                       Only owner-audience escalations reach the owner; it is
                       fixed once filed. Approval is the same whatever it says.

After create, call 'gb stop' — do NOT poll 'gb escalation show'; operator
approval wakes you via mail.

See: advice ka-0000001089 — when to escalate, the capability check, and
full worked examples.`,
	RunE: func(cmd *cobra.Command, args []string) error {
		title, _ := cmd.Flags().GetString("title")
		opType, _ := cmd.Flags().GetString("operation-type")
		opSpec, _ := cmd.Flags().GetString("operation-spec")
		script, _ := cmd.Flags().GetString("script")
		scriptFile, _ := cmd.Flags().GetString("script-file")
		escalCtx, _ := cmd.Flags().GetString("context")
		creds, _ := cmd.Flags().GetString("credentials")
		dataReqs, _ := cmd.Flags().GetString("data-requirements")
		outputs, _ := cmd.Flags().GetString("expected-outputs")
		sensitivity, _ := cmd.Flags().GetString("sensitivity")
		execTarget, _ := cmd.Flags().GetString("execution-target")
		targetRunner, _ := cmd.Flags().GetString("target-runner")
		targetOwner, _ := cmd.Flags().GetString("target-owner")
		spawnRole, _ := cmd.Flags().GetString("spawn-role")
		requestingTask, _ := cmd.Flags().GetString("requesting-task")
		closeOnExecute, _ := cmd.Flags().GetBool("close-on-execute")
		labels, _ := cmd.Flags().GetStringArray("label")
		priority, _ := cmd.Flags().GetInt("priority")
		audienceFlag, _ := cmd.Flags().GetString("audience")
		mrURLFlag, _ := cmd.Flags().GetString("mr-url")
		implementsDecision, _ := cmd.Flags().GetString("implements-decision")

		if title == "" {
			return fmt.Errorf("--title is required")
		}
		if opType == "" {
			return fmt.Errorf("--operation-type is required")
		}

		// Parse operation_spec.
		var specObj any
		if opSpec != "" {
			if err := json.Unmarshal([]byte(opSpec), &specObj); err != nil {
				return fmt.Errorf("--operation-spec must be valid JSON: %w", err)
			}
		} else {
			specObj = map[string]any{}
		}

		if err := checkBeadCloseSpec(opType, specObj); err != nil {
			return err
		}

		audience, err := escalationCreateAudience(audienceFlag)
		if err != nil {
			return err
		}

		// Resolve execution_script from --script or --script-file.
		if script != "" && scriptFile != "" {
			return fmt.Errorf("use --script or --script-file, not both")
		}
		if scriptFile != "" {
			data, err := os.ReadFile(scriptFile)
			if err != nil {
				return fmt.Errorf("reading script file: %w", err)
			}
			script = string(data)
		}

		// Validate sensitivity enum.
		if sensitivity != "" {
			switch sensitivity {
			case "public", "internal", "sensitive", "secret":
				// valid
			default:
				return fmt.Errorf("--sensitivity must be one of: public, internal, sensitive, secret")
			}
		}

		// Validate execution_target enum.
		if execTarget != "" {
			switch execTarget {
			case "local", "service_account", "server":
				// valid
			default:
				return fmt.Errorf("--execution-target must be one of: local, service_account, server")
			}
		}
		execTarget, targetOwner, impliedLocal, err := resolveEscalationLocalTarget(execTarget, targetRunner, targetOwner)
		if err != nil {
			return err
		}
		if impliedLocal {
			fmt.Fprintln(os.Stderr, "note: --target-runner/--target-owner imply --execution-target local")
		}

		// Layer 3 source-prevention (ka-0000002745, design ka-0000006833 §3):
		// when --requesting-task is omitted, auto-derive it from the current
		// agent bead's fields.task_id so the escalation still homes onto the
		// agent's task via the blocks-edge + requesting_plan path below,
		// instead of orphaning off every /plans surface. Runs after all input
		// validation (so pure validation errors never touch the daemon), and
		// before the merge-capability guard so that guard sees the derived
		// task. Best-effort — leaves requestingTask empty on any resolution
		// failure, and the no-homing warning fires after creation.
		if requestingTask == "" {
			if derived := deriveRequestingTask(cmd.Context(), ""); derived != "" {
				requestingTask = derived
				fmt.Fprintf(os.Stderr, "note: auto-derived --requesting-task %s from agent bead task_id\n", derived)
			}
		}

		// Expand curl-style @file / @- / @@ on --context (ka-0000003818),
		// matching the convention `kd` documents on its prose flags and the
		// sibling `gb decision report --content` (ka-0000003861). --context is
		// the human-readable background an operator reads to understand and
		// approve the escalation; agents commonly draft it in a file, so a
		// silently-stored "@/path" pointer here would ship an empty rationale
		// with a zero exit. A missing/typo'd path fails LOUD. (The JSON-shaped
		// flags --operation-spec / --credentials / --data-requirements /
		// --expected-outputs are deliberately left literal: they already
		// fail-loud on json.Unmarshal, and @file is not their idiom; --script
		// has its own --script-file companion.)
		if escalCtx != "" {
			var err error
			escalCtx, err = resolveTextValue(escalCtx)
			if err != nil {
				return fmt.Errorf("--context: %w", err)
			}
		}
		// Merge escalations are idempotent per PR (gd-f7GvZ7qahY). Name the PR
		// in the spec when only the title, --context or the requesting task
		// does, then return the open escalation for that PR instead of filing
		// a second one. Runs before the capability guard so the guard sees the
		// filled spec, and so a duplicate costs no GitLab call.
		if mrURLFlag != "" && !mergeOperationTypes[opType] {
			return fmt.Errorf("--mr-url applies only to a merge escalation (gitlab_mr_merge, github_pr_merge)")
		}
		if mergeOperationTypes[opType] {
			spec, ok := specObj.(map[string]any)
			if !ok {
				return fmt.Errorf("--operation-spec must be a JSON object for a merge escalation")
			}
			if mrURLFlag != "" {
				u := firstPRURL(mrURLFlag)
				if u == "" {
					return fmt.Errorf("--mr-url %q is not a GitHub PR or GitLab MR URL", mrURLFlag)
				}
				if s := specPRURL(spec); s != "" && prKey(s) != prKey(u) {
					return fmt.Errorf("--mr-url %s and --operation-spec %s name different PRs; give one", u, s)
				}
				setSpecMRURL(spec, u)
			}
			// A GitLab project + iid spec names its MR already: no branch
			// lookup, and fillMergeSpecURL leaves it alone.
			_, hasTarget := parseMergeTarget(spec)
			branchPR := func() string {
				if hasTarget {
					return ""
				}
				return escalationBranchPRURL(cmd.Context())
			}
			taskMRURL := func() string {
				if requestingTask == "" || daemon == nil {
					return ""
				}
				if b, err := daemon.GetBead(cmd.Context(), requestingTask); err == nil {
					return mrURLFromBead(b)
				}
				return ""
			}
			prURL, source := resolveMergePRURL(spec, branchPR, taskMRURL, title, escalCtx)
			if err := checkMergePRNamed(prURL, source, title, escalCtx); err != nil {
				return fmt.Errorf("refusing to create this escalation: %w", err)
			}
			if fillMergeSpecURL(spec, prURL) {
				fmt.Fprintf(os.Stderr, "note: set operation_spec.mr_url to %s (from %s)\n", prURL, source)
			}
			if existing := findOpenMergeEscalation(cmd.Context(), daemon, prKey(prURL)); existing != nil {
				reportExistingMergeEscalation(os.Stdout, os.Stderr, existing, prKey(prURL))
				return nil
			}
			// GitHub cannot merge a draft; `gb pr ready` flips it and files
			// this escalation itself (gd-9mAPy3HB8j). Fails open on a lookup
			// error — see escalation_draft.go.
			if err := checkMergeEscalationDraft(cmd.Context(), prURL, os.Stderr); err != nil {
				return fmt.Errorf("refusing to create this escalation: %w", err)
			}
		}

		// --close-on-execute against an epic would close the whole epic when
		// this escalation executes (gd-0SbA6Iyclm): use the step the PR
		// delivers, or refuse. See escalation_epic_close.go.
		if closeOnExecute && requestingTask != "" {
			spec, _ := specObj.(map[string]any)
			target, err := closeOnExecuteTarget(cmd.Context(), requestingTask, specPRURL(spec), os.Stderr)
			if err != nil {
				return fmt.Errorf("refusing to create this escalation: %w", err)
			}
			requestingTask = target
		}

		// Capability guard (ka-0000004913): refuse a gitlab_mr_merge escalation
		// for an MR this token can merge itself, or that has already merged/closed.
		// Fails open on every unknown — see escalation_merge_guard.go for why.
		//
		// The guard still RUNS under --skip-merge-capability-check; the flag only
		// stops it being fatal. A bypass over a positive verdict is recorded in
		// merge_guard_bypassed so it is auditable instead of silent: the flag is
		// the documented escape hatch for GitLab misreporting access, and the
		// only way to tell that apart from an agent talking itself past a correct
		// refusal is to keep the verdict it overrode (bug ka-0000007570).
		var mergeGuardBypassed string
		if opType == "gitlab_mr_merge" {
			d := checkMergeEscalationCapability(cmd.Context(), decodeOperationSpec(specObj), requestingTask)
			switch {
			case d.Refuse && !skipMergeCapabilityCheck:
				return fmt.Errorf("refusing to create this escalation:\n\n%s", d.Reason)
			case d.Refuse:
				mergeGuardBypassed = d.Reason
				fmt.Fprintf(os.Stderr,
					"WARNING: --skip-merge-capability-check overrode a POSITIVE merge-capability verdict.\n"+
						"Recording it on the escalation as merge_guard_bypassed.\n%s\n", d.Reason)
			case d.Warn:
				fmt.Fprintf(os.Stderr, "WARNING: %s\n", d.Reason)
			}
		}

		// Build fields.
		fields := map[string]any{
			"operation_type": opType,
			"operation_spec": specObj,
		}
		if mergeGuardBypassed != "" {
			fields["merge_guard_bypassed"] = mergeGuardBypassed
		}
		if script != "" {
			fields["execution_script"] = script
		}
		if escalCtx != "" {
			fields["context"] = escalCtx
		}
		if sensitivity != "" {
			fields["sensitivity"] = sensitivity
		}
		if audience != "" {
			fields["audience"] = audience
		}
		if execTarget != "" {
			fields["execution_target"] = execTarget
		}
		if targetRunner != "" {
			fields["target_runner"] = targetRunner
		}
		if targetOwner != "" {
			fields["target_owner"] = targetOwner
		}
		if spawnRole != "" {
			fields["spawn_role"] = spawnRole
		}
		if implementsDecision != "" {
			fields["implements_decision"] = implementsDecision
		}
		if requestingTask != "" {
			fields["requesting_task"] = requestingTask
			// close-on-execute contract, scoped to THIS escalation
			// (gd-91b7zMOR2z fix a): record the opt-in as an explicit
			// true/false field on the escalation bead itself, not as a label
			// stamped on the task. The daemon (maybeCloseEscalationWrappers)
			// reads this field to decide whether THIS escalation's success
			// closes the requesting task — a later, unrelated escalation
			// against the same task carries its own field and can never
			// inherit this one's opt-in, unlike the legacy task label, which
			// stuck to the task forever once stamped.
			fields["close_requesting_task"] = closeOnExecute
		}

		// Auto-populate requesting_agent from env.
		agentBeadID := os.Getenv("BOAT_AGENT_BEAD_ID")
		if agentBeadID == "" {
			agentBeadID = os.Getenv("KD_AGENT_ID")
		}
		if agentBeadID != "" {
			fields["requesting_agent"] = agentBeadID
		}

		// Parse JSON array fields.
		if creds != "" {
			var v any
			if err := json.Unmarshal([]byte(creds), &v); err != nil {
				return fmt.Errorf("--credentials must be valid JSON: %w", err)
			}
			fields["credential_requirements"] = v
		}
		if dataReqs != "" {
			var v any
			if err := json.Unmarshal([]byte(dataReqs), &v); err != nil {
				return fmt.Errorf("--data-requirements must be valid JSON: %w", err)
			}
			fields["data_requirements"] = v
		}
		if outputs != "" {
			var v any
			if err := json.Unmarshal([]byte(outputs), &v); err != nil {
				return fmt.Errorf("--expected-outputs must be valid JSON: %w", err)
			}
			fields["expected_outputs"] = v
		}

		fieldsJSON, err := json.Marshal(fields)
		if err != nil {
			return fmt.Errorf("marshalling fields: %w", err)
		}

		// Add project label from env if not explicitly provided.
		if project := os.Getenv("BOAT_PROJECT"); project != "" {
			hasProject := false
			for _, l := range labels {
				if strings.HasPrefix(l, "project:") {
					hasProject = true
					break
				}
			}
			if !hasProject {
				labels = append(labels, "project:"+project)
			}
		}

		// Dedup check: warn if similar open escalation already exists.
		if existing, err := daemon.ListBeadsFiltered(cmd.Context(), beadsapi.ListBeadsQuery{
			Types:    []string{"escalation"},
			Statuses: []string{"open", "in_progress"},
			Limit:    100,
		}); err == nil {
			for _, b := range existing.Beads {
				if b.Fields["operation_type"] == opType && fuzzyTitleMatch(b.Title, title) {
					fmt.Fprintf(os.Stderr, "WARNING: similar escalation already exists: %s (%s) — %s\n",
						b.ID, b.Status, b.Title)
				}
			}
		}

		req := beadsapi.CreateBeadRequest{
			Title:    title,
			Type:     "escalation",
			Kind:     "data",
			Fields:   json.RawMessage(fieldsJSON),
			Labels:   labels,
			Priority: priority,
		}
		id, err := daemon.CreateBead(cmd.Context(), req)
		if audienceUnknown(err) {
			// A daemon older than the audience field (gd-bmOiteQi36) refuses
			// it; file without it, and every surface reads the default.
			fmt.Fprintln(os.Stderr, "note: this daemon does not know the audience field yet; filing without it (the default applies)")
			delete(fields, "audience")
			if req.Fields, err = json.Marshal(fields); err == nil {
				id, err = daemon.CreateBead(cmd.Context(), req)
			}
		}
		if err != nil {
			return fmt.Errorf("creating escalation: %w", err)
		}

		// Materialize the escalation→work link as a real dependency edge
		// (ka-0000004626): the requesting task gets a forward `blocks` dep on
		// the escalation — "task is blocked by escalation" — which is exactly
		// what the /plans blocker walk (enrichPlanStepBlockers) and any "is
		// this work blocked by an escalation?" query read. The requesting_task
		// string field alone is invisible to the dependency graph; it is kept
		// for back-compat. Best-effort: the escalation exists and must not be
		// lost to a link failure.
		if requestingTask != "" {
			if err := daemon.AddDependency(cmd.Context(), requestingTask, id, "blocks", actor); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: escalation created but could not link blocks-edge from %s: %v\n", requestingTask, err)
			}
			// close-on-execute contract (ka-0000001587): the authoritative
			// opt-in is close_requesting_task on THIS escalation (set above),
			// which the daemon reads by preference. The task label below is
			// now DEPRECATED — kept for one release only as the daemon's
			// back-compat fallback for escalations that predate this fix.
			//
			// gd-91b7zMOR2z fix (b): without the flag, actively strip the
			// label if a PRIOR --close-on-execute escalation against this
			// same task left it stamped — otherwise that stale label alone
			// (read only when an escalation carries no close_requesting_task
			// field at all) could still wrongly close the task under an
			// old daemon binary. RemoveLabel is a no-op when the label is
			// absent, so this is safe to call unconditionally.
			if closeOnExecute {
				if err := daemon.AddLabel(cmd.Context(), requestingTask, "close-on-escalation-execute"); err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: escalation created but could not stamp close-on-escalation-execute on %s: %v\n", requestingTask, err)
				}
			} else {
				if err := daemon.RemoveLabel(cmd.Context(), requestingTask, "close-on-escalation-execute"); err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: escalation created but could not clear a stale close-on-escalation-execute label on %s: %v\n", requestingTask, err)
				}
			}
			// Record the owning /plans plan directly (ka-0000006860): the
			// nearest plan-labeled epic ancestor of the requesting task. The
			// blocks edge above survives only as long as the step keeps its
			// parent; the requesting_plan field pins the plan the escalation
			// blocked at create time even if the step is later reparented.
			// Best-effort — most escalations come from non-plan work and
			// simply have no plan ancestor.
			if planID := resolveOwningPlan(cmd.Context(), requestingTask); planID != "" {
				if err := daemon.UpdateBeadFields(cmd.Context(), id, map[string]string{
					"requesting_plan": planID,
				}); err != nil {
					fmt.Fprintf(os.Stderr, "WARNING: could not record requesting_plan %s: %v\n", planID, err)
				}
			}
		} else {
			// No requesting-task resolved (neither passed nor auto-derived), so
			// no plan ancestor either — the escalation homes onto NO /plans
			// surface (ka-0000002745, design ka-0000006833 §3 Layer 3). Warn so
			// the operator/agent knows to home it, or waits for Layer 1's
			// /plans unhomed backstop to surface it. (The flat /escalations
			// inbox still lists it.)
			fmt.Fprintf(os.Stderr, "WARNING: escalation %s has no requesting task or plan ancestor — it is visible only in the flat /escalations inbox (and the /plans unhomed section) until an operator homes it. Pass --requesting-task <your task bead id> to home it.\n", id)
		}

		fmt.Fprintf(os.Stdout, "escalation_id=%s\n", id)
		return nil
	},
}

// resolveOwningPlan walks a work bead's parent-child chain upward to the
// nearest `plan`-labeled epic ancestor — the /plans plan the work belongs to
// (ka-0000006860). Returns "" when the bead has no plan ancestor, on any
// fetch error, or past a small depth bound (a plan step is at most a
// grandchild: plan epic → phase sub-epic → step).
func resolveOwningPlan(ctx context.Context, beadID string) string {
	const maxDepth = 4
	cur := beadID
	seen := map[string]bool{}
	for range maxDepth {
		if cur == "" || seen[cur] {
			return ""
		}
		seen[cur] = true
		bead, err := daemon.GetBead(ctx, cur)
		if err != nil || bead == nil {
			return ""
		}
		if bead.Type == "epic" && slices.Contains(bead.Labels, "plan") {
			return bead.ID
		}
		deps, err := daemon.GetDependencies(ctx, cur)
		if err != nil {
			return ""
		}
		next := ""
		for _, d := range deps {
			if d.Type == "parent-child" && d.DependsOnID != "" {
				next = d.DependsOnID
				break
			}
		}
		cur = next
	}
	return ""
}

// fuzzyTitleMatch returns true if two titles share significant overlap,
// indicating a potential duplicate escalation.
func fuzzyTitleMatch(a, b string) bool {
	a = strings.ToLower(strings.TrimSpace(a))
	b = strings.ToLower(strings.TrimSpace(b))
	if a == b {
		return true
	}
	// Check if one title contains the other (handles prefix/suffix variations).
	if len(a) > 10 && len(b) > 10 {
		if strings.Contains(a, b) || strings.Contains(b, a) {
			return true
		}
	}
	// Check word overlap: if >60% of words match, likely a duplicate.
	wordsA := strings.Fields(a)
	wordsB := strings.Fields(b)
	if len(wordsA) < 3 || len(wordsB) < 3 {
		return false
	}
	wordSet := make(map[string]bool, len(wordsA))
	for _, w := range wordsA {
		wordSet[w] = true
	}
	matches := 0
	for _, w := range wordsB {
		if wordSet[w] {
			matches++
		}
	}
	threshold := len(wordsB) * 60 / 100
	return matches >= threshold && matches >= 3
}
