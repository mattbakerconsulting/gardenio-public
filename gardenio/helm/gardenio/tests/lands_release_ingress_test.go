package tests

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Drift test for gd-YOpXsJJYKc. A tenant flow needs both ends open: the land
// fence's egress (landns.DefaultPeers, built in Go by the controller) and the
// release namespace's ingress (this chart). No unit test sees both sides, and a
// mismatch shows up only as a tenant pod that silently cannot reach a service
// once networkPolicies.enabled is on, so this test reads DefaultPeers from the
// controller source and requires the chart's land ingress rules, per component,
// to name exactly those ports.

// landPeersFromSource parses landns.DefaultPeers from the controller source:
// component name -> sorted TCP ports. Also returns landns.CoopPort.
func landPeersFromSource(t *testing.T) (map[string][]int, int) {
	t.Helper()
	path := filepath.Join(chartDir(t), "..", "..", "controller", "internal", "lands", "landns", "landns.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	consts := map[string]int{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(\w+Port)\s*=\s*(\d+)\s*$`).FindAllStringSubmatch(src, -1) {
		consts[m[1]], _ = strconv.Atoi(m[2])
	}
	start := strings.Index(src, "func DefaultPeers()")
	if start < 0 {
		t.Fatalf("%s: no func DefaultPeers", path)
	}
	body := src[start:]
	body = body[:strings.Index(body, "\n}\n")]
	peers := map[string][]int{}
	for _, m := range regexp.MustCompile(`c\("([\w-]+)",([^)]*)\)`).FindAllStringSubmatch(body, -1) {
		for _, p := range strings.Split(m[2], ",") {
			p = strings.TrimSpace(p)
			n, err := strconv.Atoi(p)
			if err != nil {
				var ok bool
				if n, ok = consts[p]; !ok {
					t.Fatalf("DefaultPeers %s: port %q is neither a literal nor a *Port const", m[1], p)
				}
			}
			peers[m[1]] = append(peers[m[1]], n)
		}
		sort.Ints(peers[m[1]])
	}
	if len(peers) == 0 {
		t.Fatalf("%s: parsed no peers from DefaultPeers", path)
	}
	coop, ok := consts["CoopPort"]
	if !ok {
		t.Fatalf("%s: no CoopPort const", path)
	}
	return peers, coop
}

type landNetpol struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		PodSelector struct {
			MatchLabels map[string]string `yaml:"matchLabels"`
		} `yaml:"podSelector"`
		Ingress []landNetpolRule `yaml:"ingress"`
		Egress  []landNetpolRule `yaml:"egress"`
	} `yaml:"spec"`
}

type landNetpolRule struct {
	From  []landNetpolPeer `yaml:"from"`
	To    []landNetpolPeer `yaml:"to"`
	Ports []struct {
		Protocol string `yaml:"protocol"`
		Port     int    `yaml:"port"`
	} `yaml:"ports"`
}

type landNetpolPeer struct {
	NamespaceSelector *struct {
		MatchExpressions []struct {
			Key      string `yaml:"key"`
			Operator string `yaml:"operator"`
		} `yaml:"matchExpressions"`
	} `yaml:"namespaceSelector"`
}

// landRulePorts returns the TCP ports of the rules whose only peer is every
// land namespace (gardenio.io/land Exists).
func landRulePorts(t *testing.T, policy string, rules []landNetpolRule, peers func(landNetpolRule) []landNetpolPeer) []int {
	var ports []int
	for _, r := range rules {
		land := false
		for _, p := range peers(r) {
			if p.NamespaceSelector == nil {
				continue
			}
			for _, e := range p.NamespaceSelector.MatchExpressions {
				if e.Key == "gardenio.io/land" && e.Operator == "Exists" {
					land = true
				}
			}
		}
		if !land {
			continue
		}
		if len(peers(r)) != 1 {
			t.Errorf("%s: land rule has %d peers, want the land namespaceSelector alone", policy, len(peers(r)))
		}
		for _, p := range r.Ports {
			if p.Protocol != "TCP" {
				t.Errorf("%s: land rule port %d is %s, want TCP", policy, p.Port, p.Protocol)
			}
			ports = append(ports, p.Port)
		}
	}
	sort.Ints(ports)
	return ports
}

// renderLandPolicies renders every release-side NetworkPolicy a land flow
// crosses and returns, per component, the ports its ingress admits from land
// namespaces, plus the ports coopmux's egress opens to them.
func renderLandPolicies(t *testing.T, flags []string) (map[string][]int, []int) {
	t.Helper()
	ingress := map[string][]int{}
	var coopmuxEgress []int
	for _, tmpl := range []string{
		"templates/networkpolicies/networkpolicies.yaml",
		"templates/coopmux/networkpolicy.yaml",
		"templates/networkpolicies/default-deny-egress.yaml",
	} {
		dec := yaml.NewDecoder(strings.NewReader(helmTemplateNS(t, "gardenio", tmpl, flags...)))
		for {
			var np landNetpol
			if err := dec.Decode(&np); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatalf("%s: %v", tmpl, err)
			}
			if np.Kind != "NetworkPolicy" {
				continue
			}
			comp := np.Spec.PodSelector.MatchLabels["app.kubernetes.io/component"]
			if p := landRulePorts(t, np.Metadata.Name, np.Spec.Ingress, func(r landNetpolRule) []landNetpolPeer { return r.From }); len(p) > 0 {
				if comp == "" {
					t.Errorf("%s admits land namespaces but selects no app.kubernetes.io/component", np.Metadata.Name)
				}
				ingress[comp] = append(ingress[comp], p...)
				sort.Ints(ingress[comp])
			}
			if p := landRulePorts(t, np.Metadata.Name, np.Spec.Egress, func(r landNetpolRule) []landNetpolPeer { return r.To }); len(p) > 0 {
				if comp != "coopmux" {
					t.Errorf("%s (component %q) opens egress to land namespaces; only coopmux may", np.Metadata.Name, comp)
				}
				coopmuxEgress = append(coopmuxEgress, p...)
			}
		}
	}
	return ingress, coopmuxEgress
}

func TestLandNamespaces_ReleaseIngressMatchesDefaultPeers(t *testing.T) {
	t.Parallel()
	peers, coopPort := landPeersFromSource(t)
	on := []string{
		"agents.enabled=true", "coopmux.enabled=true", "nats.enabled=true",
		"litellm.enabled=true", "slackBridge.enabled=true",
		"networkPolicies.enabled=true", "networkPolicies.defaultDenyEgress=true",
		"landNamespaces.enabled=true",
	}

	ingress, egress := renderLandPolicies(t, on)
	if !reflect.DeepEqual(ingress, peers) {
		t.Errorf("release ingress from land namespaces, per component = %v\nlandns.DefaultPeers = %v\n"+
			"(gd-YOpXsJJYKc: change both, or tenant pods lose or gain a peer on one side only)", ingress, peers)
	}
	if want := []int{coopPort}; !reflect.DeepEqual(egress, want) {
		t.Errorf("coopmux egress to land namespaces = %v, want %v (landns.CoopPort, the one port land-fence admits)", egress, want)
	}

	ingress, egress = renderLandPolicies(t, on[:len(on)-1])
	if len(ingress) != 0 || len(egress) != 0 {
		t.Errorf("landNamespaces off still renders land rules: ingress %v, coopmux egress %v", ingress, egress)
	}
}
