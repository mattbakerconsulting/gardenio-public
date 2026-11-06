package model

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ValidateTypeFields checks that the given fields JSON conforms to the type
// config's field definitions. It rejects unknown keys (unless the type
// declares AllowAdditionalFields), validates types, and enforces required
// constraints. Returns a *ValidationError on failure, nil on success. Used at
// the bead-creation choke point so the per-type schema is authoritative for
// both known-field validation and open-schema behaviour.
func ValidateTypeFields(fields json.RawMessage, tc *TypeConfig) error {
	if tc == nil {
		return validateFields(fields, nil, false, nil, "")
	}
	return validateFields(fields, tc.Fields, tc.AllowAdditionalFields, nil, tc.TypeName)
}

// ValidateTypeFieldsForUpdate is like ValidateTypeFields but grandfathers the
// values already stored on the bead before this update. Pass `prior` — the
// bead's field map as it was BEFORE the patch was merged in. It grandfathers
// two things a fresh create would reject:
//
//   - Unknown keys that were already present (a schema migration, or a field
//     written by a code path the daemon no longer recognises): so they don't
//     wedge the `gb stop` write that sets `stop_requested` (ka-0000007952).
//   - A DEFINED field whose merged value is byte-for-byte UNCHANGED from its
//     prior stored value but fails type validation: a legacy value stored in
//     the wrong JSON type (e.g. a boolean `gardener_stuck_escalated` where the
//     registry declares a string) must not permanently wedge every subsequent
//     fields-PATCH of OTHER fields against the bead (ka-0000007257). Because
//     UpdateBead validates the MERGED map, such a value re-fails validation on
//     every patch — even one that only sets `{"enabled": false}` and never
//     mentions the bad field — so the bead can never be field-updated again
//     until the value is overwritten. Skipping validation only for the
//     unchanged pre-existing value retires that whole legacy-typed-value wedge
//     class. This mirrors the byte-identical grandfather in
//     validateProjectFieldsForUpdate.
//
// The create-time guarantee is preserved: a NEWLY-introduced or CHANGED value
// (defined or unknown) is still validated and still rejected on a bad type.
func ValidateTypeFieldsForUpdate(fields json.RawMessage, tc *TypeConfig, prior map[string]any) error {
	if tc == nil {
		return validateFieldsForUpdate(fields, nil, false, prior, "")
	}
	return validateFieldsForUpdate(fields, tc.Fields, tc.AllowAdditionalFields, prior, tc.TypeName)
}

// validateFieldsForUpdate is the update-path variant of validateFields: it
// derives the grandfather key-set from the prior field values and passes the
// prior values through so a defined field whose value is unchanged from what
// was already stored can skip type validation (see ValidateTypeFieldsForUpdate).
func validateFieldsForUpdate(fields json.RawMessage, defs []FieldDef, allowAdditional bool, prior map[string]any, typeName string) error {
	var grandfather map[string]bool
	if len(prior) > 0 {
		grandfather = make(map[string]bool, len(prior))
		for k := range prior {
			grandfather[k] = true
		}
	}
	return validateFieldsImpl(fields, defs, allowAdditional, grandfather, prior, typeName)
}

func validateFields(fields json.RawMessage, defs []FieldDef, allowAdditional bool, grandfather map[string]bool, typeName string) error {
	return validateFieldsImpl(fields, defs, allowAdditional, grandfather, nil, typeName)
}

