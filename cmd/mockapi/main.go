// Command mockapi serves the Hoserva API from scenario fixtures shared
// with backend tests (doc 06 §8, doc 12 §2), so frontend work never needs
// hoservad, the store, or the loop-device lab. It implements the same
// generated server interfaces hoservad will (D18): a spec change this
// binary doesn't follow fails to compile, not just to run.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ogen-go/ogen/ogenerrors"

	apiv1 "github.com/mdg-labs/hoserva/api/gen/go"
	"github.com/mdg-labs/hoserva/internal/migrate"
	"github.com/mdg-labs/hoserva/web/fixtures"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8090", "loopback listen address (hoservad's own TCP API listens on :8008)")
	scenario := flag.String("scenario", "healthy", "fixture scenario to serve: "+strings.Join(fixtures.Scenarios, ", "))
	flag.Parse()

	if err := run(*addr, *scenario); err != nil {
		fmt.Fprintln(os.Stderr, "mockapi:", err)
		os.Exit(1)
	}
}

func run(addr, scenario string) error {
	if !fixtures.Valid(scenario) {
		return fmt.Errorf("unknown --scenario %q, want one of: %s", scenario, strings.Join(fixtures.Scenarios, ", "))
	}
	if err := requireLoopback(addr); err != nil {
		return err
	}

	apiHandler, err := newHandler(scenario)
	if err != nil {
		return fmt.Errorf("load scenario %q: %w", scenario, err)
	}
	eventsH, err := newEventsHandler(scenario)
	if err != nil {
		return fmt.Errorf("load scenario %q events: %w", scenario, err)
	}

	apiServer, err := newAPIServer(apiHandler)
	if err != nil {
		return err
	}

	mux := http.NewServeMux()
	mux.Handle("/api/v1/events", eventsH)
	mux.Handle("/", flushAppLogs(apiServer))

	httpSrv := &http.Server{Addr: addr, Handler: mux}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		log.Printf("mockapi: serving scenario %q on http://%s/api/v1", scenario, addr)
		errCh <- httpSrv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

const scanPath = "/api/v1/migrate/scan"

// newAPIServer is the generated server with production's refusal of a Flash
// Backup upload past the limit: 413 zip_too_large, like the daemon's.
func newAPIServer(h apiv1.Handler) (http.Handler, error) {
	return newAPIServerWithScanLimit(h, migrate.UploadBodyLimit)
}

func newAPIServerWithScanLimit(h apiv1.Handler, scanLimit int64) (http.Handler, error) {
	srv, err := apiv1.NewServer(h, securityHandler{}, apiv1.WithPathPrefix("/api/v1"),
		apiv1.WithErrorHandler(func(ctx context.Context, w http.ResponseWriter, r *http.Request, err error) {
			if r.URL.Path == scanPath && migrate.IsUploadTooLarge(err) {
				migrate.WriteZipTooLarge(w)
				return
			}
			ogenerrors.DefaultErrorHandler(ctx, w, r, err)
		}))
	if err != nil {
		return nil, fmt.Errorf("build API server: %w", err)
	}
	return limitScanBody(srv, scanLimit), nil
}

func limitScanBody(next http.Handler, limit int64) http.Handler {
	scan := migrate.LimitUploadBody(next, limit, 0)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == scanPath {
			scan.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// requireLoopback keeps the mock, like the rest of this repo's dev tooling,
// off anything but the loopback interface (CLAUDE.md: no host-reachable
// service stands in for the daemon).
func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	if host == "" {
		return fmt.Errorf("invalid --addr %q: a host is required (e.g. 127.0.0.1:8090)", addr)
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("invalid --addr %q: %w", addr, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("invalid --addr %q: mockapi only binds loopback addresses", addr)
		}
	}
	return nil
}
