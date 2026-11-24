package booking

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// seed creates two sample shows on the first start.
func (s *Service) seed(ctx context.Context) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&n); err != nil {
		return fmt.Errorf("count events: %w", err)
	}
	if n > 0 {
		return nil
	}
	start := s.now().UTC().Add(30 * 24 * time.Hour).Truncate(24 * time.Hour)
	events := []Event{
		{ID: "jazz-night", Name: "Jazz Night", Venue: "Blue Room", StartsAt: start.Add(20 * time.Hour).Format(time.RFC3339), Rows: 8, SeatsPerRow: 12},
		{ID: "indie-rock", Name: "Indie Rock Live", Venue: "Warehouse 9", StartsAt: start.Add(44 * time.Hour).Format(time.RFC3339), Rows: 6, SeatsPerRow: 10},
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, e := range events {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO events (id, name, venue, starts_at, rows_count, seats_per_row) VALUES (?, ?, ?, ?, ?, ?)`,
				e.ID, e.Name, e.Venue, e.StartsAt, e.Rows, e.SeatsPerRow); err != nil {
				return fmt.Errorf("insert event %s: %w", e.ID, err)
			}
			for row := range e.Rows {
				for number := 1; number <= e.SeatsPerRow; number++ {
					seat := fmt.Sprintf("%c%d", 'A'+row, number)
					if _, err := tx.ExecContext(ctx, `INSERT INTO seats (event_id, seat, status) VALUES (?, ?, 'available')`, e.ID, seat); err != nil {
						return fmt.Errorf("insert seat %s: %w", seat, err)
					}
				}
			}
		}
		return nil
	})
}
