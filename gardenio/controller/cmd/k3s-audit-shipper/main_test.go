package main

import (
	"net/url"
	"strings"
	"testing"
)

func TestJSONLineURL(t *testing.T) {
	u, err := url.Parse(jsonlineURL("http://gardenio-victoria-logs.gardenio.svc:9428/"))
	if err != nil {
		t.Fatal(err)
	}
	if u.Path != "/insert/jsonline" {
		t.Errorf("path = %q", u.Path)
	}
	q := u.Query()
	if q.Get("_stream_fields") != "source,node" || q.Get("_time_field") != "time" || q.Get("_msg_field") != "msg" {
		t.Errorf("query = %v", q)
	}
}

func TestRequiredEnv(t *testing.T) {
	e := &requiredEnv{}
	_ = e.get("K3S_AUDIT_TEST_UNSET_1")
	_ = e.get("K3S_AUDIT_TEST_UNSET_2")
	err := e.err()
	if err == nil || !strings.Contains(err.Error(), "K3S_AUDIT_TEST_UNSET_1") || !strings.Contains(err.Error(), "K3S_AUDIT_TEST_UNSET_2") {
		t.Fatalf("err = %v", err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" a, b ,,c ")
	if strings.Join(got, "|") != "a|b|c" {
		t.Errorf("got %v", got)
	}
	if len(splitList("")) != 0 {
		t.Error("empty input should yield no entries")
	}
}

func TestTenantHeaders(t *testing.T) {
	h, err := tenantHeaders("9", "")
	if err != nil || h["AccountID"] != "9" || h["ProjectID"] != "0" {
		t.Fatalf("got %v, %v", h, err)
	}
	h, err = tenantHeaders(" 12 ", "3")
	if err != nil || h["AccountID"] != "12" || h["ProjectID"] != "3" {
		t.Fatalf("got %v, %v", h, err)
	}
	// 0 is the default tenant every agent-facing reader queries.
	for _, bad := range []string{"0", "", "x", "-1"} {
		if _, err := tenantHeaders(bad, ""); err == nil {
			t.Errorf("account %q accepted", bad)
		}
	}
	if _, err := tenantHeaders("0", ""); err == nil || !strings.Contains(err.Error(), "default tenant") {
		t.Errorf("err = %v", err)
	}
	if _, err := tenantHeaders("9", "p"); err == nil {
		t.Error("bad project accepted")
	}
}
