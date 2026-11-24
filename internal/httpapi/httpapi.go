// Package httpapi exposes the box office over HTTP: JSON endpoints, a Server-Sent Events stream and the seat map page.
package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/edybackend/go-ticketing-api/internal/booking"
	"github.com/edybackend/go-ticketing-api/internal/stream"
)

// Heartbeat is how often an idle stream gets a comment line, so proxies do not close it.
var Heartbeat = 15 * time.Second

// New returns the router.
func New(box *booking.Service, changes *stream.Broker, page fs.FS, logger *slog.Logger) http.Handler {
	h := handlers{box: box, changes: changes, logger: logger}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/events", h.events)
	mux.HandleFunc("GET /v1/events/{id}/seats", h.seats)
	mux.HandleFunc("GET /v1/events/{id}/stream", h.stream)
	mux.HandleFunc("POST /v1/events/{id}/holds", h.hold)
	mux.HandleFunc("POST /v1/holds/{id}/purchase", h.purchase)
	mux.HandleFunc("DELETE /v1/holds/{id}", h.release)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("/v1/", func(w http.ResponseWriter, _ *http.Request) { problem(w, http.StatusNotFound, "No such API route.") })
	mux.Handle("/", http.FileServerFS(page))
	return mux
}

type handlers struct {
	box     *booking.Service
	changes *stream.Broker
	logger  *slog.Logger
}

type holdRequest struct {
	Seats    []string `json:"seats"`
	Customer string   `json:"customer"`
}

func (h handlers) events(w http.ResponseWriter, r *http.Request) {
	events, err := h.box.Events(r.Context())
	h.respond(w, http.StatusOK, events, err)
}

func (h handlers) seats(w http.ResponseWriter, r *http.Request) {
	seats, err := h.box.Seats(r.Context(), r.PathValue("id"))
	h.respond(w, http.StatusOK, seats, err)
}

func (h handlers) hold(w http.ResponseWriter, r *http.Request) {
	var req holdRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		problem(w, http.StatusBadRequest, "Malformed JSON: "+err.Error())
		return
	}
	hold, err := h.box.Hold(r.Context(), r.PathValue("id"), req.Seats, req.Customer)
	var unavailable *booking.UnavailableError
	if errors.As(err, &unavailable) {
		writeJSON(w, http.StatusConflict, map[string]any{
			"type": "about:blank", "title": "Seats unavailable", "status": http.StatusConflict,
			"detail": unavailable.Error() + ". Nothing was held.", "seats": unavailable.Seats,
		})
		return
	}
	h.respond(w, http.StatusCreated, hold, err)
}

func (h handlers) purchase(w http.ResponseWriter, r *http.Request) {
	order, created, err := h.box.Purchase(r.Context(), r.PathValue("id"))
	status := http.StatusOK // a retry returns the existing order
	if created {
		status = http.StatusCreated
	}
	h.respond(w, status, order, err)
}

func (h handlers) release(w http.ResponseWriter, r *http.Request) {
	if err := h.box.Release(r.Context(), r.PathValue("id")); err != nil {
		h.respond(w, 0, nil, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// stream sends seat changes of one event as Server-Sent Events until the client leaves or falls behind.
func (h handlers) stream(w http.ResponseWriter, r *http.Request) {
	eventID := r.PathValue("id")
	if _, err := h.box.Seats(r.Context(), eventID); err != nil {
		h.respond(w, 0, nil, err)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		problem(w, http.StatusInternalServerError, "Streaming is not supported.")
		return
	}
	sub := h.changes.Subscribe(eventID)
	defer sub.Close()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	_, _ = fmt.Fprint(w, "retry: 2000\n: connected\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(Heartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			_, _ = fmt.Fprint(w, ": heartbeat\n\n")
			flusher.Flush()
		case change, open := <-sub.C:
			if !open {
				return // dropped for being too slow; the client reconnects and reloads the map
			}
			data, _ := json.Marshal(change)
			_, _ = fmt.Fprintf(w, "event: seats\ndata: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func (h handlers) respond(w http.ResponseWriter, status int, body any, err error) {
	switch {
	case errors.Is(err, booking.ErrInvalid):
		problem(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, booking.ErrNotFound):
		problem(w, http.StatusNotFound, err.Error())
	case errors.Is(err, booking.ErrExpired):
		problem(w, http.StatusGone, err.Error())
	case errors.Is(err, booking.ErrConflict):
		problem(w, http.StatusConflict, err.Error())
	case err != nil:
		h.logger.Error("request failed", "error", err)
		problem(w, http.StatusInternalServerError, "Something went wrong.")
	default:
		writeJSON(w, status, body)
	}
}

func problem(w http.ResponseWriter, status int, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"type": "about:blank", "title": http.StatusText(status), "status": status, "detail": detail})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
