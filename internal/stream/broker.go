// Package stream fans out seat changes to live subscribers (the SSE streams of the seat map).
package stream

import (
	"sync"
)

// Change is one seat status change of an event.
type Change struct {
	EventID string   `json:"eventId"`
	Seats   []string `json:"seats"`
	Status  string   `json:"status"` // held, sold, available
}

// Subscription receives the changes of one event. C is closed when the subscription ends: either the
// subscriber cancelled, or it fell behind and was dropped so it cannot slow anyone else down.
type Subscription struct {
	C       <-chan Change
	c       chan Change
	eventID string
	broker  *Broker
	once    sync.Once
}

// Close ends the subscription. Safe to call more than once.
func (s *Subscription) Close() { s.broker.remove(s) }

// Broker is an in-process pub/sub, one topic per event.
type Broker struct {
	buffer int

	mu   sync.Mutex
	subs map[string]map[*Subscription]struct{}
}

// NewBroker returns a broker whose subscribers can lag behind by at most buffer changes.
func NewBroker(buffer int) *Broker {
	return &Broker{buffer: max(1, buffer), subs: map[string]map[*Subscription]struct{}{}}
}

// Subscribe starts receiving the changes of eventID.
func (b *Broker) Subscribe(eventID string) *Subscription {
	c := make(chan Change, b.buffer)
	s := &Subscription{C: c, c: c, eventID: eventID, broker: b}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.subs[eventID] == nil {
		b.subs[eventID] = map[*Subscription]struct{}{}
	}
	b.subs[eventID][s] = struct{}{}
	return s
}

// Publish delivers ch to every subscriber of its event without ever blocking:
// a subscriber whose buffer is full is dropped (its channel is closed and it must reload).
func (b *Broker) Publish(ch Change) {
	b.mu.Lock()
	var slow []*Subscription
	for s := range b.subs[ch.EventID] {
		select {
		case s.c <- ch:
		default:
			slow = append(slow, s)
		}
	}
	b.mu.Unlock()
	for _, s := range slow {
		b.remove(s)
	}
}

// Subscribers counts the live subscriptions of eventID.
func (b *Broker) Subscribers(eventID string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.subs[eventID])
}

func (b *Broker) remove(s *Subscription) {
	s.once.Do(func() {
		b.mu.Lock()
		delete(b.subs[s.eventID], s)
		if len(b.subs[s.eventID]) == 0 {
			delete(b.subs, s.eventID)
		}
		b.mu.Unlock()
		close(s.c)
	})
}
