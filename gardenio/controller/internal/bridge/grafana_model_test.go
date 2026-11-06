package bridge

import "testing"

func TestNormalizeDashboardJSON_StripsVolatileAndCanonicalizes(t *testing.T) {
	a := []byte(`{"uid":"x","title":"T","version":1,"id":42,"panels":[]}`)
	// Same model, different volatile fields + key order.
	b := []byte(`{"panels":[],"title":"T","uid":"x","version":99,"id":7}`)

	na, err := NormalizeDashboardJSON(a)
	if err != nil {
		t.Fatalf("normalize a: %v", err)
	}
	nb, err := NormalizeDashboardJSON(b)
	if err != nil {
		t.Fatalf("normalize b: %v", err)
	}
	if string(na) != string(nb) {
		t.Fatalf("normalized forms differ:\n a=%s\n b=%s", na, nb)
	}
}

func TestNormalizeDashboardJSON_RejectsNonObject(t *testing.T) {
	if _, err := NormalizeDashboardJSON([]byte(`[1,2,3]`)); err == nil {
		t.Fatal("expected error for non-object JSON")
	}
	if _, err := NormalizeDashboardJSON([]byte(`not json`)); err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestHashDashboardModel_StableAcrossVolatileChanges(t *testing.T) {
	a := []byte(`{"uid":"x","title":"T","version":1,"panels":[{"id":1}]}`)
	b := []byte(`{"uid":"x","title":"T","version":500,"panels":[{"id":1}]}`)
	ha, err := HashDashboardModel(a)
	if err != nil {
		t.Fatalf("hash a: %v", err)
	}
	hb, err := HashDashboardModel(b)
	if err != nil {
		t.Fatalf("hash b: %v", err)
	}
	if ha != hb {
		t.Fatalf("hash changed for volatile-only diff: %s != %s", ha, hb)
	}
}

func TestHashDashboardModel_ChangesOnRealEdit(t *testing.T) {
	a := []byte(`{"uid":"x","title":"T","panels":[{"id":1}]}`)
	b := []byte(`{"uid":"x","title":"T CHANGED","panels":[{"id":1}]}`)
	ha, _ := HashDashboardModel(a)
	hb, _ := HashDashboardModel(b)
	if ha == hb {
		t.Fatal("hash unchanged for a real model edit")
	}
}

func TestParseDashboardMeta(t *testing.T) {
	m, err := ParseDashboardMeta([]byte(`{"uid":"abc","title":"My Dash","version":7}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if m.UID != "abc" || m.Title != "My Dash" || m.Version != 7 {
		t.Fatalf("unexpected meta: %+v", m)
	}
}
