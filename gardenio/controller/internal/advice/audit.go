package advice

import (
	"sort"
	"strings"
	"time"

	"gardenio/controller/internal/beadsapi"
)

// TokenBytesRatio is the bytes-per-token estimate used to convert catalog
// bytes into approximate tokens (~4 bytes/token for English prose). Keep in
// sync with the sizing convention in prime_advice.go's budget comments and
// the context-crisis plan (ka-0000005777).
const TokenBytesRatio = 4

// Archetype is a simulated (project, role) agent shape used to audit advice
// delivery cost without a live agent bead. It stands in for "any agent of
// this project and role" — the unit the context-crisis plan budgets against
// (≤18k matched tokens per archetype, ka-0000003733 Part A).
type Archetype struct {
	Project string `json:"project"`
	Role    string `json:"role"`
}

// ArchetypeSubscriptions builds the subscription set a typical agent of the
// given (project, role) archetype carries. It mirrors what
// BuildAgentSubscriptions + EnrichAgentSubscriptions produce for a real
// agent — global, the project label, and both singular/plural role variants
// (a real agent gets role:<plural> from its agent-ID path segment and
// role:<singular> from Singularize + its bead's role field) — minus the
// per-agent agent:<id> label, which no archetype can simulate.
func ArchetypeSubscriptions(project, role string) []string {
	subs := []string{"global"}
	if project != "" {
		subs = append(subs, "project:"+project)
	}
	if role != "" {
		subs = append(subs, "role:"+role)
		if s := Singularize(role); s != role {
			subs = append(subs, "role:"+s)
		} else {
			subs = append(subs, "role:"+role+"s")
		}
	}
	// Mirror the role-family anchor the real delivery path applies
	// (BuildAgentSubscriptions/EnrichAgentSubscriptions call AppendRoleFamilySubs),
	// so the audit's per-archetype token totals equal delivery truth for
	// differentiated gardeners (ka-0000000880).
	return AppendRoleFamilySubs(subs)
}

// ScopeStat aggregates matched advice per targeting scope
// (global/project/role/agent).
type ScopeStat struct {
	Beads int `json:"beads"`
	Bytes int `json:"bytes"`
}

// AuditItem is one matched advice bead in an archetype's audit, sized by its
// description bytes (the matched-catalog measure — what the matcher churns,
// before any render budget clips it).
type AuditItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Scope string `json:"scope"`
	Bytes int    `json:"bytes"`
	// CreatedBy is the actor that authored the bead — carried through the audit
	// so the per-bead roll-up below can report provenance without a second
	// fetch. See BeadCost.CreatedBy for why the audit surfaces it at all.
	CreatedBy string `json:"created_by,omitempty"`
	// CreatedAt is when the bead was written, used to report how long a
	// practice has been in the catalog.
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// ArchetypeAudit is the audit result for one (project, role) archetype:
// how many advice beads the real delivery pipeline matches for that agent
// shape and how many description bytes / approximate tokens they carry.
type ArchetypeAudit struct {
	Archetype
	Matched int                  `json:"matched"`
	Bytes   int                  `json:"bytes"`
	Tokens  int                  `json:"tokens"`
	ByScope map[string]ScopeStat `json:"by_scope"`
	Items   []AuditItem          `json:"items,omitempty"`
	// RenderBytes is the abbreviated-render demand for this archetype's matched
	// pool: the bytes the NON-inline items emit through the real gb prime
	// abbreviated rendering (advice.AbbrevRenderBytes) — the quantity that
	// accumulates against the abbreviated byte budget and that the R4 tripwire
	// watches (ka-0000002748). Distinct from Bytes, which is the full-description
	// matched-catalog churn the per-archetype token budget applies to.
	RenderBytes int `json:"render_bytes"`
}

