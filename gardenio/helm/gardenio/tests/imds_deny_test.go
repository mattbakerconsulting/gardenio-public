package tests

import (
	"strings"
	"testing"
)

const imdsTpl = "templates/networkpolicies/imds-deny.yaml"

func imdsDocs(t *testing.T, setFlags ...string) []string {
	t.Helper()
	out := helmTemplate(t, imdsTpl, setFlags...)
	var docs []string
	for doc := range strings.SplitSeq(out, "\n---\n") {
		if strings.Contains(doc, "kind: NetworkPolicy") {
			docs = append(docs, doc)
		}
	}
	return docs
}

func docFor(docs []string, ns, name string) string {
	for _, d := range docs {
		if strings.Contains(d, "namespace: "+ns+"\n") && strings.Contains(d, "name: "+name+"\n") {
			return d
		}
	}
	return ""
}

// Off by default: nothing renders. Renders the whole chart, because
// `helm template --show-only` errors on a template that renders nothing.
func TestIMDSDeny_OffByDefault(t *testing.T) {
	t.Parallel()
	if out := helmTemplate(t, ""); strings.Contains(out, "gardenio.io/policy: imds-deny") {
		t.Fatal("imdsDeny is off by default, but an imds-deny policy rendered")
	}
}

// Every listed namespace gets a policy that allows all egress except the IMDS
// addresses, so turning it on cannot break anything but IMDS.
func TestIMDSDeny_AllowsEverythingButIMDS(t *testing.T) {
	t.Parallel()
	docs := imdsDocs(t, "imdsDeny.enabled=true",
		"imdsDeny.namespaces={gardenio,arc-runners,gc-dogfood}",
		"imdsDeny.exemptLabelNamespaces={gardenio}")
	for _, ns := range []string{"gardenio", "arc-runners", "gc-dogfood"} {
		d := docFor(docs, ns, "imds-deny")
		if d == "" {
			t.Fatalf("no imds-deny policy in %s", ns)
		}
		for _, want := range []string{"- Egress", "cidr: 0.0.0.0/0", "- 169.254.169.254/32", "cidr: ::/0", "- fd00:ec2::254/128", "namespaceSelector: {}"} {
			if !strings.Contains(d, want) {
				t.Errorf("%s imds-deny missing %q:\n%s", ns, want, d)
			}
		}
		if strings.Contains(d, "- Ingress") {
			t.Errorf("%s imds-deny must not restrict ingress", ns)
		}
	}
}

// The exemption label is honoured only in an exempt-label namespace, and the
// agents policy keeps agent pods denied there even if they carry the label.
// gc-dogfood (where k8s-live can create pods) never honours it.
func TestIMDSDeny_ExemptionScope(t *testing.T) {
	t.Parallel()
	docs := imdsDocs(t, "imdsDeny.enabled=true",
		"imdsDeny.namespaces={gardenio,gc-dogfood}",
		"imdsDeny.exemptLabelNamespaces={gardenio}")
	g := docFor(docs, "gardenio", "imds-deny")
	if !strings.Contains(g, "key: gardenio.io/imds") || !strings.Contains(g, "operator: NotIn") {
		t.Fatalf("gardenio imds-deny must exempt gardenio.io/imds=allow:\n%s", g)
	}
	a := docFor(docs, "gardenio", "imds-deny-agents")
	if !strings.Contains(a, "key: gardenio.io/agent") || !strings.Contains(a, "operator: Exists") {
		t.Fatalf("gardenio must have imds-deny-agents selecting gardenio.io/agent:\n%s", a)
	}
	d := docFor(docs, "gc-dogfood", "imds-deny")
	if strings.Contains(d, "gardenio.io/imds") || !strings.Contains(d, "podSelector: {}") {
		t.Fatalf("gc-dogfood must select every pod and ignore the exemption label:\n%s", d)
	}
	if docFor(docs, "gc-dogfood", "imds-deny-agents") != "" {
		t.Fatal("gc-dogfood needs no agents policy: its imds-deny already selects every pod")
	}
}

// The live overlay turns it on for the namespaces in spec §4 and never lists
// kube-system (cluster-autoscaler and the cloud controller need IMDS) or
// cert-manager (DNS01 uses the node role). Nor ziti: the namespace is deleted
// with ziti (gd-xNMLrsgajy), and a policy into a missing namespace fails every
// helm upgrade.
func TestIMDSDeny_OverlayNamespaces(t *testing.T) {
	t.Parallel()
	out := helmTemplateShowOnlyWithValues(t, imdsTpl, []string{"../../../deploy-values/gardenio-k3s.yaml"})
	for _, ns := range []string{"gardenio", "arc-runners", "arc-systems", "beads", "gc-dogfood", "operators", "default", "headlamp", "public-edge"} {
		if !strings.Contains(out, "namespace: "+ns+"\n") {
			t.Errorf("overlay must deny IMDS in %s", ns)
		}
	}
	if strings.Contains(out, "namespace: ziti\n") {
		t.Fatal("ziti is switched off and its namespace deleted; an imds-deny there fails helm upgrade")
	}
	if strings.Contains(out, "namespace: kube-system\n") {
		t.Fatal("kube-system must not get an IMDS deny")
	}
	// cert-manager's DNS01 solver reaches Route53 through the node role over IMDS.
	if strings.Contains(out, "namespace: cert-manager\n") {
		t.Fatal("cert-manager must not get an IMDS deny until DNS01 stops using the node role")
	}
}
