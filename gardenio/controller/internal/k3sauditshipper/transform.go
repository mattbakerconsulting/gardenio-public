// Package k3sauditshipper ships the k3s kube-apiserver audit log to the in-chart
// VictoriaLogs store (task gd-ZrPUxj8wyU, follow-up to gd-HXRD9yizAE; decision
// gd-bNVBdrmIo5 chose 30 days of retention).
//
// The apiserver writes one audit.k8s.io/v1 Event per line to
// /var/log/k3s-audit/audit.log on each control-plane node. The node is spot and
// the file dies with it, so a DaemonSet pinned to the control plane tails the
// file through a hostPath mount, filters and redacts each event, and POSTs it to
// vlsingle /insert/jsonline.
//
// transform.go turns one audit Event line into the record to ship. It is the
// redaction gate: the audit policy logs admin writes at RequestResponse, so a
// shipped body could carry a ConfigMap's data or a HelmChart's valuesContent
// (reviewer condition on gd-ZrPUxj8wyU). tail.go reads the file across log
// rotation and checkpoints its position; shipper.go batches and delivers.
package k3sauditshipper

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"gardenio/controller/internal/claudemore"
)

// Source is the VictoriaLogs `source` stream field stamped on every record.
const Source = "k3s-audit"

// redacted replaces the value of a sensitive key.
const redacted = "[REDACTED]"

// BodyMode selects what happens to requestObject / responseObject.
type BodyMode string

const (
	// BodiesRedacted ships bodies of non-sensitive kinds with sensitive keys
	// masked, secret shapes scrubbed and an oversize cap applied. Default.
	BodiesRedacted BodyMode = "redacted"
	// BodiesNone ships metadata only: no request or response bodies at all.
	BodiesNone BodyMode = "none"
)

// DefaultMaxBodyBytes caps one shipped body. A larger one is replaced by a
// marker so a single huge apply cannot dominate the store.
const DefaultMaxBodyBytes = 32 * 1024

// Options configures Transform.
type Options struct {
	// Node is stamped as `node` so records from the three servers stay apart.
	Node string
	// Bodies defaults to BodiesRedacted.
	Bodies BodyMode
	// MaxBodyBytes caps a marshaled body; zero uses DefaultMaxBodyBytes.
	MaxBodyBytes int
	// DropReads drops get/list/watch events except those whose user is in
	// KeepReadsUsers. Off by default: the policy already drops the noisiest
	// system readers, and the audit value of "who read what" is real.
	DropReads bool
	// KeepReadsUsers are exact usernames whose reads survive DropReads.
	KeepReadsUsers []string
}

// sensitiveKinds are resources whose bodies are never shipped, whatever the
// policy logged. Secrets and token calls are Metadata in the policy already;
// listing them here keeps the gate from depending on policy order. ConfigMaps
// and the k3s HelmChart objects carry arbitrary operator data (valuesContent
// routinely holds credentials) and the admin identity's writes to them are
// logged in full.
var sensitiveKinds = map[string]bool{
	"secrets":                         true,
	"configmaps":                      true,
	"helmcharts":                      true,
	"helmchartconfigs":                true,
	"tokenreviews":                    true,
	"tokenrequests":                   true,
	"certificatesigningrequests":      true,
	"selfsubjectreviews":              true,
	"subjectaccessreviews":            true,
	"localsubjectaccessreviews":       true,
	"selfsubjectaccessreviews":        true,
	"serviceaccounts/token":           true,
	"pods/exec":                       true,
	"pods/attach":                     true,
	"pods/portforward":                true,
	"externalsecrets":                 true,
	"secretstores":                    true,
	"clustersecretstores":             true,
	"sealedsecrets":                   true,
	"mutatingwebhookconfigurations":   true,
	"validatingwebhookconfigurations": true,
}

// sensitiveKey matches a map key whose value must never be shipped.
//
// It also catches the `*_KEY`, `*_DSN` and `*_PAT` naming conventions (the
// `*_SECRET`, `*_TOKEN` and `*_PASSWORD` ones are covered by their substring), as
// snake/kebab (`DB_DSN`) or camelCase (`dbDsn`). A bare `key` is deliberately not
// matched: a secretKeyRef's `key` names a field, not its value.
var sensitiveKey = regexp.MustCompile(`(?i)(pass(word|wd|phrase)?|secret|token|api[-_]?key|access[-_]?key|credential|private[-_]?key|client[-_]?key|authorization|bearer|cookie|valuesContent|stringData|certificate-?authority-?data|client-?certificate-?data|^data$|binaryData|ssh|htpasswd|[_-](key|dsn|pat)s?$|^(dsn|pat)s?$)|(?-i:[a-z](Key|Dsn|Pat)s?$)`)

