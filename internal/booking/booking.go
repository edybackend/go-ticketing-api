// Package booking sells seats: all-or-nothing holds that expire, idempotent purchases, and a sweeper.
package booking

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver

	"github.com/edybackend/go-ticketing-api/internal/stream"
)

const schema = `
CREATE TABLE IF NOT EXISTS events (
	id            TEXT PRIMARY KEY,
	name          TEXT NOT NULL,
	venue         TEXT NOT NULL,
	starts_at     TEXT NOT NULL,
	rows_count    INTEGER NOT NULL,
	seats_per_row INTEGER NOT NULL
);
-- One row per seat. A hold or a sale is a conditional UPDATE on these rows: that is the whole concurrency control.
CREATE TABLE IF NOT EXISTS seats (
	event_id   TEXT NOT NULL REFERENCES events (id),
	seat       TEXT NOT NULL,
	status     TEXT NOT NULL CHECK (status IN ('available', 'held', 'sold')),
	hold_id    TEXT NULL,
	held_until INTEGER NULL, -- unix ms; a held seat past this time counts as available
	PRIMARY KEY (event_id, seat)
);
CREATE INDEX IF NOT EXISTS ix_seats_hold ON seats (hold_id);
CREATE INDEX IF NOT EXISTS ix_seats_expiry ON seats (status, held_until);
CREATE TABLE IF NOT EXISTS holds (
	id         TEXT PRIMARY KEY,
	event_id   TEXT NOT NULL,
	customer   TEXT NOT NULL,
	seats      TEXT NOT NULL, -- comma separated
	expires_at INTEGER NOT NULL,
	status     TEXT NOT NULL CHECK (status IN ('active', 'purchased', 'released', 'expired'))
);
CREATE TABLE IF NOT EXISTS orders (
	id         TEXT PRIMARY KEY,
	hold_id    TEXT NOT NULL UNIQUE, -- one order per hold: purchasing twice returns the same order
	event_id   TEXT NOT NULL,
	customer   TEXT NOT NULL,
	seats      TEXT NOT NULL,
	created_at TEXT NOT NULL
);`

// MaxSeatsPerHold limits how many seats one hold may take.
const MaxSeatsPerHold = 6

var (
	// ErrInvalid is a malformed request.
	ErrInvalid = errors.New("invalid request")
	// ErrNotFound is an unknown event, seat, hold.
	ErrNotFound = errors.New("not found")
	// ErrUnavailable means at least one seat is held or sold by someone else.
	ErrUnavailable = errors.New("seats unavailable")
	// ErrExpired means the hold ran out (or its seats were taken after it ran out).
	ErrExpired = errors.New("hold expired")
	// ErrConflict is an action that does not fit the hold's state (e.g. releasing a purchased hold).
	ErrConflict = errors.New("conflict")
)

// UnavailableError lists the seats that blocked a hold.
type UnavailableError struct{ Seats []string }

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("seats unavailable: %s", strings.Join(e.Seats, ", "))
}

// Unwrap lets errors.Is(err, ErrUnavailable) work.
func (e *UnavailableError) Unwrap() error { return ErrUnavailable }

// Event is a show.
type Event struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Venue       string `json:"venue"`
	StartsAt    string `json:"startsAt"`
	Rows        int    `json:"rows"`
	SeatsPerRow int    `json:"seatsPerRow"`
}

// Seat is a seat's current status.
type Seat struct {
	Seat   string `json:"seat"`
	Status string `json:"status"`
}

