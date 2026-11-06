package postgres

import (
	"context"
	"strconv"

	"gardenio/kbeads/internal/model"
	"github.com/lib/pq"
)

// Push device registry (migration 053, Lands 4.7b gd-a7U4AgwgCA).

func (s *PostgresStore) UpsertPushDevice(ctx context.Context, d *model.PushDevice) error {
	return queryUpsertPushDevice(ctx, s.db, d)
}

func (s *txStore) UpsertPushDevice(ctx context.Context, d *model.PushDevice) error {
	return queryUpsertPushDevice(ctx, s.tx, d)
}

func (s *PostgresStore) ListPushDevices(ctx context.Context, filter model.PushDeviceFilter) ([]*model.PushDevice, error) {
	return queryListPushDevices(ctx, s.db, filter)
}

func (s *txStore) ListPushDevices(ctx context.Context, filter model.PushDeviceFilter) ([]*model.PushDevice, error) {
	return queryListPushDevices(ctx, s.tx, filter)
}

func (s *PostgresStore) DeletePushDevice(ctx context.Context, token, userEmail string) (bool, error) {
	return queryDeletePushDevice(ctx, s.db, token, userEmail)
}

func (s *txStore) DeletePushDevice(ctx context.Context, token, userEmail string) (bool, error) {
	return queryDeletePushDevice(ctx, s.tx, token, userEmail)
}

// queryUpsertPushDevice keys on the token: a re-registration by another
// person moves the install to them (the latest sign-in owns it). created_at
// is kept from the first registration.
func queryUpsertPushDevice(ctx context.Context, db executor, d *model.PushDevice) error {
	return db.QueryRowContext(ctx, `
		INSERT INTO push_devices (token, platform, bundle_id, user_email)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (token)
		DO UPDATE SET platform     = EXCLUDED.platform,
		              bundle_id    = EXCLUDED.bundle_id,
		              user_email   = EXCLUDED.user_email,
		              last_seen_at = NOW()
		RETURNING created_at, last_seen_at`,
		d.Token, d.Platform, d.BundleID, d.UserEmail,
	).Scan(&d.CreatedAt, &d.LastSeenAt)
}

func queryListPushDevices(ctx context.Context, db executor, filter model.PushDeviceFilter) ([]*model.PushDevice, error) {
	q := `SELECT token, platform, bundle_id, user_email, created_at, last_seen_at
	      FROM push_devices`
	var args []any
	where := ""
	if len(filter.UserEmails) > 0 {
		args = append(args, pq.Array(filter.UserEmails))
		where = appendWhere(where, "user_email = ANY($"+strconv.Itoa(len(args))+"::text[])")
	}
	if filter.Platform != "" {
		args = append(args, filter.Platform)
		where = appendWhere(where, "platform = $"+strconv.Itoa(len(args)))
	}
	q += where + " ORDER BY user_email, token"

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*model.PushDevice{}
	for rows.Next() {
		d := &model.PushDevice{}
		if err := rows.Scan(&d.Token, &d.Platform, &d.BundleID, &d.UserEmail, &d.CreatedAt, &d.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func queryDeletePushDevice(ctx context.Context, db executor, token, userEmail string) (bool, error) {
	q := `DELETE FROM push_devices WHERE token = $1`
	args := []any{token}
	if userEmail != "" {
		q += ` AND user_email = $2`
		args = append(args, userEmail)
	}
	res, err := db.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}
