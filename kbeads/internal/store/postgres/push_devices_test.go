package postgres

import (
	"context"
	"testing"
	"time"

	"gardenio/kbeads/internal/model"
	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"
)

// TestUpsertPushDevice pins the upsert: keyed on token, a re-registration
// moves the install to the new owner and keeps created_at.
func TestUpsertPushDevice(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	created := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	seen := time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`INSERT INTO push_devices \(token, platform, bundle_id, user_email\)\s+VALUES \(\$1, \$2, \$3, \$4\)\s+ON CONFLICT \(token\)\s+DO UPDATE SET platform\s+= EXCLUDED\.platform,\s+bundle_id\s+= EXCLUDED\.bundle_id,\s+user_email\s+= EXCLUDED\.user_email,\s+last_seen_at = NOW\(\)\s+RETURNING created_at, last_seen_at`).
		WithArgs("abc123", "ios", "com.gardenio.app", "owner@example.com").
		WillReturnRows(sqlmock.NewRows([]string{"created_at", "last_seen_at"}).AddRow(created, seen))

	d := &model.PushDevice{Token: "abc123", Platform: "ios", BundleID: "com.gardenio.app", UserEmail: "owner@example.com"}
	if err := store.UpsertPushDevice(context.Background(), d); err != nil {
		t.Fatalf("UpsertPushDevice: %v", err)
	}
	if !d.CreatedAt.Equal(created) || !d.LastSeenAt.Equal(seen) {
		t.Fatalf("timestamps not filled: %+v", d)
	}
}

// TestListPushDevices_Unfiltered: no filter means every row, no WHERE.
func TestListPushDevices_Unfiltered(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	now := time.Now().UTC()
	mock.ExpectQuery(`SELECT token, platform, bundle_id, user_email, created_at, last_seen_at\s+FROM push_devices ORDER BY user_email, token$`).
		WillReturnRows(sqlmock.NewRows([]string{"token", "platform", "bundle_id", "user_email", "created_at", "last_seen_at"}).
			AddRow("t1", "ios", "", "a@example.com", now, now).
			AddRow("t2", "ios", "b", "b@example.com", now, now))

	got, err := store.ListPushDevices(context.Background(), model.PushDeviceFilter{})
	if err != nil {
		t.Fatalf("ListPushDevices: %v", err)
	}
	if len(got) != 2 || got[0].Token != "t1" || got[1].UserEmail != "b@example.com" {
		t.Fatalf("rows = %+v", got)
	}
}

// TestListPushDevices_ByOwnersAndPlatform pins the owner ANY() match and the
// platform equality, in that argument order.
func TestListPushDevices_ByOwnersAndPlatform(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectQuery(`FROM push_devices WHERE user_email = ANY\(\$1::text\[\]\) AND platform = \$2 ORDER BY user_email, token`).
		WithArgs(pq.Array([]string{"a@example.com", "b@example.com"}), "ios").
		WillReturnRows(sqlmock.NewRows([]string{"token", "platform", "bundle_id", "user_email", "created_at", "last_seen_at"}))

	got, err := store.ListPushDevices(context.Background(), model.PushDeviceFilter{
		UserEmails: []string{"a@example.com", "b@example.com"},
		Platform:   "ios",
	})
	if err != nil {
		t.Fatalf("ListPushDevices: %v", err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("want empty non-nil slice, got %#v", got)
	}
}

// TestDeletePushDevice_OwnerScoped: a non-empty owner narrows the DELETE, so
// a person can only remove their own install.
func TestDeletePushDevice_OwnerScoped(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`DELETE FROM push_devices WHERE token = \$1 AND user_email = \$2`).
		WithArgs("t1", "a@example.com").
		WillReturnResult(sqlmock.NewResult(0, 0))

	removed, err := store.DeletePushDevice(context.Background(), "t1", "a@example.com")
	if err != nil {
		t.Fatalf("DeletePushDevice: %v", err)
	}
	if removed {
		t.Fatal("a miss must report false")
	}
}

// TestDeletePushDevice_AnyOwner: the push bridge prunes dead tokens with no
// owner filter.
func TestDeletePushDevice_AnyOwner(t *testing.T) {
	db, mock := newMockDB(t)
	store := &PostgresStore{db: db}

	mock.ExpectExec(`DELETE FROM push_devices WHERE token = \$1$`).
		WithArgs("t1").
		WillReturnResult(sqlmock.NewResult(0, 1))

	removed, err := store.DeletePushDevice(context.Background(), "t1", "")
	if err != nil {
		t.Fatalf("DeletePushDevice: %v", err)
	}
	if !removed {
		t.Fatal("a hit must report true")
	}
}