// Hold reserves seats for a while.
type Hold struct {
	ID        string    `json:"id"`
	EventID   string    `json:"eventId"`
	Customer  string    `json:"customer"`
	Seats     []string  `json:"seats"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// Order is a purchased hold.
type Order struct {
	ID        string    `json:"id"`
	HoldID    string    `json:"holdId"`
	EventID   string    `json:"eventId"`
	Customer  string    `json:"customer"`
	Seats     []string  `json:"seats"`
	CreatedAt time.Time `json:"createdAt"`
}

// Service is the box office.
type Service struct {
	db      *sql.DB
	now     func() time.Time
	holdTTL time.Duration
	changes *stream.Broker
}

// Open opens the database at path, applies the schema and seeds the sample events on first start.
func Open(ctx context.Context, path string, now func() time.Time, holdTTL time.Duration, changes *stream.Broker) (*Service, error) {
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// One connection: SQLite has one writer, and transactions are then serialized in the pool instead of failing.
	db.SetMaxOpenConns(1)
	s := &Service{db: db, now: now, holdTTL: holdTTL, changes: changes}
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return nil, errors.Join(fmt.Errorf("apply schema: %w", err), db.Close())
	}
	if err := s.seed(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return s, nil
}

// Close releases the database.
func (s *Service) Close() error { return s.db.Close() }

var seatPattern = regexp.MustCompile(`^[A-Z][0-9]{1,2}$`)

// Hold reserves every seat in seats for customer, or none of them.
func (s *Service) Hold(ctx context.Context, eventID string, seats []string, customer string) (Hold, error) {
	customer = strings.TrimSpace(customer)
	if err := validateHold(seats, customer); err != nil {
		return Hold{}, err
	}
	now := s.now()
	h := Hold{ID: "hold_" + strings.ToLower(rand.Text()[:12]), EventID: eventID, Customer: customer, Seats: seats, ExpiresAt: now.Add(s.holdTTL).UTC()}

	err := s.inTx(ctx, func(tx *sql.Tx) error {
		if err := s.requireSeats(ctx, tx, eventID, seats); err != nil {
			return err
		}
		// The compare-and-set: take each seat only if it is free (or its hold ran out). Rows affected must be all of them.
		args := []any{h.ID, h.ExpiresAt.UnixMilli(), eventID, now.UnixMilli()}
		res, err := tx.ExecContext(ctx,
			`UPDATE seats SET status = 'held', hold_id = ?, held_until = ?
			 WHERE event_id = ? AND (status = 'available' OR (status = 'held' AND held_until < ?)) AND seat IN (`+placeholders(len(seats))+`)`,
			append(args, toAny(seats)...)...)
		if err != nil {
			return fmt.Errorf("take seats: %w", err)
		}
		if n, _ := res.RowsAffected(); int(n) != len(seats) {
			taken, err := s.notHeldBy(ctx, tx, eventID, seats, h.ID)
			if err != nil {
				return err
			}
			return &UnavailableError{Seats: taken} // the transaction rolls back: nothing is held
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO holds (id, event_id, customer, seats, expires_at, status) VALUES (?, ?, ?, ?, ?, 'active')`,
			h.ID, eventID, customer, strings.Join(seats, ","), h.ExpiresAt.UnixMilli())
		return err
	})
	if err != nil {
		return Hold{}, err
	}
	s.changes.Publish(stream.Change{EventID: eventID, Seats: seats, Status: "held"})
	return h, nil
}

// Purchase turns an active hold into an order. Repeating it returns the same order (created=false).
func (s *Service) Purchase(ctx context.Context, holdID string) (order Order, created bool, err error) {
	now := s.now()
	err = s.inTx(ctx, func(tx *sql.Tx) error {
		h, status, err := s.hold(ctx, tx, holdID)
		if err != nil {
			return err
		}
		switch {
		case status == "purchased":
			order, err = s.orderOf(ctx, tx, holdID)
			return err
		case status != "active":
			return fmt.Errorf("%w: the hold is %s", ErrConflict, status)
		case now.After(h.ExpiresAt):
			return fmt.Errorf("%w: it ended at %s; hold the seats again", ErrExpired, h.ExpiresAt.Format(time.RFC3339))
		}
		res, err := tx.ExecContext(ctx, `UPDATE seats SET status = 'sold', held_until = NULL WHERE hold_id = ? AND status = 'held'`, holdID)
		if err != nil {
			return fmt.Errorf("sell seats: %w", err)
		}
		if n, _ := res.RowsAffected(); int(n) != len(h.Seats) {
			return fmt.Errorf("%w: some seats were taken after the hold ran out", ErrExpired)
		}
		order = Order{ID: "ord_" + strings.ToLower(rand.Text()[:12]), HoldID: holdID, EventID: h.EventID, Customer: h.Customer, Seats: h.Seats, CreatedAt: now.UTC()}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO orders (id, hold_id, event_id, customer, seats, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			order.ID, holdID, h.EventID, h.Customer, strings.Join(h.Seats, ","), order.CreatedAt.Format(time.RFC3339Nano)); err != nil {
			return fmt.Errorf("insert order: %w", err)
		}
		created = true
		_, err = tx.ExecContext(ctx, `UPDATE holds SET status = 'purchased' WHERE id = ?`, holdID)
		return err
	})
	if err == nil && created {
		s.changes.Publish(stream.Change{EventID: order.EventID, Seats: order.Seats, Status: "sold"})
	}
	return order, created, err
}

