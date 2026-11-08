// Package main runs a metadata server using Hardcover as an upstream.
package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/alecthomas/kong"
	"github.com/blampe/rreading-glasses/cmd"
	"github.com/blampe/rreading-glasses/internal"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/prometheus/client_golang/prometheus"
)

// cli contains our command-line flags.
type cli struct {
	Serve server `cmd:"" help:"Run an HTTP server."`

	Bust cmd.Bust `cmd:"" help:"Bust cache entries."`

	Backfill backfill `cmd:"" help:"Copy every cached author, work, book and series into the store."`
}

type backfill struct {
	cmd.PGConfig
	cmd.LogConfig

	Keys []string `arg:"" optional:"" help:"Only these cache keys, e.g. a123 w456."`
}

func (b *backfill) Run() error {
	_ = b.LogConfig.Run()
	ctx := context.Background()
	store, err := internal.NewStore(ctx, b.DSN())
	if err != nil {
		return fmt.Errorf("setting up store: %w", err)
	}
	_, err = store.Backfill(ctx, b.Keys...)
	return err
}

type server struct {
	cmd.PGConfig
	cmd.LogConfig
	cmd.CloudflareConfig

	Port     int    `default:"8788" env:"PORT" help:"Port to serve traffic on."`
	Proxy    string `default:"" env:"PROXY" help:"HTTP proxy URL to use for upstream requests."`
	Upstream string `default:"api.hardcover.app" env:"UPSTREAM" help:"Upstream host (e.g. www.example.com)."`

	HardcoverAuth     string `required:"" env:"HARDCOVER_AUTH" xor:"hardcover-auth" help:"Hardcover Authorization header, e.g. 'Bearer ...'"`
	HardcoverAuthFile []byte `required:"" type:"filecontent" xor:"hardcover-auth" env:"HARDCOVER_AUTH_FILE" help:"File containing the Hardcover Authorization header, e.g. 'Bearer ...'"`

	AdminPort      int    `default:"8789" env:"ADMIN_PORT" help:"Port for the curation API."`
	AdminToken     string `env:"ADMIN_TOKEN" help:"Bearer token for the curation API; it's off without one."`
	AdminTokenFile []byte `type:"filecontent" env:"ADMIN_TOKEN_FILE" help:"File containing the curation API token."`

	HardcoverDailyBudget int64 `default:"40000" env:"HARDCOVER_DAILY_BUDGET" help:"Max Hardcover requests per rolling 24h (0 = no cap). Background refreshes stop at 80%."`
}

func (s *server) Run() error {
	_ = s.LogConfig.Run()
	reg := internal.NewMetrics()

	cf, err := s.Cache(reg)
	if err != nil {
		return fmt.Errorf("setting up cloudflare: %w", err)
	}

	ctx := context.Background()
	layered, err := internal.NewCache(ctx, s.DSN(), cf, reg)
	if err != nil {
		return fmt.Errorf("setting up cache: %w", err)
	}
	store, err := internal.NewStore(ctx, s.DSN())
	if err != nil {
		return fmt.Errorf("setting up store: %w", err)
	}
	cache := internal.NewStoreCache(layered, store)
	go store.BackfillOnce(context.WithValue(ctx, middleware.RequestIDKey, "backfill"))

	if len(s.HardcoverAuthFile) > 0 {
		s.HardcoverAuth = string(bytes.TrimSpace(s.HardcoverAuthFile))
	}

	// Inside RetryTransport so every attempt counts against the budget.
	budget := &internal.BudgetTransport{RoundTripper: http.DefaultTransport, Daily: s.HardcoverDailyBudget}
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "rg_upstream_requests_24h",
		Help: "Upstream requests sent in the last 24h.",
	}, func() float64 { return float64(budget.Used()) }))

	hcTransport := internal.ScopedTransport{
		Host: s.Upstream,
		RoundTripper: &internal.HeaderTransport{
			Key:          "Authorization",
			Value:        s.HardcoverAuth,
			RoundTripper: &internal.RetryTransport{RoundTripper: budget},
		},
	}

	hcClient := &http.Client{Transport: hcTransport}

	gql, err := internal.NewBatchedGraphQLClient("https://api.hardcover.app/v1/graphql", hcClient, time.Second, 25 /* Not sure about this */, reg)
	if err != nil {
		return err
	}

	getter, err := internal.NewHardcoverGetter(cache, gql)
	if err != nil {
		return err
	}

	persister, err := internal.NewPersister(ctx, cache, s.DSN())
	if err != nil {
		return err
	}

	ctrl, err := internal.NewController(cache, getter, persister, reg)
	if err != nil {
		return err
	}
	ctrl.SetBudget(budget)
	h := internal.NewHandler(ctrl)
	mux := internal.NewMux(h, reg)

	mux = middleware.RequestSize(1024)(mux)  // Limit request bodies.
	mux = internal.Requestlogger{}.Wrap(mux) // Log requests.
	mux = middleware.RequestID(mux)          // Include a request ID header.
	mux = middleware.Recoverer(mux)          // Recover from panics.

	// TODO: The client doesn't send Accept-Encoding and doesn't handle
	// Content-Encoding responses. This would allow us to send compressed bytes
	// directly from the cache.

	addr := fmt.Sprintf(":%d", s.Port)
	server := &http.Server{
		Handler:  mux,
		Addr:     addr,
		ErrorLog: slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}

	go func() {
		slog.Info("listening on " + addr)
		err := server.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			internal.Log(ctx).Error(err.Error())
			os.Exit(1)
		}
	}()

	if len(s.AdminTokenFile) > 0 {
		s.AdminToken = string(bytes.TrimSpace(s.AdminTokenFile))
	}
	if s.AdminToken != "" {
		go serveAdmin(ctx, store, s.AdminPort, s.AdminToken)
	} else {
		slog.Info("curation API disabled: no admin token")
	}

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt)

	go func() {
		<-shutdown
		os.Exit(0)
		// slog.Info("shutting down http server")
		// _ = server.Shutdown(ctx)
		// slog.Info("waiting for denormalization to finish")
		// ctrl.Shutdown(ctx)
	}()

	ctrl.Run(ctx)

	slog.Info("au revoir!")

	return nil
}

func serveAdmin(ctx context.Context, store *internal.Store, port int, token string) {
	mux := internal.NewAdminMux(store, token)
	mux = internal.Requestlogger{}.Wrap(mux)
	mux = middleware.RequestID(mux)
	mux = middleware.Recoverer(mux)

	addr := fmt.Sprintf(":%d", port)
	server := &http.Server{
		Handler:           mux,
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		ErrorLog:          slog.NewLogLogger(slog.Default().Handler(), slog.LevelError),
	}
	slog.Info("curation API listening on " + addr)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		internal.Log(ctx).Error("curation API stopped", "err", err)
	}
}

func main() {
	kctx := kong.Parse(&cli{})
	err := kctx.Run()
	if err != nil {
		internal.Log(context.Background()).Error("fatal", "err", err)
		os.Exit(1)
	}
}
