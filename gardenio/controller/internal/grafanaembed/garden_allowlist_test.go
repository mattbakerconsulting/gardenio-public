package grafanaembed

import (
	"errors"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

// The garden allowlists against the shipped registry (plan gd-gh5J0vVbJP step
// 2.4, gd-5HipEi6w2o): what the Costs panel may ask for, and that nothing else
// reaches a frame URL. TestBuild_Rejections covers each set shape on testSets;
// these cover the near misses a request could carry to costs-fleet,
// costs-project and health-land (step 4.1, gd-SKVLOuGToB), and FuzzBuild_StaysInAllowlist holds every accepted query to
// the URL shape (its seeds run under go test; `go test -fuzz` explores more).

// Near misses of every closed set are refused on the shipped sets: case,
// whitespace, encodings, a second value smuggled into one, a set name that is
// almost one.
func TestBuild_ShippedRegistryRejectsNearMisses(t *testing.T) {
	for _, q := range []Query{
		{Set: "Costs-Fleet"}, {Set: "costs-fleet "}, {Set: "costs-fleet/"}, {Set: "costs"}, {Set: "costs-fleet.json"}, {Set: "../costs-fleet"},
		{Set: "costs-fleet", Range: "Today"}, {Set: "costs-fleet", Range: "TODAY"}, {Set: "costs-fleet", Range: " 7d"}, {Set: "costs-fleet", Range: "7d "},
		{Set: "costs-fleet", Range: "30d,7d"}, {Set: "costs-fleet", Range: "now-30d"}, {Set: "costs-fleet", Range: "7d&to=now-1y"}, {Set: "costs-fleet", Range: "0"},
		{Set: "costs-fleet", Theme: "Dark"}, {Set: "costs-fleet", Theme: "dark "}, {Set: "costs-fleet", Theme: "system"}, {Set: "costs-fleet", Theme: "dark&kiosk=off"},
		{Set: "costs-fleet", Project: "$__all"}, {Set: "costs-fleet", Project: "fleet"}, {Set: "costs-fleet", Land: true},
		{Set: "costs-project"}, {Set: "costs-project", Project: " gardenio"}, {Set: "costs-project", Project: "gardenio\n"}, {Set: "costs-project", Project: "gardenio\x00"},
		{Set: "costs-project", Project: "gärdenio"}, {Set: "costs-project", Project: "gardenio%26var-x%3D1"}, {Set: "costs-project", Project: "gardenio#x"},
		{Set: "costs-project", Project: "garden/io"}, {Set: "costs-project", Project: "$__all"}, {Set: "costs-project", Project: "${project}"},
		{Set: "costs-project", Project: strings.Repeat("a", 64)}, {Set: "costs-project", Project: "gardenio", Range: "1y"},
		{Set: "costs-fleet", Agents: []string{"gardenio-run-1"}}, {Set: "costs-project", Project: "gardenio", Agents: []string{"gardenio-run-1"}},
		{Set: "agent"}, {Set: "agent", Agents: []string{""}}, {Set: "agent", Agents: []string{".*"}}, {Set: "agent", Agents: []string{"a|b"}},
		{Set: "agent", Agents: []string{"Gardenio-Run"}}, {Set: "agent", Agents: []string{"run.x"}}, {Set: "agent", Agents: []string{"run_1"}},
		{Set: "agent", Agents: []string{"run-1&var-project=x"}}, {Set: "agent", Agents: []string{"run-1\n"}}, {Set: "agent", Agents: []string{"$__all"}},
		{Set: "agent", Agents: []string{strings.Repeat("a", 64)}}, {Set: "agent", Agents: []string{"run-1"}, Project: "gardenio"},
		{Set: "agent", Agents: []string{"run-1"}, Land: true}, {Set: "gardener", Agents: []string{"run-1", "run-2", ")|(.*"}},
		{Set: "gardener", Agents: runNames(AgentsMax + 1)},
		{Set: "health"}, {Set: "health-land"}, {Set: "Health-Land", Project: "gardenio"}, {Set: "health-land", Project: "$__all"},
		{Set: "health-land", Project: "my-land&var-project=other"}, {Set: "health-land", Project: "my-land", Theme: "auto"},
	} {
		res, err := Build(testBase, q)
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("Build(%#v) = %d panels, %v; want ErrInvalid", q, len(res.Panels), err)
		}
		if res.OpenDashboardURL != "" || len(res.Panels) != 0 {
			t.Errorf("Build(%#v) refused but still answered %+v", q, res)
		}
	}
}

// runNames is n valid agent names, run-1..run-n, so a cap case fails on the
// count alone.
func runNames(n int) []string {
	names := make([]string, n)
	for i := range names {
		names[i] = "run-" + strconv.Itoa(i+1)
	}
	return names
}

// The AgentsMax cap is the only thing between AgentsMax and AgentsMax+1 valid
// runs: the first is a full gardener card, the next is refused.
func TestBuild_GardenerTakesAgentsMaxRuns(t *testing.T) {
	res, err := Build(testBase, Query{Set: "gardener", Agents: runNames(AgentsMax)})
	if err != nil || len(res.Agents) != AgentsMax {
		t.Fatalf("Build(gardener, %d runs) = %d agents, %v; want %d, nil", AgentsMax, len(res.Agents), err, AgentsMax)
	}
	if _, err := Build(testBase, Query{Set: "gardener", Agents: runNames(AgentsMax + 1)}); !errors.Is(err, ErrInvalid) {
		t.Errorf("Build(gardener, %d runs) = %v, want ErrInvalid", AgentsMax+1, err)
	}
}

