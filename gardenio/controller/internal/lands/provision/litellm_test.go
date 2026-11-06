package provision

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeLiteLLM is the slice of LiteLLM's key admin API the provisioner calls.
type fakeLiteLLM struct {
	mu    sync.Mutex
	keys  map[string]map[string]any
	calls []string
	// infoStatus, when set, answers /key/info with it instead.
	infoStatus int
}

func (f *fakeLiteLLM) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, r.Method+" "+r.URL.Path)
	if r.Header.Get("Authorization") != "Bearer sk-master" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/key/info":
		if f.infoStatus != 0 {
			w.WriteHeader(f.infoStatus)
			_, _ = w.Write([]byte(`{"error":"boom for ` + r.URL.Query().Get("key") + `"}`))
			return
		}
		if _, ok := f.keys[r.URL.Query().Get("key")]; !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"message":"Key not found"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"info":{}}`))
	case "/key/generate":
		f.keys[body["key"].(string)] = body
		_, _ = w.Write([]byte(`{"key":"` + body["key"].(string) + `"}`))
	case "/key/update":
		k, ok := f.keys[body["key"].(string)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		k["max_budget"] = body["max_budget"]
		_, _ = w.Write([]byte(`{}`))
	case "/key/block", "/key/unblock":
		k, ok := f.keys[body["key"].(string)]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		k["blocked"] = r.URL.Path == "/key/block"
		_, _ = w.Write([]byte(`{}`))
	case "/key/delete":
		key := body["keys"].([]any)[0].(string)
		if _, ok := f.keys[key]; !ok {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Key ` + key + ` does not exist"}}`))
			return
		}
		delete(f.keys, key)
		_, _ = w.Write([]byte(`{"deleted_keys":["` + key + `"]}`))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func newFakeLiteLLM(t *testing.T) (*fakeLiteLLM, *LiteLLM) {
	f := &fakeLiteLLM{keys: map[string]map[string]any{}}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, &LiteLLM{BaseURL: srv.URL + "/", MasterKey: "sk-master", HTTP: srv.Client()}
}

func TestLiteLLM_EnsureCreatesOnceWithTheCallersKey(t *testing.T) {
	f, l := newFakeLiteLLM(t)
	ctx := context.Background()
	for range 2 {
		if err := l.Ensure(ctx, "sk-lands-abc", "lands-acme", 0, map[string]string{"land": "acme"}); err != nil {
			t.Fatal(err)
		}
	}
	k := f.keys["sk-lands-abc"]
	if k == nil || k["key_alias"] != "lands-acme" || k["max_budget"] != 0.0 {
		t.Fatalf("key = %v", k)
	}
	if got := strings.Join(f.calls, ","); got != "GET /key/info,POST /key/generate,GET /key/info" {
		t.Fatalf("calls = %s", got)
	}
	if err := l.SetBudget(ctx, "sk-lands-abc", 33.33); err != nil || k["max_budget"] != 33.33 {
		t.Fatalf("SetBudget: %v, budget %v", err, k["max_budget"])
	}
	for range 2 {
		if err := l.Delete(ctx, "sk-lands-abc"); err != nil {
			t.Fatalf("Delete: %v", err)
		}
	}
	if len(f.keys) != 0 {
		t.Fatal("key not deleted")
	}
}

func TestLiteLLM_ErrorsNeverCarryTheKey(t *testing.T) {
	f, l := newFakeLiteLLM(t)
	f.infoStatus = http.StatusInternalServerError
	err := l.Ensure(context.Background(), "sk-lands-secret", "lands-acme", 0, nil)
	if err == nil || strings.Contains(err.Error(), "sk-lands-secret") {
		t.Fatalf("err = %v: want an error without the key", err)
	}
	if strings.Contains(strings.Join(f.calls, ","), "generate") {
		t.Fatal("a failed info read must not create a key")
	}
	if err := (&LiteLLM{}).SetBudget(context.Background(), "k", 1); err == nil {
		t.Fatal("an unconfigured client must refuse")
	}
	if err := l.SetBudget(context.Background(), "sk-unknown", 1); err == nil {
		t.Fatal("SetBudget on an unknown key must fail")
	}
}

// Block and Unblock flip the key's blocked flag and keep its budget; a key
// LiteLLM does not know is no error either way (gd-nHh7M0k3ps).
func TestLiteLLM_BlockAndUnblock(t *testing.T) {
	f, l := newFakeLiteLLM(t)
	ctx := context.Background()
	if err := l.Ensure(ctx, "sk-land", "lands-x", 10, nil); err != nil {
		t.Fatal(err)
	}
	if err := l.Block(ctx, "sk-land"); err != nil {
		t.Fatal(err)
	}
	if f.keys["sk-land"]["blocked"] != true || f.keys["sk-land"]["max_budget"] != float64(10) {
		t.Fatalf("after Block: %v", f.keys["sk-land"])
	}
	if err := l.Unblock(ctx, "sk-land"); err != nil {
		t.Fatal(err)
	}
	if f.keys["sk-land"]["blocked"] != false {
		t.Fatalf("after Unblock: %v", f.keys["sk-land"])
	}
	if err := l.Block(ctx, "sk-gone"); err != nil {
		t.Errorf("Block of an unknown key: %v", err)
	}
	if err := l.Unblock(ctx, "sk-gone"); err != nil {
		t.Errorf("Unblock of an unknown key: %v", err)
	}
}
