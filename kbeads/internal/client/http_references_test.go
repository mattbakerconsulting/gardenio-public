package client

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

func TestHTTPClient_ListBeadsMentioning_URLAndDecode(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"references": [
				{"id":"gd-ldkpaoah1s","title":"alpha","type":"task","status":"open","source_field":"description"},
				{"id":"gd-hsr6nhjuuy","title":"beta","type":"epic","status":"in_progress","source_field":"comment:7"}
			],
			"total": 2,
			"limit": 25,
			"offset": 10
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.ListBeadsMentioning(context.Background(), "gd-target00001", 25, 10, nil)
	if err != nil {
		t.Fatalf("ListBeadsMentioning: %v", err)
	}

	if h.method != http.MethodGet {
		t.Errorf("method = %q, want GET", h.method)
	}
	if h.path != "/v1/beads/gd-target00001/mentioned-by" {
		t.Errorf("path = %q, want /v1/beads/gd-target00001/mentioned-by", h.path)
	}
	if !strings.Contains(h.query, "limit=25") || !strings.Contains(h.query, "offset=10") {
		t.Errorf("query = %q, want limit=25 and offset=10", h.query)
	}
	if resp.Total != 2 || len(resp.References) != 2 {
		t.Errorf("total/len = %d/%d, want 2/2", resp.Total, len(resp.References))
	}
	if resp.References[0].ID != "gd-ldkpaoah1s" || resp.References[0].SourceField != "description" {
		t.Errorf("ref 0 = %+v", resp.References[0])
	}
	if resp.References[1].SourceField != "comment:7" {
		t.Errorf("ref 1 source_field = %q, want comment:7", resp.References[1].SourceField)
	}
}

func TestHTTPClient_ListBeadsMentioning_OmitsZeroParams(t *testing.T) {
	h := &testHandler{
		responseBody: `{"references":[],"total":0,"limit":50,"offset":0}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	if _, err := c.ListBeadsMentioning(context.Background(), "gd-target00001", 0, 0, nil); err != nil {
		t.Fatalf("ListBeadsMentioning: %v", err)
	}
	if h.query != "" {
		t.Errorf("query = %q, want empty (zero limit/offset should not appear in URL)", h.query)
	}
}

func TestHTTPClient_ListBeadReferencesFrom_URLAndDecode(t *testing.T) {
	h := &testHandler{
		responseBody: `{
			"references": [
				{"id":"gd-FNHTKRBDIQ","title":"real","type":"task","status":"open","source_field":"description"},
				{"id":"gd-ZACQZ1SGLD","title":"","type":"","status":"","source_field":"description"}
			],
			"total": 2,
			"limit": 50,
			"offset": 0
		}`,
	}
	c, srv := newTestClient(h)
	defer srv.Close()

	resp, err := c.ListBeadReferencesFrom(context.Background(), "gd-ldkpaoah1s", 50, 0, nil)
	if err != nil {
		t.Fatalf("ListBeadReferencesFrom: %v", err)
	}

	if h.path != "/v1/beads/gd-ldkpaoah1s/mentions" {
		t.Errorf("path = %q, want /v1/beads/gd-ldkpaoah1s/mentions", h.path)
	}
	if len(resp.References) != 2 {
		t.Fatalf("len(refs) = %d, want 2", len(resp.References))
	}
	if resp.References[1].ID != "gd-ZACQZ1SGLD" || resp.References[1].Title != "" {
		t.Errorf("dangling ref = %+v", resp.References[1])
	}
}
