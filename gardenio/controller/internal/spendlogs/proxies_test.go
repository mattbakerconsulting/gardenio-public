package spendlogs

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"gardenio/controller/internal/beadsapi"
)

type stubLister struct {
	beads []*beadsapi.BeadDetail
	err   error
}

func (s *stubLister) ListBeadsFiltered(_ context.Context, q beadsapi.ListBeadsQuery) (*beadsapi.ListBeadsResult, error) {
	if s.err != nil {
		return nil, s.err
	}
	if !reflect.DeepEqual(q.Types, []string{"account"}) {
		return nil, errors.New("expected an account-bead query")
	}
	return &beadsapi.ListBeadsResult{Beads: s.beads, Total: len(s.beads)}, nil
}

func accountBead(baseURL string) *beadsapi.BeadDetail {
	fields := map[string]string{}
	if baseURL != "" {
		fields["base_url"] = baseURL
	}
	return &beadsapi.BeadDetail{Type: "account", Fields: fields}
}

func TestProxyURLs(t *testing.T) {
	lister := &stubLister{beads: []*beadsapi.BeadDetail{
		accountBead("http://litellm-a:4000"),
		accountBead("http://litellm-a:4000/"), // slash variant dedups
		accountBead("http://litellm-b:4000"),
		accountBead(""), // no override → global only
	}}
	got := ProxyURLs(context.Background(), lister, "http://global:4000/")
	want := []string{"http://global:4000", "http://litellm-a:4000", "http://litellm-b:4000"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ProxyURLs = %v, want %v", got, want)
	}
}

func TestProxyURLsDaemonErrorFallsBackToGlobal(t *testing.T) {
	lister := &stubLister{err: errors.New("daemon down")}
	got := ProxyURLs(context.Background(), lister, "http://global:4000")
	if want := []string{"http://global:4000"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ProxyURLs = %v, want %v", got, want)
	}
}
