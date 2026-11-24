package httpapi_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/edybackend/go-ticketing-api/internal/booking"
	"github.com/edybackend/go-ticketing-api/internal/httpapi"
	"github.com/edybackend/go-ticketing-api/internal/stream"
)

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.now }
func (c *clock) Advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.now = c.now.Add(d) }

type app struct {
	url     string
	clock   *clock
	changes *stream.Broker
}

func newApp(t *testing.T) *app {
	t.Helper()
	c := &clock{now: time.Date(2026, 11, 24, 18, 0, 0, 0, time.UTC)}
	changes := stream.NewBroker(16)
	box, err := booking.Open(t.Context(), filepath.Join(t.TempDir(), "t.db"), c.Now, 5*time.Minute, changes)
	if err != nil {
		t.Fatal(err)
	}
	page := fstest.MapFS{"index.html": {Data: []byte("<h1>seat map</h1>")}}
	server := httptest.NewServer(httpapi.New(box, changes, page, slog.New(slog.DiscardHandler)))
	t.Cleanup(func() { server.Close(); _ = box.Close() })
	return &app{url: server.URL, clock: c, changes: changes}
}

func (a *app) do(t *testing.T, method, path, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequestWithContext(t.Context(), method, a.url+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var out map[string]any
	data, _ := io.ReadAll(res.Body)
	_ = json.Unmarshal(data, &out)
	return res.StatusCode, out
}

func TestStatusCodes(t *testing.T) {
	a := newApp(t)
	hold := func(body string) (int, map[string]any) { return a.do(t, "POST", "/v1/events/jazz-night/holds", body) }

	if status, h := hold(`{"seats":["A1","A2"],"customer":"ana"}`); status != http.StatusCreated || h["id"] == nil {
		t.Fatalf("hold: %d %v", status, h)
	}
	status, conflict := hold(`{"seats":["A2","A3"],"customer":"bruno"}`)
	if status != http.StatusConflict || conflict["seats"].([]any)[0] != "A2" {
		t.Fatalf("overlap: %d %v; want 409 listing A2", status, conflict)
	}
	tests := []struct {
		name, method, path, body string
		want                     int
	}{
		{"malformed JSON", "POST", "/v1/events/jazz-night/holds", `{"seats":`, http.StatusBadRequest},
		{"unknown field", "POST", "/v1/events/jazz-night/holds", `{"seats":["B1"],"customer":"x","price":1}`, http.StatusBadRequest},
		{"seven seats", "POST", "/v1/events/jazz-night/holds", `{"seats":["B1","B2","B3","B4","B5","B6","B7"],"customer":"x"}`, http.StatusBadRequest},
		{"unknown event", "GET", "/v1/events/opera/seats", "", http.StatusNotFound},
		{"unknown hold", "POST", "/v1/holds/hold_nope/purchase", "", http.StatusNotFound},
		{"unknown route", "GET", "/v1/nothing", "", http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, body := a.do(t, tt.method, tt.path, tt.body); got != tt.want {
				t.Fatalf("status %d (%v); want %d", got, body, tt.want)
			}
		})
	}
}

func TestPurchaseIsIdempotentAndAnExpiredHoldIsGone(t *testing.T) {
	a := newApp(t)
	_, h := a.do(t, "POST", "/v1/events/jazz-night/holds", `{"seats":["C1"],"customer":"ana"}`)
	_, late := a.do(t, "POST", "/v1/events/jazz-night/holds", `{"seats":["C2"],"customer":"bruno"}`)

	first, order := a.do(t, "POST", "/v1/holds/"+h["id"].(string)+"/purchase", "")
	again, same := a.do(t, "POST", "/v1/holds/"+h["id"].(string)+"/purchase", "")
	a.clock.Advance(6 * time.Minute)
	expired, _ := a.do(t, "POST", "/v1/holds/"+late["id"].(string)+"/purchase", "")

	if first != http.StatusCreated || again != http.StatusOK || order["id"] != same["id"] {
		t.Fatalf("purchase: %d then %d, orders %v / %v", first, again, order["id"], same["id"])
	}
	if expired != http.StatusGone {
		t.Fatalf("purchase after expiry: %d; want 410", expired)
	}
}

func TestAHoldByOneClientReachesAnotherClientsStream(t *testing.T) {
	a := newApp(t)
	ctx, cancel := context.WithCancel(t.Context())
	req, _ := http.NewRequestWithContext(ctx, "GET", a.url+"/v1/events/jazz-night/stream", nil)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if ct := res.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content type %q", ct)
	}
	lines := bufio.NewScanner(res.Body)
	waitFor := func(prefix string) string {
		t.Helper()
		for lines.Scan() {
			if strings.HasPrefix(lines.Text(), prefix) {
				return lines.Text()
			}
		}
		t.Fatalf("stream ended before %q", prefix)
		return ""
	}
	waitFor(": connected")

	a.do(t, "POST", "/v1/events/jazz-night/holds", `{"seats":["E4","E5"],"customer":"other-client"}`)

	if got := waitFor("data: "); got != `data: {"eventId":"jazz-night","seats":["E4","E5"],"status":"held"}` {
		t.Fatalf("got %s", got)
	}

	cancel() // the client leaves: its subscription must go away
	_ = res.Body.Close()
	deadline := time.Now().Add(2 * time.Second)
	for a.changes.Subscribers("jazz-night") > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := a.changes.Subscribers("jazz-night"); n != 0 {
		t.Fatalf("%d subscribers left after the client disconnected", n)
	}
}

func TestThePageIsServed(t *testing.T) {
	a := newApp(t)
	res, err := http.Get(a.url + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	if body, _ := io.ReadAll(res.Body); !strings.Contains(string(body), "seat map") {
		t.Fatalf("page: %s", body)
	}
}