func validateFieldsImpl(fields json.RawMessage, defs []FieldDef, allowAdditional bool, grandfather map[string]bool, prior map[string]any, typeName string) error {
	if len(fields) == 0 {
		// No fields provided — only fail if any field is required.
		for _, d := range defs {
			if d.Required {
				return &ValidationError{Errors: []FieldError{{
					Field:   d.Name,
					Message: "is required",
				}}}
			}
		}
		return nil
	}

	var m map[string]any
	if err := json.Unmarshal(fields, &m); err != nil {
		return &ValidationError{Errors: []FieldError{{
			Field:   "fields",
			Message: "must be a JSON object",
		}}}
	}

	// Build lookup for known field names.
	defsByName := make(map[string]*FieldDef, len(defs))
	for i := range defs {
		defsByName[defs[i].Name] = &defs[i]
	}

	declaredFold := make(map[string]bool, len(defs))
	for i := range defs {
		declaredFold[foldFieldKey(defs[i].Name)] = true
	}

	var ve ValidationError
	ve.Errors = append(ve.Errors, FieldKeyCaseErrors(m, defs, prior)...)

	// Reject unknown keys unless the type allows arbitrary additional fields
	// or the key is explicitly grandfathered (e.g. it was already stored on
	// this bead before the update, and the caller wants to preserve it).
	if !allowAdditional {
		for key := range m {
			if _, ok := defsByName[key]; ok {
				continue
			}
			if _, ok := declaredFold[foldFieldKey(key)]; ok {
				// A case variant of a declared field: FieldKeyCaseErrors
				// has reported it with the exact name to use.
				continue
			}
			if (grandfather != nil && grandfather[key]) || key == IdempotencyKeyField {
				continue // IdempotencyKeyField: every type accepts it, see idempotencyKeyErrors
			}
			ve.Errors = append(ve.Errors, FieldError{
				Field:   key,
				Message: unknownFieldMessage(key, defs, typeName),
			})
		}
	}

	// Validate each defined field.
	for _, d := range defs {
		val, present := m[d.Name]
		if !present || val == nil {
			if d.Required {
				ve.Errors = append(ve.Errors, FieldError{
					Field:   d.Name,
					Message: "is required",
				})
			}
			continue
		}
		if err := validateFieldValue(d, val); err != nil {
			// Update path only: a DEFINED field whose value is unchanged from
			// what was already stored is grandfathered past type validation, so
			// a legacy value in the wrong JSON type (e.g. a boolean where the
			// registry says string) does not wedge every subsequent PATCH of
			// OTHER fields (ka-0000007257). A CHANGED value is still validated.
			if prior != nil && fieldValueUnchanged(prior[d.Name], val) {
				continue
			}
			ve.Errors = append(ve.Errors, FieldError{
				Field:   d.Name,
				Message: typeMismatchMessage(err.Error(), typeName),
			})
		}
	}

	ve.Errors = append(ve.Errors, idempotencyKeyErrors(m, defsByName)...)
	if ve.HasErrors() {
		return &ve
	}
	return nil
}

// IdempotencyKeyField is where a keyed create stores its key (the server's
// applyCreateIdempotencyKey). The daemon writes it on a create of any type, so
// every schema accepts it as a string, declared or not: a strict type that left
// it out refused every keyed create (the lands provisioner's project bead,
// 2026-11-05).
const IdempotencyKeyField = "idempotency_key"

// idempotencyKeyErrors checks IdempotencyKeyField on a type that does not
// declare it (a declared one is checked with the type's other fields).
func idempotencyKeyErrors(m map[string]any, defsByName map[string]*FieldDef) []FieldError {
	if _, declared := defsByName[IdempotencyKeyField]; declared {
		return nil
	}
	if v, ok := m[IdempotencyKeyField]; ok && v != nil {
		if _, isString := v.(string); !isString {
			return []FieldError{{Field: IdempotencyKeyField, Message: "must be a string"}}
		}
	}
	return nil
}

// fieldValueUnchanged reports whether the incoming (merged) value is identical
// to the value that was already stored on the bead. Both come from
// json.Unmarshal into map[string]any, so a deep equality over the decoded JSON
// value tree is exact: a boolean `false` equals a stored boolean `false`, but a
// boolean `false` does NOT equal a string "false" or a string "" — so flipping
// a legacy bool value to the registry-correct string is still validated (and
// rejected if the new value has the wrong type). Used only on the update path,
// where prior is non-nil, to grandfather a pre-existing wrong-typed value past
// type validation without weakening validation of a CHANGED value.
func fieldValueUnchanged(priorVal, newVal any) bool {
	return reflect.DeepEqual(priorVal, newVal)
}

