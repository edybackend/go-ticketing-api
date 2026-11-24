package stream

import (
	"testing"
	"time"
)

func TestSubscribersOfAnEventReceiveItsChangesOnly(t *testing.T) {
	b := NewBroker(4)
	a1 := b.Subscribe("concert")
	a2 := b.Subscribe("concert")
	other := b.Subscribe("play")
	defer a1.Close()
	defer a2.Close()
	defer other.Close()

	b.Publish(Change{EventID: "concert", Seats: []string{"A1"}, Status: "held"})

	for _, s := range []*Subscription{a1, a2} {
		if got := <-s.C; got.Seats[0] != "A1" || got.Status != "held" {
			t.Fatalf("got %+v", got)
		}
	}
	select {
	case got := <-other.C:
		t.Fatalf("the other event's subscriber received %+v", got)
	default:
	}
}

func TestASlowSubscriberIsDroppedAndNeverBlocksThePublisher(t *testing.T) {
	b := NewBroker(2)
	slow := b.Subscribe("concert") // never reads
	fast := b.Subscribe("concert")

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.Publish(Change{EventID: "concert", Status: "held"})
			<-fast.C
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	// The slow one got its 2 buffered changes, then its channel was closed.
	n := 0
	for range slow.C {
		n++
	}
	if n != 2 || b.Subscribers("concert") != 1 {
		t.Fatalf("slow subscriber received %d changes, %d subscribers left; want 2 and 1", n, b.Subscribers("concert"))
	}
	fast.Close()
}

func TestCloseRemovesTheSubscriptionAndIsIdempotent(t *testing.T) {
	b := NewBroker(1)
	s := b.Subscribe("concert")

	s.Close()
	s.Close()

	if _, open := <-s.C; open || b.Subscribers("concert") != 0 {
		t.Fatal("the subscription should be closed and removed")
	}
}