// AuditCatalog simulates advice delivery for each archetype against the
// given corpus and returns per-archetype matched count + byte/token totals.
// It applies the SAME pipeline gb prime applies on the real delivery path —
// fixture exclusion, the bare-project broadcast deny, MatchesSubscriptions,
// supersession collapse, and the legacy hippo-mirror drop — so audit numbers
// equal delivery truth (the repeatable form of the one-off audit behind
// ka-0000003733; see ka-0000007251).
//
// Expired and closed beads are filtered defensively even though
// ListOpenAdvice already excludes them, so callers may pass a raw bead list.
// Items in each result are sorted by bytes descending.
func AuditCatalog(beads []*beadsapi.BeadDetail, archetypes []Archetype) []ArchetypeAudit {
	// Pre-filter the corpus once: everything the delivery path drops
	// unconditionally (per-bead predicates, independent of subscriptions).
	deliverable := make([]*beadsapi.BeadDetail, 0, len(beads))
	for _, b := range beads {
		if b == nil || b.Status == "closed" || IsExpired(b) || IsFixture(b) || IsHippoMirror(b) || IsDeniedBroadcast(b) {
			continue
		}
		deliverable = append(deliverable, b)
	}

	results := make([]ArchetypeAudit, 0, len(archetypes))
	for _, a := range archetypes {
		subs := ArchetypeSubscriptions(a.Project, a.Role)

		var matched []MatchedAdvice
		for _, b := range deliverable {
			if !MatchesSubscriptions(b.Labels, subs) {
				continue
			}
			ml := FindMatchedLabels(b.Labels, subs)
			scope, target := CategorizeScopeForMatch(b.Labels, ml)
			matched = append(matched, MatchedAdvice{
				Bead:          b,
				MatchedLabels: ml,
				Scope:         scope,
				ScopeTarget:   target,
			})
		}
		matched = ApplySupersessions(matched)

		res := ArchetypeAudit{
			Archetype: a,
			ByScope:   make(map[string]ScopeStat),
		}
		for _, m := range matched {
			n := len(m.Bead.Description)
			res.Matched++
			res.Bytes += n
			st := res.ByScope[m.Scope]
			st.Beads++
			st.Bytes += n
			res.ByScope[m.Scope] = st
			res.Items = append(res.Items, AuditItem{
				ID:        m.Bead.ID,
				Title:     m.Bead.Title,
				Scope:     m.Scope,
				Bytes:     n,
				CreatedBy: m.Bead.CreatedBy,
				CreatedAt: m.Bead.CreatedAt,
			})
		}
		res.Tokens = res.Bytes / TokenBytesRatio
		// Abbreviated-render demand — measured through the SAME rendering gb
		// prime emits, over the post-supersession matched pool, so the R4
		// tripwire metric equals delivery truth (ka-0000002748).
		res.RenderBytes = AbbrevRenderBytes(matched, DefaultSummaryMaxChars)
		sort.SliceStable(res.Items, func(i, j int) bool {
			return res.Items[i].Bytes > res.Items[j].Bytes
		})
		results = append(results, res)
	}
	return results
}