// Release gives the seats of an active hold back.
func (s *Service) Release(ctx context.Context, holdID string) error {
	var h Hold
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		var status string
		var err error
		if h, status, err = s.hold(ctx, tx, holdID); err != nil {
			return err
		}
		if status != "active" {
			return fmt.Errorf("%w: the hold is %s", ErrConflict, status)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE seats SET status = 'available', hold_id = NULL, held_until = NULL WHERE hold_id = ? AND status = 'held'`, holdID); err != nil {
			return fmt.Errorf("release seats: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE holds SET status = 'released' WHERE id = ?`, holdID)
		return err
	})
	if err == nil {
		s.changes.Publish(stream.Change{EventID: h.EventID, Seats: h.Seats, Status: "available"})
	}
	return err
}

// SweepExpired releases the seats of holds that ran out and returns how many seats it freed.
func (s *Service) SweepExpired(ctx context.Context) (int, error) {
	now := s.now().UnixMilli()
	freed := map[string][]string{}
	err := s.inTx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT event_id, seat FROM seats WHERE status = 'held' AND held_until < ?`, now)
		if err != nil {
			return fmt.Errorf("find expired: %w", err)
		}
		for rows.Next() {
			var event, seat string
			if err := rows.Scan(&event, &seat); err != nil {
				_ = rows.Close()
				return fmt.Errorf("scan expired: %w", err)
			}
			freed[event] = append(freed[event], seat)
		}
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE seats SET status = 'available', hold_id = NULL, held_until = NULL WHERE status = 'held' AND held_until < ?`, now); err != nil {
			return fmt.Errorf("free expired: %w", err)
		}
		_, err = tx.ExecContext(ctx, `UPDATE holds SET status = 'expired' WHERE status = 'active' AND expires_at < ?`, now)
		return err
	})
	if err != nil {
		return 0, err
	}
	total := 0
	for event, seats := range freed {
		total += len(seats)
		s.changes.Publish(stream.Change{EventID: event, Seats: seats, Status: "available"})
	}
	return total, nil
}