func validateFieldValue(d FieldDef, val any) error {
	switch d.Type {
	case FieldTypeString:
		if _, ok := val.(string); !ok {
			return fmt.Errorf("must be a string%s", stringCoercionHint(d.Name, val))
		}
	case FieldTypeInteger:
		n, ok := val.(float64)
		if !ok {
			return fmt.Errorf("must be an integer")
		}
		if n != math.Trunc(n) {
			return fmt.Errorf("must be an integer")
		}
		if d.Min != nil && int(n) < *d.Min {
			return fmt.Errorf("must be >= %d", *d.Min)
		}
		if d.Max != nil && int(n) > *d.Max {
			return fmt.Errorf("must be <= %d", *d.Max)
		}
	case FieldTypeFloat:
		if _, ok := val.(float64); !ok {
			return fmt.Errorf("must be a number")
		}
	case FieldTypeBoolean:
		if _, ok := val.(bool); !ok {
			return fmt.Errorf("must be a boolean")
		}
	case FieldTypeTimestamp:
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("must be an RFC 3339 timestamp string")
		}
		if _, err := time.Parse(time.RFC3339, s); err != nil {
			return fmt.Errorf("must be an RFC 3339 timestamp string")
		}
	case FieldTypeEnum:
		s, ok := val.(string)
		if !ok {
			return fmt.Errorf("must be a string")
		}
		if !contains(d.Values, s) {
			return fmt.Errorf("must be one of %v", d.Values)
		}
	case FieldTypeStrings:
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("must be an array of strings")
		}
		for _, elem := range arr {
			if _, ok := elem.(string); !ok {
				return fmt.Errorf("must be an array of strings")
			}
		}
	case FieldTypeEnums:
		arr, ok := val.([]any)
		if !ok {
			return fmt.Errorf("must be an array of strings")
		}
		for _, elem := range arr {
			s, ok := elem.(string)
			if !ok {
				return fmt.Errorf("must be an array of strings")
			}
			if !contains(d.Values, s) {
				return fmt.Errorf("array element %q must be one of %v", s, d.Values)
			}
		}
	case FieldTypeJSON:
		// Any valid JSON value is accepted.
	default:
		return fmt.Errorf("unknown field type %q", d.Type)
	}
	return nil
}

// stringCoercionHint appends an actionable hint to the "must be a string" error
// when the offending value is a bare JSON scalar (number or boolean) that the
// author almost certainly meant as a string. The CLI's `-f key=value` parser
// auto-detects `2387`, `true`, etc. as JSON literals before any server
// round-trip (rawOrString in cmd/kd/fields.go), so an integer-looking value fed
// to a string-typed field (mr iids, ticket numbers, versions) is rejected with
// no path forward unless the author already knows to nest-quote it as
// `key="2387"`. This hint names that fix inline so a failed call is
// self-correcting rather than a dead end (ka-0000001910) — mirroring the
// did-you-mean enrichment on unknownFieldMessage.
//
// The value has already failed the string type assertion, so it is a non-string
// JSON value. We render number and boolean scalars (the values a bare CLI token
// decodes to) with a concrete `key="…"` example; for objects/arrays/null there
// is no obvious scalar re-quoting, so we return no hint and leave the bare
// message.
func stringCoercionHint(name string, val any) string {
	switch v := val.(type) {
	case float64:
		return fmt.Sprintf(` (got a number; wrap the value in quotes to store it as a string, e.g. %s=%q)`, name, formatJSONNumber(v))
	case bool:
		return fmt.Sprintf(` (got a boolean; wrap the value in quotes to store it as a string, e.g. %s=%q)`, name, strconv.FormatBool(v))
	default:
		return ""
	}
}