// Every chip and theme the panel can send is accepted on both shipped sets, and
// the Costs panel's own defaults (no range, no theme) are the 7 days and dark
// it shows.
func TestBuild_ShippedRegistryAcceptsEveryChip(t *testing.T) {
	for _, set := range []string{"costs-fleet", "costs-project", "health-land"} {
		project := ""
		if set != "costs-fleet" {
			project = "gardenio"
		}
		for _, r := range append(Ranges(), Range{Name: ""}) {
			for _, th := range []string{"", "dark", "light"} {
				res, err := Build(testBase, Query{Set: set, Project: project, Range: r.Name, Theme: th})
				if err != nil {
					t.Errorf("%s range %q theme %q: %v", set, r.Name, th, err)
					continue
				}
				wantR, wantT := r.Name, th
				if wantR == "" {
					wantR = DefaultRange
				}
				if wantT == "" {
					wantT = string(DefaultTheme)
				}
				if res.Range != wantR || res.Theme != wantT || res.Project != project || len(res.Panels) == 0 {
					t.Errorf("%s range %q theme %q: %+v", set, r.Name, th, res)
				}
			}
		}
	}
}

// FuzzBuild_StaysInAllowlist: whatever a request carries, Build either refuses
// it with ErrInvalid or answers URLs on the base's solo and pop-out paths of the
// registry's dashboard, with only the known params, a registry panel id, a
// chip's from/to, a known theme and a project that is $__all or a DNS label.
func FuzzBuild_StaysInAllowlist(f *testing.F) {
	for _, s := range []struct {
		set, project, rng, theme string
		land                     bool
		agent                    string
	}{
		{"costs-fleet", "", "", "", false, ""}, {"costs-fleet", "", "today", "light", false, ""}, {"costs-project", "gardenio", "30d", "dark", false, ""},
		{"costs-project", "my-land", "7d", "", true, ""}, {"costs-fleet", "$__all", "", "", false, ""}, {"costs-project", "a&var-b=c", "", "", false, ""},
		{"costs-project", "gardenio", "7d&from=now-1y", "", false, ""}, {"nope", "", "", "", false, ""}, {"costs-fleet", "", "", "auto", false, ""},
		{"agent", "", "7d", "", false, "gardenio-42-agent-and-rfu"}, {"gardener", "", "", "", false, "run-1|run-2"}, {"agent", "", "", "", false, ".*"},
		{"agent", "", "", "", false, "run-1&var-agent=x"}, {"costs-fleet", "", "", "", false, "run-1"},
		{"health-land", "gardenio", "today", "dark", false, ""}, {"health-land", "my-land", "30d", "", true, ""}, {"health-land", "", "", "", true, ""},
	} {
		f.Add(s.set, s.project, s.rng, s.theme, s.land, s.agent)
	}
	f.Fuzz(func(t *testing.T, set, project, rng, theme string, land bool, agent string) {
		var agents []string
		if agent != "" {
			agents = strings.Split(agent, ",")
		}
		res, err := Build(testBase, Query{Set: set, Project: project, Range: rng, Theme: theme, Land: land, Agents: agents})
		if err != nil {
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("a refusal that is not ErrInvalid: %v", err)
			}
			return
		}
		s, ok := LookupSet(set)
		if !ok {
			t.Fatalf("built unknown set %q", set)
		}
		ids := map[string]bool{}
		for _, p := range s.Panels {
			ids[strconv.Itoa(p.ID)] = true
		}
		r, _ := ParseRange(res.Range)
		check := func(raw, path, idKey string) {
			u, err := url.Parse(raw)
			if err != nil || u.Scheme+"://"+u.Host != testBase || u.Path != path || u.Fragment != "" {
				t.Fatalf("%q is not on %s%s", raw, testBase, path)
			}
			q := u.Query()
			for k := range q {
				switch k {
				case idKey, "kiosk", "theme", "from", "to", "timezone", "var-project", "var-agent":
				default:
					t.Fatalf("%q carries a param outside the allowlist: %s", raw, k)
				}
				if len(q[k]) != 1 {
					t.Fatalf("%q repeats %s", raw, k)
				}
			}
			if !ids[q.Get(idKey)] || q.Get("from") != r.From || q.Get("to") != r.To || q.Get("timezone") != Timezone || q.Get("theme") != res.Theme {
				t.Fatalf("%q: panel, range or theme outside the sets", raw)
			}
			if s.PerAgent {
				if q.Has("var-project") {
					t.Fatalf("%q: a per-agent set sent var-project", raw)
				}
				for _, a := range strings.Split(q.Get("var-agent"), "|") {
					if !ValidAgent(a) {
						t.Fatalf("%q: var-agent part %q", raw, a)
					}
				}
			} else if v := q.Get("var-project"); q.Has("var-agent") || (v != allProjects && !ValidProject(v)) {
				t.Fatalf("%q: var-project %q", raw, v)
			}
		}
		for _, c := range res.Panels {
			check(c.Src, "/d-solo/"+s.UID, "panelId")
			check(c.OpenURL, "/d/"+s.UID, "viewPanel")
		}
	})
}
