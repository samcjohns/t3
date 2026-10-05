// Command server runs the market engine, ledger, reporting service and API
// gateway in one process. Each is wired to the others only through the
// gateway's ports and the engine's tick callback, so they can later be split
// into separate containers behind network adapters.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/samcjohns/t3/pkg/engine"
	"github.com/samcjohns/t3/pkg/gateway"
	"github.com/samcjohns/t3/pkg/ledger"
	"github.com/samcjohns/t3/pkg/reporting"
)

type config struct {
	addr          string
	heartbeat     time.Duration
	symbols       []string
	startingCash  int64
	origins       []string
	adminUsername string
	adminPassword string
	logLevel      slog.Level
}

func loadConfig() (config, error) {
	c := config{
		addr:          env("T3_ADDR", ":8080"),
		symbols:       split(env("T3_SYMBOLS", "ACME")),
		origins:       split(os.Getenv("T3_ALLOWED_ORIGINS")),
		adminUsername: env("T3_ADMIN_USERNAME", "admin"),
		adminPassword: os.Getenv("T3_ADMIN_PASSWORD"),
	}
	var err error
	if c.heartbeat, err = time.ParseDuration(env("T3_HEARTBEAT", engine.DefaultHeartbeat.String())); err != nil || c.heartbeat <= 0 {
		return c, fmt.Errorf("T3_HEARTBEAT must be a positive duration")
	}
	if c.startingCash, err = strconv.ParseInt(env("T3_STARTING_CASH", "1000000"), 10, 64); err != nil || c.startingCash < 0 {
		return c, fmt.Errorf("T3_STARTING_CASH must be a non-negative integer of cents")
	}
	if err := c.logLevel.UnmarshalText([]byte(env("T3_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("T3_LOG_LEVEL: %w", err)
	}
	if len(c.symbols) == 0 {
		return c, fmt.Errorf("T3_SYMBOLS must list at least one symbol")
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func split(s string) []string {
	var out []string
	for f := range strings.SplitSeq(s, ",") {
		if f = strings.TrimSpace(f); f != "" {
			out = append(out, f)
		}
	}
	return out
}

func main() {
	if err := run(); err != nil {
		slog.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.logLevel}))
	slog.SetDefault(log)

	ldg := ledger.New()
	reports := reporting.New(ldg, reporting.Config{})
	eng := engine.NewOrchestrator(engine.FBAMatcher{}, engine.Config{
		Heartbeat: cfg.heartbeat,
		Clock:     func() time.Time { return time.Now().UTC() },
		OnTick: func(tr engine.TickResult) {
			var volume int64
			for _, b := range tr.Books {
				volume += b.Volume
			}
			log.Debug("tick", "tick", tr.Tick, "books", len(tr.Books), "volume", volume)
			if err := ldg.ApplyTick(tr); err != nil {
				// The ledger refused to settle: balances no longer match the
				// engine. This needs operator attention.
				log.Error("ledger rejected tick", "tick", tr.Tick, "err", err)
			}
			reports.Ingest(tr)
		},
	})
	gw := gateway.New(eng, ldg, reports, gateway.Config{
		Symbols:        cfg.symbols,
		StartingCash:   cfg.startingCash,
		AllowedOrigins: cfg.origins,
		Logger:         log,
	})

	if cfg.adminPassword != "" {
		if _, err := gw.CreateUser(cfg.adminUsername, cfg.adminPassword, gateway.RoleAdmin); err != nil {
			return fmt.Errorf("creating admin user: %w", err)
		}
	} else {
		log.Warn("T3_ADMIN_PASSWORD not set; no admin user, so nobody can seed shares")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv := &http.Server{
		Addr:              cfg.addr,
		Handler:           gw.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	go eng.Run(ctx)
	log.Info("server started", "addr", cfg.addr, "heartbeat", cfg.heartbeat, "symbols", cfg.symbols)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
