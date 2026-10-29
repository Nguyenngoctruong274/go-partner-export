package postgres

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Nguyenngoctruong274/go-partner-export/internal/export"
)

// TripStore reads and writes the rows to export.
type TripStore struct {
	db *sql.DB
}

// NewTripStore returns a store over db.
func NewTripStore(db *sql.DB) *TripStore {
	return &TripStore{db: db}
}

// Upsert inserts trips or, when an engagement id is already present,
// replaces that row. Replayed or corrected events therefore converge on
// the latest values instead of producing duplicates in the export.
func (s *TripStore) Upsert(ctx context.Context, trips []export.Trip) error {
	if len(trips) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("upsert trips: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO trips (
			engagement_id, partner_user_id, booking_time, trip_end_time, service_type,
			pickup_location, dropoff_location, total_fare, discount, toll_fee,
			insurance_fee, customer_pay, currency, payment_method, status
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
		ON CONFLICT (engagement_id) DO UPDATE SET
			partner_user_id = EXCLUDED.partner_user_id,
			booking_time    = EXCLUDED.booking_time,
			trip_end_time   = EXCLUDED.trip_end_time,
			service_type    = EXCLUDED.service_type,
			pickup_location = EXCLUDED.pickup_location,
			dropoff_location = EXCLUDED.dropoff_location,
			total_fare      = EXCLUDED.total_fare,
			discount        = EXCLUDED.discount,
			toll_fee        = EXCLUDED.toll_fee,
			insurance_fee   = EXCLUDED.insurance_fee,
			customer_pay    = EXCLUDED.customer_pay,
			currency        = EXCLUDED.currency,
			payment_method  = EXCLUDED.payment_method,
			status          = EXCLUDED.status,
			updated_at      = now()`)
	if err != nil {
		return fmt.Errorf("upsert trips: prepare: %w", err)
	}
	defer stmt.Close()

	for _, t := range trips {
		if _, err := stmt.ExecContext(ctx,
			t.EngagementID, t.PartnerUserID, t.BookingTime, t.TripEndTime, t.ServiceType,
			t.PickupLocation, t.DropoffLocation, t.TotalFare, t.Discount, t.TollFee,
			t.InsuranceFee, t.CustomerPay, t.Currency, t.PaymentMethod, t.Status,
		); err != nil {
			return fmt.Errorf("upsert trip %d: %w", t.EngagementID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("upsert trips: commit: %w", err)
	}
	return nil
}

// ListEnded returns the trips that ended in [from, to), oldest first with
// the engagement id as a tie-breaker, so two runs of the same day produce
// byte-identical files.
func (s *TripStore) ListEnded(ctx context.Context, from, to time.Time) ([]export.Trip, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT engagement_id, partner_user_id, booking_time, trip_end_time, service_type,
		       pickup_location, dropoff_location, total_fare, discount, toll_fee,
		       insurance_fee, customer_pay, currency, payment_method, status
		FROM trips
		WHERE trip_end_time >= $1 AND trip_end_time < $2
		ORDER BY trip_end_time, engagement_id`, from, to)
	if err != nil {
		return nil, fmt.Errorf("list trips: %w", err)
	}
	defer rows.Close()

	var out []export.Trip
	for rows.Next() {
		var t export.Trip
		if err := rows.Scan(
			&t.EngagementID, &t.PartnerUserID, &t.BookingTime, &t.TripEndTime, &t.ServiceType,
			&t.PickupLocation, &t.DropoffLocation, &t.TotalFare, &t.Discount, &t.TollFee,
			&t.InsuranceFee, &t.CustomerPay, &t.Currency, &t.PaymentMethod, &t.Status,
		); err != nil {
			return nil, fmt.Errorf("list trips: scan: %w", err)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list trips: %w", err)
	}
	return out, nil
}

// CountEnded returns how many trips ended in [from, to).
func (s *TripStore) CountEnded(ctx context.Context, from, to time.Time) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM trips WHERE trip_end_time >= $1 AND trip_end_time < $2`, from, to).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count trips: %w", err)
	}
	return n, nil
}
