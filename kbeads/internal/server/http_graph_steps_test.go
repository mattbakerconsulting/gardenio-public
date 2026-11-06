package server

import (
	"errors"
	"reflect"
	"sort"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
)

// Unit tests for the handleGraph steps in http_graph_steps.go (gd-ubMxMUx6zQ).
// The end-to-end /v1/graph behaviour is covered by the http_graph_*_test.go
// handler tests; these pin each step on its own.

func TestApplyGraphDefaults(t *testing.T) {
	var args model.GraphArgs
	applyGraphDefaults(&args)
	if args.Limit != 500 {
		t.Errorf("Limit = %d, want 500", args.Limit)
	}
	if want := []string{"open", "in_progress", "blocked", "deferred"}; !reflect.DeepEqual(args.Status, want) {
		t.Errorf("Status = %v, want %v", args.Status, want)
	}
	if want := []string{"message", "config", "advice", "role", "event"}; !reflect.DeepEqual(args.ExcludeTypes, want) {
		t.Errorf("ExcludeTypes = %v, want %v", args.ExcludeTypes, want)
	}

	args = model.GraphArgs{Limit: 10, Status: []string{"closed"}, ExcludeTypes: []string{"epic"}}
	applyGraphDefaults(&args)
	if args.Limit != 10 || !reflect.DeepEqual(args.Status, []string{"closed"}) || !reflect.DeepEqual(args.ExcludeTypes, []string{"epic"}) {
		t.Errorf("caller values overwritten: %+v", args)
	}
}

func TestGraphBaseFilter_RoutesLabelsAnyToUserClause(t *testing.T) {
	p := 1
	f := graphBaseFilter(model.GraphArgs{
		Limit:        7,
		Assignee:     "alice",
		Priority:     &p,
		Labels:       []string{"a"},
		LabelsAny:    []string{"project:other"},
		ParentID:     "gd-epic",
		ExcludeTypes: []string{"message", "config"},
	})
	if f.Limit != 7 || f.Assignee != "alice" || f.Priority != &p || f.ParentID != "gd-epic" {
		t.Errorf("scalar fields not copied: %+v", f)
	}
	if len(f.LabelsAny) != 0 {
		t.Errorf("LabelsAny = %v, want empty: it is reserved for the OIDC scope labels", f.LabelsAny)
	}
	if !reflect.DeepEqual(f.LabelsAnyUser, []string{"project:other"}) {
		t.Errorf("LabelsAnyUser = %v", f.LabelsAnyUser)
	}
	if want := []model.BeadType{"message", "config"}; !reflect.DeepEqual(f.ExcludeTypes, want) {
		t.Errorf("ExcludeTypes = %v, want %v", f.ExcludeTypes, want)
	}
}

func TestGraphStatusFilters(t *testing.T) {
	base := model.BeadFilter{Limit: 5}

	t.Run("one per status", func(t *testing.T) {
		fs := graphStatusFilters(base, model.GraphArgs{Status: []string{"open", "closed"}, Query: "q"})
		if len(fs) != 2 {
			t.Fatalf("got %d filters, want 2", len(fs))
		}
		for i, st := range []model.Status{"open", "closed"} {
			if !reflect.DeepEqual(fs[i].Status, []model.Status{st}) || fs[i].Search != "q" || fs[i].Limit != 5 {
				t.Errorf("filter %d = %+v", i, fs[i])
			}
			if fs[i].UpdatedAfter != nil {
				t.Errorf("filter %d has UpdatedAfter without max_age_days", i)
			}
		}
	})

	t.Run("status x type in order", func(t *testing.T) {
		fs := graphStatusFilters(base, model.GraphArgs{Status: []string{"open", "blocked"}, Types: []string{"task", "bug"}})
		var got []string
		for _, f := range fs {
			got = append(got, string(f.Status[0])+"/"+string(f.Type[0]))
		}
		if want := []string{"open/task", "open/bug", "blocked/task", "blocked/bug"}; !reflect.DeepEqual(got, want) {
			t.Errorf("order = %v, want %v", got, want)
		}
	})

	t.Run("max age only on closed", func(t *testing.T) {
		before := time.Now().AddDate(0, 0, -3)
		fs := graphStatusFilters(base, model.GraphArgs{Status: []string{"open", "closed"}, MaxAgeDays: 3})
		if fs[0].UpdatedAfter != nil {
			t.Errorf("open filter got UpdatedAfter")
		}
		if fs[1].UpdatedAfter == nil || fs[1].UpdatedAfter.Before(before) {
			t.Errorf("closed filter UpdatedAfter = %v, want ~%v", fs[1].UpdatedAfter, before)
		}
	})
}

