package booking

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/edybackend/go-ticketing-api/internal/stream"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

func newService(t *testing.T) (*Service, *clock, *stream.Broker) {
	t.Helper()
	c := &clock{now: time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)}
	b := stream.NewBroker(64)
	s, err := Open(t.Context(), filepath.Join(t.TempDir(), "tickets.db"), c.Now, 5*time.Minute, b)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, c, b
}

func statusOf(t *testing.T, s *Service, seat string) string {
	t.Helper()
	seats, err := s.Seats(t.Context(), "jazz-night")
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range seats {
		if x.Seat == seat {
			return x.Status
		}
	}
	t.Fatalf("seat %s not found", seat)
	return ""
}

func TestTwoHundredBuyersOneSeatOneWinner(t *testing.T) {
	s, _, _ := newService(t)
	var wins, conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := range 200 {
		wg.Go(func() {
			_, err := s.Hold(t.Context(), "jazz-night", []string{"D7"}, "buyer-"+string(rune('a'+i%26)))
			switch {
			case err == nil:
				wins.Add(1)
			case errors.Is(err, ErrUnavailable):
				conflicts.Add(1)
			default:
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
	wg.Wait()

	if wins.Load() != 1 || conflicts.Load() != 199 {
		t.Fatalf("wins = %d, conflicts = %d; want 1 and 199", wins.Load(), conflicts.Load())
	}
}

func TestOverlappingHoldsAreAllOrNothing(t *testing.T) {
	s, _, _ := newService(t)
	if _, err := s.Hold(t.Context(), "jazz-night", []string{"A1", "A2"}, "ana"); err != nil {
		t.Fatal(err)
	}

	_, err := s.Hold(t.Context(), "jazz-night", []string{"A2", "A3"}, "bruno")

	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || len(unavailable.Seats) != 1 || unavailable.Seats[0] != "A2" {
		t.Fatalf("err = %v; want A2 unavailable", err)
	}
	if got := statusOf(t, s, "A3"); got != "available" {
		t.Fatalf("A3 = %s; the losing hold must not keep any seat", got)
	}
}

func TestAnExpiredHoldFreesItsSeatsEvenBeforeTheSweeper(t *testing.T) {
	s, c, _ := newService(t)
	old, _ := s.Hold(t.Context(), "jazz-night", []string{"B5"}, "ana")
	c.Advance(5*time.Minute + time.Second)

	if got := statusOf(t, s, "B5"); got != "available" {
		t.Fatalf("B5 = %s after the hold expired; want available", got)
	}
	if _, err := s.Hold(t.Context(), "jazz-night", []string{"B5"}, "bruno"); err != nil {
		t.Fatalf("another buyer should get B5: %v", err)
	}
	if _, _, err := s.Purchase(t.Context(), old.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("purchasing the expired hold: %v; want ErrExpired", err)
	}
}

func TestPurchasingTwiceReturnsTheSameOrder(t *testing.T) {
	s, _, _ := newService(t)
	h, _ := s.Hold(t.Context(), "jazz-night", []string{"C1", "C2"}, "ana")

	first, created1, err1 := s.Purchase(t.Context(), h.ID)
	second, created2, err2 := s.Purchase(t.Context(), h.ID)

	if err1 != nil || err2 != nil || !created1 || created2 || first.ID != second.ID {
		t.Fatalf("first %v %v %v, second %v %v %v", first.ID, created1, err1, second.ID, created2, err2)
	}
	if got := statusOf(t, s, "C2"); got != "sold" {
		t.Fatalf("C2 = %s; want sold", got)
	}
}

func TestAPurchasedHoldCannotBeReleased(t *testing.T) {
	s, _, _ := newService(t)
	h, _ := s.Hold(t.Context(), "jazz-night", []string{"E1"}, "ana")
	if _, _, err := s.Purchase(t.Context(), h.ID); err != nil {
		t.Fatal(err)
	}

	if err := s.Release(t.Context(), h.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("release after purchase: %v; want ErrConflict", err)
	}
}

func TestReleaseAndSweepGiveSeatsBackAndAnnounceIt(t *testing.T) {
	s, c, b := newService(t)
	sub := b.Subscribe("jazz-night")
	defer sub.Close()

	released, _ := s.Hold(t.Context(), "jazz-night", []string{"F1"}, "ana")
	_, _ = s.Hold(t.Context(), "jazz-night", []string{"F2", "F3"}, "bruno")
	if err := s.Release(t.Context(), released.ID); err != nil {
		t.Fatal(err)
	}
	c.Advance(6 * time.Minute)
	freed, err := s.SweepExpired(t.Context())

	if err != nil || freed != 2 {
		t.Fatalf("swept %d seats (%v); want 2", freed, err)
	}
	var statuses []string
	for range 4 {
		statuses = append(statuses, (<-sub.C).Status)
	}
	if want := []string{"held", "held", "available", "available"}; !equal(statuses, want) {
		t.Fatalf("changes = %v; want %v", statuses, want)
	}
}

func TestInvalidRequests(t *testing.T) {
	s, _, _ := newService(t)
	tests := []struct {
		name     string
		event    string
		seats    []string
		customer string
		want     error
	}{
		{"no seats", "jazz-night", nil, "ana", ErrInvalid},
		{"too many seats", "jazz-night", []string{"A1", "A2", "A3", "A4", "A5", "A6", "A7"}, "ana", ErrInvalid},
		{"duplicate seat", "jazz-night", []string{"A1", "A1"}, "ana", ErrInvalid},
		{"malformed seat", "jazz-night", []string{"1A"}, "ana", ErrInvalid},
		{"no customer", "jazz-night", []string{"A1"}, " ", ErrInvalid},
		{"seat outside the hall", "jazz-night", []string{"Z1"}, "ana", ErrNotFound},
		{"unknown event", "opera", []string{"A1"}, "ana", ErrNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := s.Hold(t.Context(), tt.event, tt.seats, tt.customer); !errors.Is(err, tt.want) {
				t.Fatalf("err = %v; want %v", err, tt.want)
			}
		})
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