// Value-shape patterns, applied to every string value (never to serialized JSON:
// a `token=\S+` match over the whole record can swallow a closing quote and turn
// the line into invalid JSON, which VictoriaLogs drops or rejects).
var (
	// A JWS/JWT compact form: base64url header (always `eyJ`), payload, signature.
	jwtRE = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{6,}\.[A-Za-z0-9_-]{6,}(?:\.[A-Za-z0-9_-]*)?`)
	// `Authorization: Bearer x`, `authorization=Basic x`, `"authorization":"x"`.
	authHeaderRE = regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*(?:(?:bearer|basic|token|digest)\s+)?)[^\s"',;&]+`)
	// A bare `Bearer <token>`; the 16-char floor keeps prose ("bearer of news") out.
	bearerRE = regexp.MustCompile(`(?i)\b(bearer\s+)[A-Za-z0-9._~+/=-]{16,}`)
	// NAME=value / NAME: value for credential-looking names, e.g. an exec argv
	// `command=DB_DSN=postgres://…` or `GITHUB_PAT=ghp_…`. The value stops at a
	// space, quote or `&` so the rest of a URL query survives.
	namedSecretRE = regexp.MustCompile(`(?i)\b([A-Za-z0-9_.-]*(?:[_-](?:key|dsn|pat)|secret|token|passw(?:or)?d|credentials?|dsn)\s*[:=]\s*)[^\s&"']+`)
	// user:password@ in a URL.
	urlUserinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^/\s:@"']+:)[^@/\s"']+@`)
	// A PEM private key, through its END line (or to the end of the string).
	pemKeyRE = regexp.MustCompile(`(?s)-----BEGIN [A-Z ]*PRIVATE KEY-----.*?(?:-----END [A-Z ]*PRIVATE KEY-----|$)`)
)

// redactString scrubs secret shapes from one string value: the local patterns
// first (they keep a delimiter so the surrounding text survives), then the shared
// claudemore contract (sk-/glpat-/ghp_… and `token=`).
func redactString(s string) string {
	if s == "" {
		return s
	}
	s = pemKeyRE.ReplaceAllString(s, claudemore.RedactedValue)
	s = jwtRE.ReplaceAllString(s, claudemore.RedactedValue)
	s = authHeaderRE.ReplaceAllString(s, "${1}"+claudemore.RedactedValue)
	s = bearerRE.ReplaceAllString(s, "${1}"+claudemore.RedactedValue)
	s = namedSecretRE.ReplaceAllString(s, "${1}"+claudemore.RedactedValue)
	s = urlUserinfoRE.ReplaceAllString(s, "${1}"+claudemore.RedactedValue+"@")
	return claudemore.RedactString(s)
}