// Events lists the shows.
func (s *Service) Events(ctx context.Context) ([]Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, name, venue, starts_at, rows_count, seats_per_row FROM events ORDER BY starts_at`)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Name, &e.Venue, &e.StartsAt, &e.Rows, &e.SeatsPerRow); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Seats returns the seat map of an event. A seat held past its time is shown as available.
func (s *Service) Seats(ctx context.Context, eventID string) ([]Seat, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT seat, CASE WHEN status = 'held' AND held_until < ? THEN 'available' ELSE status END
		 FROM seats WHERE event_id = ? ORDER BY substr(seat, 1, 1), CAST(substr(seat, 2) AS INTEGER)`,
		s.now().UnixMilli(), eventID)
	if err != nil {
		return nil, fmt.Errorf("seat map: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []Seat
	for rows.Next() {
		var seat Seat
		if err := rows.Scan(&seat.Seat, &seat.Status); err != nil {
			return nil, fmt.Errorf("scan seat: %w", err)
		}
		out = append(out, seat)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: event %s", ErrNotFound, eventID)
	}
	return out, nil
}

func validateHold(seats []string, customer string) error {
	switch {
	case customer == "" || len(customer) > 100:
		return fmt.Errorf("%w: customer is required (up to 100 characters)", ErrInvalid)
	case len(seats) == 0 || len(seats) > MaxSeatsPerHold:
		return fmt.Errorf("%w: hold 1 to %d seats", ErrInvalid, MaxSeatsPerHold)
	}
	seen := map[string]bool{}
	for _, seat := range seats {
		if !seatPattern.MatchString(seat) {
			return fmt.Errorf("%w: %q is not a seat (row letter + number, e.g. B7)", ErrInvalid, seat)
		}
		if seen[seat] {
			return fmt.Errorf("%w: seat %s appears twice", ErrInvalid, seat)
		}
		seen[seat] = true
	}
	return nil
}

func (s *Service) requireSeats(ctx context.Context, tx *sql.Tx, eventID string, seats []string) error {
	rows, err := tx.QueryContext(ctx, `SELECT seat FROM seats WHERE event_id = ? AND seat IN (`+placeholders(len(seats))+`)`, append([]any{eventID}, toAny(seats)...)...)
	if err != nil {
		return fmt.Errorf("check seats: %w", err)
	}
	found := map[string]bool{}
	for rows.Next() {
		var seat string
		if err := rows.Scan(&seat); err != nil {
			_ = rows.Close()
			return err
		}
		found[seat] = true
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	var missing []string
	for _, seat := range seats {
		if !found[seat] {
			missing = append(missing, seat)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w: event %s has no seat %s", ErrNotFound, eventID, strings.Join(missing, ", "))
	}
	return nil
}

func (s *Service) notHeldBy(ctx context.Context, tx *sql.Tx, eventID string, seats []string, holdID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT seat FROM seats WHERE event_id = ? AND (hold_id IS NULL OR hold_id <> ?) AND seat IN (`+placeholders(len(seats))+`) ORDER BY seat`,
		append([]any{eventID, holdID}, toAny(seats)...)...)
	if err != nil {
		return nil, fmt.Errorf("find taken seats: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var taken []string
	for rows.Next() {
		var seat string
		if err := rows.Scan(&seat); err != nil {
			return nil, err
		}
		taken = append(taken, seat)
	}
	return taken, rows.Err()
}

func (s *Service) hold(ctx context.Context, tx *sql.Tx, holdID string) (Hold, string, error) {
	var h Hold
	var seats, status string
	var expires int64
	err := tx.QueryRowContext(ctx, `SELECT id, event_id, customer, seats, expires_at, status FROM holds WHERE id = ?`, holdID).
		Scan(&h.ID, &h.EventID, &h.Customer, &seats, &expires, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return Hold{}, "", fmt.Errorf("%w: hold %s", ErrNotFound, holdID)
	}
	if err != nil {
		return Hold{}, "", fmt.Errorf("read hold: %w", err)
	}
	h.Seats = strings.Split(seats, ",")
	h.ExpiresAt = time.UnixMilli(expires).UTC()
	return h, status, nil
}

func (s *Service) orderOf(ctx context.Context, tx *sql.Tx, holdID string) (Order, error) {
	var o Order
	var seats, created string
	err := tx.QueryRowContext(ctx, `SELECT id, hold_id, event_id, customer, seats, created_at FROM orders WHERE hold_id = ?`, holdID).
		Scan(&o.ID, &o.HoldID, &o.EventID, &o.Customer, &seats, &created)
	if err != nil {
		return Order{}, fmt.Errorf("read order: %w", err)
	}
	o.Seats = strings.Split(seats, ",")
	o.CreatedAt, err = time.Parse(time.RFC3339Nano, created)
	return o, err
}

func (s *Service) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	if err := fn(tx); err != nil {
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func toAny(values []string) []any {
	out := make([]any, len(values))
	for i, v := range values {
		out[i] = v
	}
	return out
}
