package server

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	beadsv1 "gardenio/kbeads/gen/beads/v1"
	"gardenio/kbeads/internal/events"
	"gardenio/kbeads/internal/model"
)

// getConfigCountingStore counts GetConfig calls that reach the store.
type getConfigCountingStore struct {
	*mockStore
	getConfigCalls int
}

func (c *getConfigCountingStore) GetConfig(ctx context.Context, key string) (*model.Config, error) {
	c.getConfigCalls++
	return c.mockStore.GetConfig(ctx, key)
}

func newTypeConfigCacheServer(t *testing.T) (*BeadsServer, *getConfigCountingStore) {
	t.Helper()
	cs := &getConfigCountingStore{mockStore: newMockStore()}
	return NewBeadsServer(cs, &events.NoopPublisher{}), cs
}

// TestResolveTypeConfig_CachesStoreRow: repeated resolves of a type with no
// store row (the 97% case, gd-i3QuMqdgDB) and of one with a row each reach the
// store once.
func TestResolveTypeConfig_CachesStoreRow(t *testing.T) {
	srv, cs := newTypeConfigCacheServer(t)
	cs.configs["type:widget"] = &model.Config{Key: "type:widget", Value: json.RawMessage(`{"kind":"data"}`)}
	ctx := context.Background()

	for i := 0; i < 50; i++ {
		if tc, err := srv.resolveTypeConfig(ctx, "task"); err != nil || tc == nil {
			t.Fatalf("resolve task: tc=%v err=%v", tc, err)
		}
		if tc, err := srv.resolveTypeConfig(ctx, "widget"); err != nil || tc == nil || tc.Kind != model.KindData {
			t.Fatalf("resolve widget: tc=%v err=%v", tc, err)
		}
		if tc, err := srv.resolveTypeConfig(ctx, "no_such_type"); err != nil || tc != nil {
			t.Fatalf("resolve no_such_type: tc=%v err=%v", tc, err)
		}
	}
	if cs.getConfigCalls != 3 {
		t.Fatalf("GetConfig reached the store %d times, want 3 (one per type)", cs.getConfigCalls)
	}
}

// TestResolveTypeConfig_SetAndDeleteInvalidate: a config set or delete
// through the daemon takes effect on the next resolve, cache or no cache.
func TestResolveTypeConfig_SetAndDeleteInvalidate(t *testing.T) {
	srv, _ := newTypeConfigCacheServer(t)
	ctx := WithAuthIdentity(context.Background(), AuthIdentity{Type: "oidc", Subject: defaultTestSubject})

	if tc, err := srv.resolveTypeConfig(ctx, "gadget"); err != nil || tc != nil {
		t.Fatalf("before set: tc=%v err=%v, want nil (negative entry cached)", tc, err)
	}
	if _, err := srv.SetConfig(ctx, &beadsv1.SetConfigRequest{Key: "type:gadget", Value: []byte(`{"kind":"data"}`)}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	tc, err := srv.resolveTypeConfig(ctx, "gadget")
	if err != nil || tc == nil || tc.Kind != model.KindData {
		t.Fatalf("after set: tc=%v err=%v, want kind data", tc, err)
	}
	if _, err := srv.DeleteConfig(ctx, &beadsv1.DeleteConfigRequest{Key: "type:gadget"}); err != nil {
		t.Fatalf("DeleteConfig: %v", err)
	}
	if tc, err := srv.resolveTypeConfig(ctx, "gadget"); err != nil || tc != nil {
		t.Fatalf("after delete: tc=%v err=%v, want nil", tc, err)
	}
}

// TestTypeConfigCache_TTLAndRacingFill: an entry expires after the TTL, and a
// fill begun before an invalidation is dropped rather than reinstalling the
// old row.
func TestTypeConfigCache_TTLAndRacingFill(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	c := &typeConfigCache{now: func() time.Time { return now }}
	row := &model.Config{Key: "type:x"}

	_, ok, gen := c.lookup("type:x")
	if ok {
		t.Fatal("empty cache reported a hit")
	}
	c.fill("type:x", row, gen)
	if got, ok, _ := c.lookup("type:x"); !ok || got != row {
		t.Fatalf("after fill: got=%v ok=%v", got, ok)
	}
	now = now.Add(typeConfigCacheTTL)
	if _, ok, _ := c.lookup("type:x"); ok {
		t.Fatal("entry still live at the TTL")
	}

	_, _, gen = c.lookup("type:x")
	c.invalidate("type:x")
	c.fill("type:x", row, gen)
	if _, ok, _ := c.lookup("type:x"); ok {
		t.Fatal("a fill that raced an invalidation was installed")
	}
}