// redactTree applies redactString to every string value in a decoded JSON tree,
// in place. Map keys are left alone (they are checked by the residual gate in
// transform).
func redactTree(v any) any {
	switch t := v.(type) {
	case string:
		return redactString(t)
	case map[string]any:
		for k, val := range t {
			t[k] = redactTree(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = redactTree(t[i])
		}
		return t
	default:
		return v
	}
}

// Status is the outcome of transforming one audit line.
type Status int

const (
	// Kept: the record is in out and is valid single-line JSON.
	Kept Status = iota
	// Skipped: not an audit event, or dropped by the options. Not an error.
	Skipped
	// Unshippable: the redacted record failed the output gate (it did not parse
	// as one JSON line, or a credential shape survived). The event is dropped, and
	// the shipper counts it, rather than risk a store-side reject that blocks the
	// batch behind it forever.
	Unshippable
)

// Transform converts one audit Event line into the JSONL record to ship. keep is
// false for a line that is not an audit event, that the options drop, or that
// fails the output gate; use TransformStatus to tell those apart.
func Transform(line []byte, opts Options) (out []byte, keep bool) {
	out, st := TransformStatus(line, opts)
	return out, st == Kept
}

// TransformStatus is Transform with the reason a line was not kept.
func TransformStatus(line []byte, opts Options) (out []byte, st Status) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 || line[0] != '{' {
		return nil, Skipped
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.UseNumber()
	var ev map[string]any
	if err := dec.Decode(&ev); err != nil {
		return nil, Skipped
	}
	// Only audit events: a stray JSON line must not reach the store as one.
	if k, _ := ev["kind"].(string); k != "Event" {
		return nil, Skipped
	}

	verb, _ := ev["verb"].(string)
	user := nestedString(ev, "user", "username")
	if opts.DropReads && isRead(verb) && !contains(opts.KeepReadsUsers, user) {
		return nil, Skipped
	}

	rec := map[string]any{
		"source": Source,
		"time":   firstString(ev, "stageTimestamp", "requestReceivedTimestamp"),
	}
	if opts.Node != "" {
		rec["node"] = opts.Node
	}
	for _, k := range []string{
		"auditID", "stage", "level", "verb", "requestURI", "sourceIPs", "userAgent",
		"requestReceivedTimestamp", "stageTimestamp", "annotations",
	} {
		if v, ok := ev[k]; ok {
			rec[k] = v
		}
	}
	if u, ok := ev["impersonatedUser"].(map[string]any); ok {
		// Same trim as user: `extra` is authenticator-supplied.
		rec["impersonatedUser"] = pick(u, "username", "uid", "groups")
	}
	if u, ok := ev["user"].(map[string]any); ok {
		// `extra` can carry authenticator-supplied claims; username/groups/uid
		// are what an audit reader needs.
		rec["user"] = pick(u, "username", "uid", "groups")
	}
	if uri, ok := rec["requestURI"].(string); ok {
		rec["requestURI"] = decodeQuery(uri)
	}
	ref, _ := ev["objectRef"].(map[string]any)
	if ref != nil {
		rec["objectRef"] = ref
	}
	if rs, ok := ev["responseStatus"].(map[string]any); ok {
		rec["responseStatus"] = pick(rs, "code", "status", "reason", "message")
	}

	resource := resourceKey(ref)
	rec["msg"] = message(verb, resource, ref, user, ev)

	attachBody(rec, ev, "requestObject", resource, ref, opts)
	attachBody(rec, ev, "responseObject", resource, ref, opts)

	// Last gate: scrub secret-shaped substrings (sk-…, glpat-…, JWTs, Bearer,
	// NAME_KEY=…, token=…) from anything the key-based pass did not catch, e.g. an
	// `exec` command in requestURI or a message echoing a credential. This runs
	// per string value on the decoded tree, then the result is serialized, so a
	// greedy pattern can only ever eat part of one string.
	redactTree(rec)
	b, err := marshalLine(rec)
	if err != nil || !shippable(b) {
		return nil, Unshippable
	}
	return b, Kept
}

// marshalLine serializes rec as one JSON line without HTML escaping (so the
// `<redacted>` marker stays readable in the store).
func marshalLine(rec map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(rec); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// shippable is the output gate: b must be exactly one JSON object on one line,
// and must hold no provider-credential shape. The second check also covers map
// keys, which redactTree does not rewrite.
func shippable(b []byte) bool {
	if len(b) == 0 || bytes.IndexByte(b, '\n') >= 0 {
		return false
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(b, &probe); err != nil {
		return false
	}
	return !claudemore.ContainsSecretShape(b)
}

// attachBody copies ev[key] into rec subject to the body mode and kind rules.
func attachBody(rec, ev map[string]any, key, resource string, ref map[string]any, opts Options) {
	body, ok := ev[key]
	if !ok || body == nil {
		return
	}
	switch {
	case opts.Bodies == BodiesNone:
		rec[key+"Omitted"] = "bodies-disabled"
		return
	case isSensitiveKind(resource, ref):
		rec[key+"Omitted"] = "sensitive-kind"
		return
	}
	scrubbed := scrub(body)
	if m, ok := scrubbed.(map[string]any); ok {
		if md, ok := m["metadata"].(map[string]any); ok {
			// managedFields is large, adds nothing to an audit, and lists field
			// owners; annotations such as last-applied-configuration embed the
			// whole prior object, which would bypass the scrub above.
			delete(md, "managedFields")
			if an, ok := md["annotations"].(map[string]any); ok {
				delete(an, "kubectl.kubernetes.io/last-applied-configuration")
			}
		}
	}
	max := opts.MaxBodyBytes
	if max <= 0 {
		max = DefaultMaxBodyBytes
	}
	b, err := json.Marshal(scrubbed)
	if err != nil {
		rec[key+"Omitted"] = "unmarshalable"
		return
	}
	if len(b) > max {
		rec[key+"Omitted"] = fmt.Sprintf("oversize:%d>%d bytes", len(b), max)
		return
	}
	rec[key] = scrubbed
}

// scrub masks the value of every sensitive key, recursively. A container env
// entry {"name":"X_TOKEN","value":"…"} is masked by its name, since the secret
// is in `value`, not under a key that looks sensitive.
func scrub(v any) any {
	switch t := v.(type) {
	case map[string]any:
		if name, ok := t["name"].(string); ok && sensitiveKey.MatchString(name) {
			if _, has := t["value"]; has {
				t["value"] = redacted
			}
		}
		for k, val := range t {
			if sensitiveKey.MatchString(k) {
				switch val.(type) {
				case nil, bool, json.Number:
					// A flag or number carries no secret; keep it readable.
				default:
					t[k] = redacted
					continue
				}
			}
			t[k] = scrub(val)
		}
		return t
	case []any:
		for i := range t {
			t[i] = scrub(t[i])
		}
		return t
	default:
		return v
	}
}

func isSensitiveKind(resource string, ref map[string]any) bool {
	if sensitiveKinds[resource] {
		return true
	}
	// A subresource of a sensitive kind, e.g. secrets/status.
	base, _, _ := strings.Cut(resource, "/")
	if sensitiveKinds[base] {
		return true
	}
	// Whole API groups that exist to move credentials or authn decisions.
	switch group, _ := ref["apiGroup"].(string); group {
	case "authentication.k8s.io", "authorization.k8s.io", "certificates.k8s.io",
		"external-secrets.io", "bitnami.com", "helm.cattle.io":
		return true
	}
	return false
}

// resourceKey is "resource" or "resource/subresource" from objectRef.
func resourceKey(ref map[string]any) string {
	if ref == nil {
		return ""
	}
	r, _ := ref["resource"].(string)
	if sub, _ := ref["subresource"].(string); sub != "" {
		return r + "/" + sub
	}
	return r
}

// message is the one-line summary VictoriaLogs treats as _msg.
func message(verb, resource string, ref map[string]any, user string, ev map[string]any) string {
	target := resource
	if ref != nil {
		ns, _ := ref["namespace"].(string)
		name, _ := ref["name"].(string)
		switch {
		case ns != "" && name != "":
			target = fmt.Sprintf("%s %s/%s", resource, ns, name)
		case name != "":
			target = fmt.Sprintf("%s %s", resource, name)
		case ns != "":
			target = fmt.Sprintf("%s in %s", resource, ns)
		}
	}
	if target == "" {
		target, _ = ev["requestURI"].(string)
	}
	msg := fmt.Sprintf("%s %s by %s", verb, target, user)
	if code := nestedNumber(ev, "responseStatus", "code"); code != "" {
		msg += " -> " + code
	}
	return msg
}

// decodeQuery percent-decodes the query of a request URI. An exec call carries
// its argv there (command=curl&command=token%3D…), and the redaction pass keys
// on the plain `token=` / `sk-…` shapes, which the encoded form hides.
func decodeQuery(uri string) string {
	path, query, found := strings.Cut(uri, "?")
	if !found {
		return uri
	}
	dec, err := url.QueryUnescape(query)
	if err != nil {
		return uri
	}
	return path + "?" + dec
}

func isRead(verb string) bool {
	return verb == "get" || verb == "list" || verb == "watch"
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func pick(m map[string]any, keys ...string) map[string]any {
	out := make(map[string]any, len(keys))
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out
}

func firstString(m map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, _ := m[k].(string); s != "" {
			return s
		}
	}
	return ""
}

func nestedString(m map[string]any, keys ...string) string {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	s, _ := cur.(string)
	return s
}

func nestedNumber(m map[string]any, keys ...string) string {
	cur := any(m)
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = mm[k]
	}
	if n, ok := cur.(json.Number); ok {
		return n.String()
	}
	return ""
}