// BeadCost is one advice bead's FLEET-AMPLIFIED cost across an audited
// archetype population. Where ArchetypeAudit.Items ranks beads WITHIN one
// archetype (so a global bead reappears, at the same per-copy Bytes, in every
// archetype's list), BeadCost collapses those copies into a single row and
// weights each bead by its Reach — how many audited archetypes actually match
// it. The result answers the question the per-archetype view cannot: which one
// bead, if cut, saves the most tokens ACROSS THE FLEET. A global bead that
// reaches 57 archetypes carries 57× the fleet cost of a role bead of equal
// size that reaches one, and only FleetBytes surfaces that.
type BeadCost struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Scope is the bead's targeting scope (global/project/role/agent). It is a
	// per-bead intrinsic (a global bead is global in every archetype it hits),
	// so it is stable to read off any one match.
	Scope string `json:"scope"`
	// Bytes is the bead's description size (one copy) — the token weight the
	// per-archetype audit already reports for it.
	Bytes int `json:"bytes"`
	// Reach is the number of audited archetypes whose matched pool includes
	// this bead — the amplification factor. Bounded by the audited population,
	// so narrowing the audit with --project/--role scopes Reach to that subset.
	Reach int `json:"reach"`
	// FleetBytes is Bytes × Reach: total description bytes this bead injects
	// summed over every archetype it reaches — the fleet-wide cut lever.
	FleetBytes int `json:"fleet_bytes"`
	// FleetTokens is FleetBytes ÷ TokenBytesRatio, the approximate-token form.
	FleetTokens int `json:"fleet_tokens"`
	// CreatedBy is the actor that authored this practice, and CreatedAt is when
	// (task ka-0000007527).
	//
	// WHY PROVENANCE BELONGS IN A COST RANKING. Reach — how many archetypes an
	// advice bead is delivered to — is the only quantity this system computes
	// about advice reuse, and it is computed exclusively to sort a DELETION
	// list: FleetBytes = Bytes × Reach, descending, cut from the top. The
	// consequence is visible in live output. The current top row is a practice
	// contributed by an agent that reaches every archetype in the fleet, which
	// makes it simultaneously the most-reused item in the catalog and the first
	// thing to delete. Those are the same number read with opposite signs.
	//
	// Carrying the author and the write date does not change the sort, and
	// deliberately adds no competing score: FleetBytes remains the ranking, and
	// there is intentionally NO per-author roll-up or reach leaderboard here.
	// What it adds is the ability to see that a heavy row is a practice some
	// agent wrote on a particular day and the fleet has kept delivering since —
	// a thing with an origin you can go read, rather than an anonymous line item
	// with a byte count. Cut it or keep it on the merits; just know what it is.
	CreatedBy string    `json:"created_by,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

// AggregateByBead collapses a per-archetype audit (the output of AuditCatalog)
// into a per-bead, fleet-amplified cost ranking. Each bead that appears in any
// archetype's matched Items contributes one BeadCost row whose Reach counts the
// archetypes it appears in and whose FleetBytes = Bytes × Reach. Rows are
// sorted by FleetBytes descending (ID-ascending tie-break) so the heaviest
// fleet-wide cut target is first. The audited archetype population is whatever
// AuditCatalog was given, so a --project/--role-narrowed audit yields a
// scoped-reach ranking for exactly that subset.
func AggregateByBead(results []ArchetypeAudit) []BeadCost {
	byID := make(map[string]*BeadCost)
	for _, r := range results {
		for _, it := range r.Items {
			bc, ok := byID[it.ID]
			if !ok {
				bc = &BeadCost{
					ID:        it.ID,
					Title:     it.Title,
					Scope:     it.Scope,
					Bytes:     it.Bytes,
					CreatedBy: it.CreatedBy,
					CreatedAt: it.CreatedAt,
				}
				byID[it.ID] = bc
			}
			bc.Reach++
		}
	}
	out := make([]BeadCost, 0, len(byID))
	for _, bc := range byID {
		bc.FleetBytes = bc.Bytes * bc.Reach
		bc.FleetTokens = bc.FleetBytes / TokenBytesRatio
		out = append(out, *bc)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].FleetBytes != out[j].FleetBytes {
			return out[i].FleetBytes > out[j].FleetBytes
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ArchetypesFromAgentBeads derives the (project, role) archetype set the REAL
// fleet has actually run, from agent beads. Each agent bead contributes exactly
// the ONE (project, role) pair it carries — never a cross-product. So the
// audited population is the fleet, not an imaginary grid.
//
// This replaced DeriveArchetypes, which crossed every project: label with every
// role: label found in the ADVICE CORPUS (ka-0000002153): that cross-product
// manufactured phantom archetypes for (project, role) combinations no agent ever
// ran (e.g. aws-cost-management/gardener-e2e) while the highest-volume real pairs
// — gardenio/gardener-bugfix and 17 others, 749 agent beads — went unaudited,
// because the corpus was never consulted for which pairs co-occur on a live agent.
//
// Each pair is read from the agent bead's project/role fields (the source
// ListAgentBeads canonicalizes), falling back to its project:/role: labels — some
// agent beads carry the role as a field only, with no role: label. Roles are
// deduped by singular form, since a real agent subscribes to both the singular
// and plural role variants (ArchetypeSubscriptions mirrors this). The junk
// project guard (project:gd-…, a BuildAgentSubscriptions artifact) and empty
// project/role rows are skipped. Output is sorted by project then role for stable
// rendering.
func ArchetypesFromAgentBeads(agentBeads []*beadsapi.BeadDetail) []Archetype {
	seen := map[Archetype]struct{}{}
	for _, b := range agentBeads {
		if b == nil {
			continue
		}
		project := agentBeadAttr(b, "project")
		role := agentBeadAttr(b, "role")
		if role == "" {
			// Mirror ListAgentBeads, which defaults a roleless agent to "crew".
			role = "crew"
		}
		// Junk project values shaped like bead IDs are a BuildAgentSubscriptions
		// artifact, not a real project; an empty project is an un-projected agent
		// (e.g. the bare role:gardener beads) that names no archetype grid cell.
		if project == "" || (strings.HasPrefix(project, "gd-") || strings.HasPrefix(project, "ka-")) {
			continue
		}
		seen[Archetype{Project: project, Role: Singularize(role)}] = struct{}{}
	}

	return sortedArchetypes(seen)
}

// sortedArchetypes flattens an archetype set into the canonical project-then-role
// order. Shared by ArchetypesFromAgentBeads and the paged walk
// (FleetArchetypesPaged) so a paged derivation and a single-shot one return
// byte-identical output.
func sortedArchetypes(seen map[Archetype]struct{}) []Archetype {
	out := make([]Archetype, 0, len(seen))
	for a := range seen {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Project != out[j].Project {
			return out[i].Project < out[j].Project
		}
		return out[i].Role < out[j].Role
	})
	return out
}

// agentBeadAttr reads an agent bead's project/role, preferring the custom field
// (fields->>key, the value ListAgentBeads reads) and falling back to the
// project:/role: label. Returns "" when neither is set.
func agentBeadAttr(b *beadsapi.BeadDetail, key string) string {
	if b.Fields != nil {
		if v := b.Fields[key]; v != "" {
			return v
		}
	}
	for _, l := range b.Labels {
		if name, ok := strings.CutPrefix(StripGroupPrefix(l), key+":"); ok && name != "" {
			return name
		}
	}
	return ""
}

// IsHippoMirror returns true if the bead is a legacy hippo→advice mirror
// artifact from the removed hippo subsystem. Identified by a
// "source:hippo-global" or "source:hippo-memory" label, or a matching source
// field. Both variants are stale: hippo-global mirrors are malformed (title
// bleeds into description) and hippo-memory mirrors are Q/A gardener-cycle
// status replays that are not durable guidance. This is the single delivery
// predicate shared by gb prime, gb advice list, gb advice audit, and the
// controller's advice-catalog metrics (ka-0000002735 parity contract).
func IsHippoMirror(b *beadsapi.BeadDetail) bool {
	for _, l := range b.Labels {
		if l == "source:hippo-global" || l == "source:hippo-memory" {
			return true
		}
	}
	if b.Fields != nil {
		switch b.Fields["source"] {
		case "hippo-global", "hippo-memory":
			return true
		}
	}
	return false
}