// formatJSONNumber renders a float64 decoded from JSON back to its most natural
// textual form: integers without a trailing ".0" (so 2387.0 → "2387"), other
// values via the shortest round-trippable representation.
func formatJSONNumber(f float64) string {
	if f == math.Trunc(f) && !math.IsInf(f, 0) {
		return strconv.FormatInt(int64(f), 10)
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

func contains(slice []string, val string) bool {
	for _, s := range slice {
		if s == val {
			return true
		}
	}
	return false
}

// typeMismatchMessage decorates a wrong-value-TYPE rejection (e.g. a bare
// "must be a string" / "must be an integer") with a pointer to `kd fields
// <type>` — the introspection command that lists every field WITH its declared
// type. It is the value-type analogue of unknownFieldMessage's far-miss branch:
// the unknown-field-NAME case already routes the author to the schema
// (ka-0000000365), but a mistyped VALUE still 400'd with only the shape it
// wanted and no cheap way to discover the field's declared type at the point of
// use. That asymmetry is the dominant efficiency papercut in the OSHA-300
// sample (ka-0000004431): agents guess a --field value type, submit, eat the
// 400, and only THEN read the schema. Appending the pointer turns the error
// itself into the affordance.
//
// typeName is the bead type being validated (e.g. "task"), or "" when the
// caller has no type context (ValidateTypeFields with a nil config). When empty the base
// message is returned unchanged — a bogus `kd fields ` pointer would be worse
// than none.
func typeMismatchMessage(base, typeName string) string {
	if typeName == "" {
		return base
	}
	return fmt.Sprintf("%s (run `kd fields %s` to see each field's declared type)", base, typeName)
}

// unknownFieldMessage renders the rejection text for a key that isn't in the
// type schema. It always begins with "unknown field" (so existing callers and
// tests that key on that prefix keep working) and then appends a hint so the
// author isn't left guessing: either a single did-you-mean when a valid field
// is close enough, or a short sorted sample of the valid field names.
//
// Why a hint at all: the bare "unknown field" message sent an agent who typed
// `slack_thread_channel` (the field is `slack_channel`) down a dead end — it
// concluded the capability didn't exist, built a workaround, and filed a
// redundant feature (ka-0000003647). One near-miss name would have fixed it.
//
// The near-miss is deliberately NOT a plain Levenshtein threshold: the
// motivating typo is 7 edits away (a dropped "thread_" token), which no sane
// typo threshold accepts. closestFieldName combines token-set overlap (catches
// snake_case structural variants like slack_thread_channel↔slack_channel) with
// normalized edit similarity (catches ordinary typos like sevrity↔severity),
// so both failure modes get a suggestion without firing on unrelated names.
//
// typeName is the bead type being validated (e.g. "task"), or "" when the
// caller has no type context. When known, the far-miss branch routes the
// author to `kd fields <type>` — the introspection command that lists every
// valid field — instead of dumping a truncated "+N more" sample that leaves
// them guessing at the other 75 names (ka-0000000365). When unknown, the
// far-miss branch falls back to the capped inline sample.
func unknownFieldMessage(key string, defs []FieldDef, typeName string) string {
	names := make([]string, 0, len(defs))
	for _, d := range defs {
		names = append(names, d.Name)
	}
	if len(names) == 0 {
		// Closed schema with no declared fields (e.g. a nil/empty type config):
		// there is nothing to suggest or list, so keep the bare message.
		return "unknown field"
	}

	if suggestion := closestFieldName(key, names); suggestion != "" {
		return fmt.Sprintf(`unknown field (did you mean %q?)`, suggestion)
	}
	// Far miss: no confident suggestion. Prefer pointing at the introspection
	// command over an inline dump — one short, complete pointer beats a
	// truncated list. Fall back to the capped sample only when the type name
	// isn't known (ValidateTypeFields with a nil config).
	if typeName != "" {
		return fmt.Sprintf("unknown field; run `kd fields %s` to see all valid fields", typeName)
	}
	return "unknown field; valid fields: " + sampleFieldNames(names)
}

// sampleFieldNames renders a sorted, deduped, capped list of valid field names
// for the far-miss branch when no type name is available to point at
// `kd fields <type>`. Cap at 10 with a "+N more" suffix so a 89-field type
// like trigger doesn't dump a wall of text into a single error line.
func sampleFieldNames(names []string) string {
	sorted := dedupeSorted(names)
	const maxSample = 10
	if len(sorted) <= maxSample {
		return strings.Join(sorted, ", ")
	}
	return fmt.Sprintf("%s (+%d more)", strings.Join(sorted[:maxSample], ", "), len(sorted)-maxSample)
}

// dedupeSorted returns names sorted ascending with duplicates removed.
func dedupeSorted(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := seen[n]; ok {
			continue
		}
		seen[n] = struct{}{}
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// closestFieldName returns the valid field name nearest to key, or "" when
// nothing is close enough to suggest confidently. It scores each candidate by
// the MAX of two similarities in [0,1] and requires the best to clear a
// threshold:
//
//   - token-set Jaccard over '_'-split tokens: |A∩B| / |A∪B|. This is what
//     catches slack_thread_channel↔slack_channel — they share {slack, channel}
//     out of {slack, thread, channel}, Jaccard 2/3 ≈ 0.67 — even though their
//     edit distance (7) is nowhere near a typo threshold.
//   - normalized edit similarity: 1 - levenshtein/maxLen. This catches ordinary
//     typos (sevrity↔severity ≈ 0.875) that share no whole token.
//
// Taking the max means a candidate wins if it is close by EITHER measure. The
// 0.6 floor is high enough that unrelated names (which share neither a token
// nor most characters) score well below it and produce no suggestion, so the
// far-miss branch falls through to listing valid fields instead.
func closestFieldName(key string, names []string) string {
	key = strings.ToLower(key)
	const threshold = 0.6
	best := ""
	bestScore := threshold
	for _, name := range names {
		score := fieldSimilarity(key, strings.ToLower(name))
		if score > bestScore {
			bestScore = score
			best = name
		}
	}
	return best
}

// fieldSimilarity is max(tokenSetJaccard, normalizedEditSimilarity) in [0,1].
func fieldSimilarity(a, b string) float64 {
	if a == b {
		return 1
	}
	return math.Max(tokenSetJaccard(a, b), normalizedEditSimilarity(a, b))
}

// tokenSetJaccard splits both strings on '_' and returns the Jaccard index of
// the resulting token sets: |intersection| / |union|. Empty on either side → 0.
func tokenSetJaccard(a, b string) float64 {
	at := tokenSet(a)
	bt := tokenSet(b)
	if len(at) == 0 || len(bt) == 0 {
		return 0
	}
	inter := 0
	for tok := range at {
		if _, ok := bt[tok]; ok {
			inter++
		}
	}
	union := len(at) + len(bt) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// tokenSet splits s on '_' into a set of non-empty tokens.
func tokenSet(s string) map[string]struct{} {
	set := make(map[string]struct{})
	for _, tok := range strings.Split(s, "_") {
		if tok != "" {
			set[tok] = struct{}{}
		}
	}
	return set
}

// normalizedEditSimilarity is 1 - levenshtein(a,b)/max(len(a),len(b)), giving a
// similarity in [0,1] that is comparable across names of different lengths.
func normalizedEditSimilarity(a, b string) float64 {
	maxLen := len(a)
	if len(b) > maxLen {
		maxLen = len(b)
	}
	if maxLen == 0 {
		return 1
	}
	return 1 - float64(levenshtein(a, b))/float64(maxLen)
}

// levenshtein computes the Levenshtein edit distance between a and b using the
// standard two-row dynamic-programming table. Inputs are field names (short),
// so the O(len(a)*len(b)) cost is negligible.
func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	curr := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		curr[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			curr[j] = min(curr[j-1]+1, prev[j]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(br)]
}