func TestLimitByRelevance(t *testing.T) {
	beads := map[string]*model.Bead{
		"closed":   {ID: "closed", Status: model.StatusClosed},
		"open-p2":  {ID: "open-p2", Status: model.StatusOpen, Priority: 2},
		"open-p0":  {ID: "open-p0", Status: model.StatusOpen, Priority: 0},
		"progress": {ID: "progress", Status: model.StatusInProgress, Priority: 4},
	}
	if got := limitByRelevance(beads, 4); len(got) != 4 {
		t.Errorf("at the limit: kept %d, want 4", len(got))
	}
	got := limitByRelevance(beads, 2)
	var ids []string
	for id := range got {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	if want := []string{"open-p0", "progress"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("kept %v, want %v", ids, want)
	}
}

func TestGraphEdgeSet_AddDepEdges(t *testing.T) {
	inWindow := map[string]*model.Bead{"a": {ID: "a"}, "b": {ID: "b"}}
	batch := &graphBatch{
		depRecords: map[string][]*model.Dependency{
			"a": {
				{BeadID: "a", DependsOnID: "b", Type: model.DepBlocks},
				{BeadID: "a", DependsOnID: "x", Type: model.DepBlocks},
			},
		},
		revDepRecords: map[string][]*model.Dependency{
			"b": {
				{BeadID: "a", DependsOnID: "b", Type: model.DepBlocks}, // same edge as forward: deduped
				{BeadID: "y", DependsOnID: "b", Type: model.DepRelated},
			},
		},
	}

	es := newGraphEdgeSet()
	missing := es.addDepEdges(batch, inWindow, true)
	if len(es.edges) != 3 {
		t.Fatalf("got %d edges, want 3 (one deduped)", len(es.edges))
	}
	for _, e := range es.edges {
		if e.Metadata["source"] != "dep" || e.Type != e.DepType {
			t.Errorf("edge %+v: want dep source and Type == DepType", e)
		}
	}
	if want := map[string]bool{"x": true, "y": true}; !reflect.DeepEqual(missing, want) {
		t.Errorf("missing = %v, want %v", missing, want)
	}

	es = newGraphEdgeSet()
	if missing := es.addDepEdges(batch, inWindow, false); len(missing) != 0 {
		t.Errorf("include_deps off: missing = %v, want empty", missing)
	}

	batch.revDepErr = errors.New("boom")
	es = newGraphEdgeSet()
	es.addDepEdges(batch, inWindow, false)
	if len(es.edges) != 2 {
		t.Errorf("reverse deps failed: got %d edges, want only the 2 forward", len(es.edges))
	}
}

func TestGraphEdgeSet_AddTextRefEdges(t *testing.T) {
	inWindow := map[string]*model.Bead{"a": {ID: "a"}, "b": {ID: "b"}}
	blocks := "blocks"
	refs := []*model.BeadReference{
		{SourceBeadID: "a", TargetBeadID: "b", SourceField: "notes"},
		{SourceBeadID: "a", TargetBeadID: "b", SourceField: "description"},
		{SourceBeadID: "a", TargetBeadID: "b", SourceField: "description", RelationshipType: &blocks},
		{SourceBeadID: "a", TargetBeadID: "gone", SourceField: "notes"},
	}
	es := newGraphEdgeSet()
	es.addTextRefEdges(refs, inWindow)
	if len(es.edges) != 2 {
		t.Fatalf("got %d edges, want 2 (mentions + typed blocks)", len(es.edges))
	}
	m := es.edges[0]
	if m.Type != "mentions" || m.Metadata["source"] != "text" || m.Metadata["source_field"] != "description,notes" {
		t.Errorf("mentions edge = %+v", m)
	}
	if es.edges[1].Type != "blocks" || es.edges[1].DepType != "blocks" {
		t.Errorf("typed edge = %+v", es.edges[1])
	}
}

func TestConnectedClosedCandidates(t *testing.T) {
	inWindow := map[string]*model.Bead{"a": {ID: "a"}}
	edges := []*model.GraphEdge{{Source: "a", Target: "b"}, {Source: "c", Target: "a"}}
	deps := map[string][]*model.Dependency{"a": {{DependsOnID: "d"}, {DependsOnID: "a"}}}
	got := connectedClosedCandidates(edges, deps, inWindow)
	if want := map[string]bool{"b": true, "c": true, "d": true}; !reflect.DeepEqual(got, want) {
		t.Errorf("candidates = %v, want %v", got, want)
	}
}

func TestKeepConnectedClosed(t *testing.T) {
	old := time.Now().AddDate(0, 0, -10)
	recent := time.Now().AddDate(0, 0, -1)
	cases := []struct {
		name   string
		bead   *model.Bead
		maxAge int
		want   bool
	}{
		{"open is not pulled in", &model.Bead{Status: model.StatusOpen, UpdatedAt: recent}, 0, false},
		{"closed, no window", &model.Bead{Status: model.StatusClosed, UpdatedAt: old}, 0, true},
		{"closed inside window", &model.Bead{Status: model.StatusClosed, UpdatedAt: recent}, 5, true},
		{"closed outside window", &model.Bead{Status: model.StatusClosed, UpdatedAt: old}, 5, false},
	}
	for _, c := range cases {
		if got := keepConnectedClosed(c.bead, c.maxAge); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestGraphParentOfAndPromotion(t *testing.T) {
	inWindow := map[string]*model.Bead{
		"epic": {ID: "epic", Type: model.TypeEpic},
		"task": {ID: "task", Type: model.TypeTask},
	}
	batch := &graphBatch{
		depRecords: map[string][]*model.Dependency{
			"child":   {{BeadID: "child", DependsOnID: "explicit", Type: model.DepParentChild}},
			"blocker": {{BeadID: "blocker", DependsOnID: "task", Type: model.DepBlocks}, {BeadID: "blocker", DependsOnID: "epic", Type: model.DepBlocks}},
			"both":    {{BeadID: "both", DependsOnID: "epic", Type: model.DepBlocks}, {BeadID: "both", DependsOnID: "p", Type: model.DepParentChild}},
		},
		revDepRecords: map[string][]*model.Dependency{
			"rp": {{BeadID: "rchild", DependsOnID: "rp", Type: model.DepParentChild}},
		},
	}
	got := graphParentOf(batch, inWindow)
	want := map[string]string{"child": "explicit", "blocker": "epic", "both": "p", "rchild": "rp"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parentOf = %v, want %v", got, want)
	}

	edges := []*model.GraphEdge{
		{Source: "blocker", Target: "epic", Type: "blocks", DepType: "blocks"},
		{Source: "blocker", Target: "task", Type: "blocks", DepType: "blocks"},
	}
	promoteParentChildEdges(edges, got)
	if edges[0].Type != "parent-child" || edges[0].DepType != "parent-child" {
		t.Errorf("epic edge not promoted: %+v", edges[0])
	}
	if edges[1].Type != "blocks" {
		t.Errorf("task edge promoted: %+v", edges[1])
	}
}

func TestBuildGraphNodes(t *testing.T) {
	ts := time.Date(2026, 11, 1, 2, 3, 4, 0, time.UTC)
	beads := map[string]*model.Bead{"a": {ID: "a", Title: "A", Status: model.StatusOpen, Type: model.TypeTask, Description: "d", Notes: "n", CreatedAt: ts, UpdatedAt: ts}}
	batch := &graphBatch{
		labelsMap:  map[string][]string{"a": {"l"}},
		blockedMap: map[string][]string{"a": {"b"}},
		depCounts:  map[string]*model.DependencyCounts{"a": {DependencyCount: 2, DependentCount: 3}},
	}
	n := buildGraphNodes(beads, map[string]string{"a": "p"}, batch, false)[0]
	if n.ParentID != "p" || n.DepCount != 2 || n.DepByCount != 3 || n.CreatedAt != "2026-11-01T02:03:04Z" ||
		!reflect.DeepEqual(n.Labels, []string{"l"}) || !reflect.DeepEqual(n.BlockedBy, []string{"b"}) {
		t.Errorf("node = %+v", n)
	}
	if n.Description != "" || n.Notes != "" {
		t.Errorf("body copied without include_body: %+v", n)
	}
	if n := buildGraphNodes(beads, nil, batch, true)[0]; n.Description != "d" || n.Notes != "n" {
		t.Errorf("include_body: node = %+v", n)
	}
}

func TestAssignAgentsAndAgentNodesWithEdges(t *testing.T) {
	agents := map[string]*model.GraphNode{
		"rostered": {ID: "agent:rostered", IssueType: "agent", Status: "active"},
	}
	beads := map[string]*model.Bead{
		"t1": {ID: "t1", Assignee: "worker", Status: model.StatusInProgress},
		"t2": {ID: "t2", Assignee: "sleeper", Status: model.StatusOpen},
	}
	edges := assignAgents(agents, beads, nil)
	if _, ok := agents["worker"]; !ok {
		t.Errorf("no fallback node for in-progress assignee")
	}
	if _, ok := agents["sleeper"]; ok {
		t.Errorf("fallback node for an open (not in-progress) assignee")
	}
	if len(edges) != 1 || edges[0].Source != "agent:worker" || edges[0].Target != "t1" || edges[0].Type != "assigned_to" {
		t.Fatalf("edges = %+v", edges)
	}
	nodes := agentNodesWithEdges(agents, edges)
	if len(nodes) != 1 || nodes[0].ID != "agent:worker" {
		t.Errorf("agent nodes with edges = %+v, want only agent:worker", nodes)
	}
}

func TestPruneGraph(t *testing.T) {
	nodes := []model.GraphNode{
		{ID: "open", Status: "open", IssueType: "task"},
		{ID: "lonely-open", Status: "open", IssueType: "task"},
		{ID: "closed-near", Status: "closed", IssueType: "task"},
		{ID: "closed-far", Status: "closed", IssueType: "task"},
		{ID: "gate-alone", Status: "open", IssueType: "gate"},
		{ID: "agent:idle", Status: "idle", IssueType: "agent"},
		{ID: "agent:gone", Status: "", IssueType: "agent"},
	}
	edges := []*model.GraphEdge{
		{Source: "open", Target: "closed-near"},
		{Source: "closed-far", Target: "missing"},
	}
	gotNodes, gotEdges := pruneGraph(nodes, edges)
	var ids []string
	for _, n := range gotNodes {
		ids = append(ids, n.ID)
	}
	if want := []string{"open", "lonely-open", "closed-near", "agent:idle"}; !reflect.DeepEqual(ids, want) {
		t.Errorf("kept nodes %v, want %v", ids, want)
	}
	if len(gotEdges) != 1 || gotEdges[0].Target != "closed-near" {
		t.Errorf("kept edges %+v, want only open->closed-near", gotEdges)
	}
}
