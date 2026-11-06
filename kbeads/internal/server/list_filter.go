package server

import (
	"context"
	"sort"
	"strings"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/model"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// buildListFilter converts a ListBeadsRequest into a model.BeadFilter,
// merging the legacy scalar fields with the Phase 3b "_in" repeated
// variants and applying server-side validation. Validation rejects
// combinations that are unambiguously contradictory (e.g.
// decision_pending=true together with an explicit non-decision type),
// returning codes.InvalidArgument.
//
// Plan-of-record: beads3d/docs/my-work-operator-tab.md §Phase 3.
func buildListFilter(req *beadsv1.ListBeadsRequest) (model.BeadFilter, error) {
	filter := model.BeadFilter{
		Assignee: req.GetAssignee(),
		Labels:   req.GetLabels(),
		// labels_any is the user-facing any-of filter; route it into
		// LabelsAnyUser (its own AND-ed EXISTS clause) so it never merges into
		// the OIDC-scoping LabelsAny field (ka-0000003307).
		LabelsAnyUser:    req.GetLabelsAny(),
		Search:           req.GetSearch(),
		Sort:             req.GetSort(),
		Limit:            int(req.GetLimit()),
		Offset:           int(req.GetOffset()),
		ExcludeScheduled: req.GetExcludeScheduled(),
	}
	filter.ShortTitleMissing = req.GetShortTitleMissing()
	if err := setHeraldryFilter(&filter, req.GetHeraldry()); err != nil {
		return filter, status.Error(codes.InvalidArgument, err.Error())
	}

	// Status: legacy `status` repeated + new `status_in` repeated (union).
	statusSet := make(map[model.Status]struct{})
	addStatus := func(values []string) {
		for _, v := range values {
			if v == "" {
				continue
			}
			statusSet[model.Status(v)] = struct{}{}
		}
	}
	addStatus(req.GetStatus())
	addStatus(req.GetStatusIn())

	// Type: legacy `type` repeated + new `type_in` repeated (union).
	// Resolve deprecated aliases (template→formula, bundle→molecule).
	typeSet := make(map[model.BeadType]struct{})
	addType := func(values []string) {
		for _, v := range values {
			if v == "" {
				continue
			}
			bt := model.BeadType(v)
			if canonical, ok := model.TypeAliases[bt]; ok {
				bt = canonical
			}
			typeSet[bt] = struct{}{}
		}
	}
	addType(req.GetType())
	addType(req.GetTypeIn())

	// decision_pending shortcut. Conflicts with explicit non-decision types
	// or any status other than open/in_progress.
	if req.GetDecisionPending() {
		for t := range typeSet {
			if t != model.TypeDecision {
				return model.BeadFilter{}, status.Errorf(
					codes.InvalidArgument,
					"decision_pending is mutually exclusive with type=%q", t)
			}
		}
		typeSet[model.TypeDecision] = struct{}{}
		// Restrict status set: must be subset of {open, in_progress}.
		// If unspecified, set it to those two; otherwise validate.
		if len(statusSet) == 0 {
			statusSet[model.StatusOpen] = struct{}{}
			statusSet[model.StatusInProgress] = struct{}{}
		} else {
			for s := range statusSet {
				if s != model.StatusOpen && s != model.StatusInProgress {
					return model.BeadFilter{}, status.Errorf(
						codes.InvalidArgument,
						"decision_pending is mutually exclusive with status=%q", s)
				}
			}
		}
	}

	for s := range statusSet {
		filter.Status = append(filter.Status, s)
	}
	for t := range typeSet {
		filter.Type = append(filter.Type, t)
	}

	for _, k := range req.GetKind() {
		filter.Kind = append(filter.Kind, model.Kind(k))
	}
	if req.GetPriority() != nil {
		p := int(req.GetPriority().GetValue())
		filter.Priority = &p
	}
	if len(req.GetFieldFilters()) > 0 {
		filter.Fields = req.GetFieldFilters()
	}

	// Multi-valued operator-tab predicates.
	if v := req.GetAssigneeIn(); len(v) > 0 {
		filter.Assignees = append(filter.Assignees, v...)
	}
	if v := req.GetOwnerIn(); len(v) > 0 {
		filter.Owners = append(filter.Owners, v...)
	}
	if v := req.GetCreatedByIn(); len(v) > 0 {
		filter.CreatedBys = append(filter.CreatedBys, v...)
	}
	// operated_by_in: legacy TEXT-keyed filter. Post-P6 (migration 024) the
	// operated_by TEXT column is gone — values still arrive as canonical
	// "kind:subject" alias strings, so route into OperatedByAliases which
	// resolves through operator_aliases. operated_by_is_null has no successor
	// (no_operator semantics no longer expressible after the column drop).
	if v := req.GetOperatedByIn(); len(v) > 0 {
		filter.OperatedByAliases = append(filter.OperatedByAliases, v...)
	}

	// mentions filter (ka-0000001544). Three-state semantics for mentions_type
	// match the HTTP ?mentions_type= shape (see http_beads.go):
	//   - mentions_type field absent     → MentionsType nil
	//   - mentions_type field present "" → non-nil pointer to "" (untyped only)
	//   - mentions_type field "<verb>"   → non-nil pointer to "<verb>"
	if req.Mentions != nil {
		filter.Mentions = req.GetMentions()
	}
	if req.MentionsType != nil {
		v := req.GetMentionsType()
		filter.MentionsType = &v
	}

	// has_report (R1, ka-0000007923): three-state via optional bool:
	//   - field absent → HasReport nil
	//   - field true   → HasReport &true
	//   - field false  → HasReport &false
	if req.HasReport != nil {
		v := req.GetHasReport()
		filter.HasReport = &v
	}

	// last_active_before — bound by updated_at.
	if ts := req.GetLastActiveBefore(); ts != nil {
		t := ts.AsTime()
		filter.UpdatedBefore = &t
	}

	// stale_for_minutes — relative to server clock.
	if n := req.GetStaleForMinutes(); n > 0 {
		threshold := nowFn().Add(-time.Duration(n) * time.Minute)
		// If both last_active_before and stale_for_minutes are supplied,
		// take the more restrictive (earlier) bound — the row must be
		// stale by *both* measures.
		if filter.UpdatedBefore == nil || threshold.Before(*filter.UpdatedBefore) {
			filter.UpdatedBefore = &threshold
		}
	}

	// Clamp the limit defensively so the gRPC transport shares the same OOM
	// choke point as the HTTP handler. An unbounded ListBeads against a
	// populated DB returns hundreds of MB and inflates server heap (the
	// gRPC/JSON encoder buffers the full response); the HTTP path was capped
	// for exactly this reason (defaultListBeadsLimit/maxListBeadsLimit, see
	// ka-0000006889). Callers that omit `limit` get a sane default; callers
	// that ask for more than the cap are clamped.
	filter.Limit = clampListBeadsLimit(filter.Limit, defaultListBeadsLimit)

	return filter, nil
}

// nowFn is overridable in tests so stale_for_minutes can be exercised
// deterministically.
var nowFn = func() time.Time { return time.Now().UTC() }

// validateFilterTypes rejects any type in the LIST/filter set that does not
// resolve to a known bead type, returning inputError (which both transports map
// to 400 / codes.InvalidArgument). It is the server-side backstop for the
// silent-empty-on-unknown-type footgun (ka-0000006244): historically a
// `--type=gardener` / `?type=zzz` filter passed the raw value straight into the
// query and matched nothing, so an invalid filter value was indistinguishable
// from a genuinely empty result. The CLI grew a client-side guard first
// (ka-0000002804, cmd/kd/type_validate.go); this is the deeper single-choke-point
// fix that covers EVERY client of the ListBeads RPC and the HTTP /v1/beads
// endpoint (gRPC + HTTP + beads-viewer + bridges + automation), not just kd.
//
// Reuse over reinvention (ka-0000008041): validation delegates to
// s.resolveTypeConfig — the EXACT validator the CREATE path uses to 400
// "unknown bead type" — rather than maintaining a second known-type union that
// could drift. A nil TypeConfig means the type is neither a compiled-in builtin
// nor a stored custom `type:*` config, so it is genuinely unknown. Because it
// consults the live store, custom types registered on this daemon (e.g. the
// gardenio controller's EnsureConfigs types) validate correctly with no extra
// round-trip beyond the single GetConfig resolveTypeConfig already makes.
//
// Callers pass the ALREADY-alias-resolved type set (buildListFilter and the
// HTTP handler both fold model.TypeAliases before calling), so a deprecated but
// valid alias like `template` is never rejected here.
func (s *BeadsServer) validateFilterTypes(ctx context.Context, types []model.BeadType) error {
	if len(types) == 0 {
		return nil
	}
	// Deduplicate so a repeated `--type=task --type=task` costs one lookup.
	seen := make(map[model.BeadType]struct{}, len(types))
	for _, t := range types {
		if t == "" {
			// An empty type value never matches anything, but it is not an
			// "unknown type" — leave the pre-existing (no-op) behaviour rather
			// than invent a new error, mirroring the CLI guard.
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		tc, err := s.resolveTypeConfig(ctx, t)
		if err != nil {
			// A store error resolving the type is an internal fault, not bad
			// input — surface it as-is so the transport maps it to 500, never a
			// misleading 400 "unknown type".
			return status.Errorf(codes.Internal, "failed to validate type filter: %v", err)
		}
		if tc == nil {
			return inputError("unknown bead type " + s.unknownFilterTypeMessage(ctx, string(t)))
		}
	}
	return nil
}

// unknownFilterTypeMessage renders the body of the unknown-type error: the
// offending value followed by a sorted list of valid types so a caller (human
// or agent) can immediately see what to use instead, matching the CLI guard's
// "unknown bead type %q; valid types: ..." shape.
func (s *BeadsServer) unknownFilterTypeMessage(ctx context.Context, value string) string {
	valid := s.knownTypeNames(ctx)
	if len(valid) == 0 {
		return "\"" + value + "\""
	}
	return "\"" + value + "\"; valid types: " + strings.Join(valid, ", ")
}

// knownTypeNames returns the sorted union of compiled-in builtin type names and
// any custom `type:*` configs stored on this daemon — the same set
// resolveTypeConfig validates a single type against, materialised for the
// error message. Best-effort: a store error listing custom configs degrades to
// the builtin floor rather than failing the surrounding request.
func (s *BeadsServer) knownTypeNames(ctx context.Context) []string {
	known := make(map[string]struct{})
	for _, name := range BuiltinBeadTypeNames() {
		known[name] = struct{}{}
	}
	if configs, err := s.store.ListConfigs(ctx, "type"); err == nil {
		for _, c := range configs {
			if c == nil {
				continue
			}
			if name, ok := strings.CutPrefix(c.Key, "type:"); ok && name != "" {
				known[name] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(known))
	for name := range known {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
