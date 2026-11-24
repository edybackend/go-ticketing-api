// Command tickets runs the box office: the HTTP API, the live seat map and the sweeper of expired holds.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/edybackend/go-ticketing-api/internal/booking"
	"github.com/edybackend/go-ticketing-api/internal/httpapi"
	"github.com/edybackend/go-ticketing-api/internal/stream"
	"github.com/edybackend/go-ticketing-api/web"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("service stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	holdTTL := duration("HOLD_TTL", 5*time.Minute)
	changes := stream.NewBroker(32)
	box, err := booking.Open(ctx, env("DB_PATH", "data/tickets.db"), time.Now, holdTTL, changes)
	if err != nil {
		return err
	}
	defer func() { _ = box.Close() }()

	addr := env("HTTP_ADDR", ":8080")
	server := &http.Server{
		Addr:              addr,
		Handler:           httpapi.New(box, changes, web.Page, logger),
		ReadHeaderTimeout: 5 * time.Second,
		// Requests inherit the signal context: on shutdown, open SSE streams see it cancelled and end, instead of
		// keeping Shutdown waiting until its timeout.
		BaseContext: func(net.Listener) context.Context { return ctx },
	}

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		logger.Info("http listening", "addr", addr, "holdTTL", holdTTL.String())
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	})
	g.Go(func() error { return sweep(ctx, box, duration("SWEEP_EVERY", 5*time.Second), logger) })
	g.Go(func() error {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return server.Shutdown(shutdown)
	})
	return g.Wait()
}

// sweep releases expired holds on a timer, so seats come back on sale (and the stream says so) without a request.
func sweep(ctx context.Context, box *booking.Service, every time.Duration, logger *slog.Logger) error {
	ticker := time.NewTicker(every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			n, err := box.SweepExpired(ctx)
			if err != nil && ctx.Err() == nil {
				logger.Error("sweep expired holds", "error", err)
			} else if n > 0 {
				logger.Info("released expired holds", "seats", n)
			}
		}
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func duration(name string, fallback time.Duration) time.Duration {
	if d, err := time.ParseDuration(os.Getenv(name)); err == nil && d > 0 {
		return d
	}
	return fallback
}
