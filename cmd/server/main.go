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
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/samcjohns/t3/internal/tickarchive"
	"github.com/samcjohns/t3/pkg/engine"
	enginepg "github.com/samcjohns/t3/pkg/engine/postgres"
	"github.com/samcjohns/t3/pkg/gateway"
	gatewaypg "github.com/samcjohns/t3/pkg/gateway/postgres"
	"github.com/samcjohns/t3/pkg/ledger"
	ledgerpg "github.com/samcjohns/t3/pkg/ledger/postgres"
	"github.com/samcjohns/t3/pkg/listing"
	"github.com/samcjohns/t3/pkg/reporting"
)

type config struct {
	addr          string
	heartbeat     time.Duration
	tickers       []listing.Ticker
	makers        []string
	makerPassword string
	makerCash     int64
	proxies       []netip.Prefix
	startingCash  int64
	origins       []string
	adminUsername string
	adminPassword string
	logLevel      slog.Level
	engineDB      string
	ledgerDB      string
	gatewayDB     string
	compactAfter  time.Duration
	archiveDir    string
}

func loadConfig() (config, error) {
	c := config{
		addr:          env("T3_ADDR", ":8080"),
		makers:        split(env("T3_MARKET_MAKERS", "mm-liquidity,mm-flow")),
		makerPassword: os.Getenv("T3_MARKET_MAKER_PASSWORD"),
		origins:       split(os.Getenv("T3_ALLOWED_ORIGINS")),
		adminUsername: env("T3_ADMIN_USERNAME", "admin"),
		adminPassword: os.Getenv("T3_ADMIN_PASSWORD"),
		engineDB:      env("T3_ENGINE_DATABASE_URL", os.Getenv("T3_DATABASE_URL")),
		ledgerDB:      env("T3_LEDGER_DATABASE_URL", os.Getenv("T3_DATABASE_URL")),
		gatewayDB:     env("T3_GATEWAY_DATABASE_URL", os.Getenv("T3_DATABASE_URL")),
		archiveDir:    os.Getenv("T3_ARCHIVE_DIR"),
	}
	var err error
	if c.heartbeat, err = time.ParseDuration(env("T3_HEARTBEAT", engine.DefaultHeartbeat.String())); err != nil || c.heartbeat <= 0 {
		return c, fmt.Errorf("T3_HEARTBEAT must be a positive duration")
	}
	if c.compactAfter, err = time.ParseDuration(env("T3_COMPACT_AFTER", "24h")); err != nil || c.compactAfter < 0 {
		return c, fmt.Errorf("T3_COMPACT_AFTER must be a non-negative duration")
	}
	if c.startingCash, err = strconv.ParseInt(env("T3_STARTING_CASH", "1000000"), 10, 64); err != nil || c.startingCash < 0 {
		return c, fmt.Errorf("T3_STARTING_CASH must be a non-negative integer of cents")
	}
	if err := c.logLevel.UnmarshalText([]byte(env("T3_LOG_LEVEL", "info"))); err != nil {
		return c, fmt.Errorf("T3_LOG_LEVEL: %w", err)
	}
	if c.makerCash, err = strconv.ParseInt(env("T3_MARKET_MAKER_CASH", "500000000"), 10, 64); err != nil || c.makerCash < 0 {
		return c, fmt.Errorf("T3_MARKET_MAKER_CASH must be a non-negative integer of cents")
	}
	for _, cidr := range split(os.Getenv("T3_TRUSTED_PROXIES")) {
		p, err := netip.ParsePrefix(cidr)
		if err != nil {
			return c, fmt.Errorf("T3_TRUSTED_PROXIES: %w", err)
		}
		c.proxies = append(c.proxies, p)
	}
	c.tickers = listing.Default()
	if path := os.Getenv("T3_TICKERS_FILE"); path != "" {
		if c.tickers, err = listing.Load(path); err != nil {
			return c, fmt.Errorf("T3_TICKERS_FILE: %w", err)
		}
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
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
	}
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	stores, closeStores, err := openStores(ctx, cfg, log)
	if err != nil {
		return err
	}
	defer closeStores()

	// Recover in dependency order: the engine's journal is the record the
	// ledger and reporting service rebuild from.
	ldg, err := ledger.Open(ctx, ledger.Config{Store: stores.ledger})
	if err != nil {
		return err
	}
	reports := reporting.New(ldg, reporting.Config{Listing: cfg.tickers, Heartbeat: cfg.heartbeat})
	var eng *engine.Orchestrator
	eng, err = engine.OpenOrchestrator(ctx, engine.FBAMatcher{}, engine.Config{
		Heartbeat: cfg.heartbeat,
		Clock:     func() time.Time { return time.Now().UTC() },
		Journal:   stores.journal,
		OnTick: func(tr engine.TickResult) {
			var volume int64
			for _, b := range tr.Books {
				volume += b.Volume
			}
			log.Debug("tick", "tick", tr.Tick, "books", len(tr.Books), "volume", volume)
			settle(log, ldg, eng, tr)
			reports.Ingest(tr)
		},
		OnTickError: func(err error) { log.Error("tick failed; retrying next heartbeat", "err", err) },
	})
	if err != nil {
		return err
	}
	if stores.journal != nil {
		if err := ldg.CatchUp(ctx, eng); err != nil {
			return fmt.Errorf("ledger catch-up: %w", err)
		}
		reconcile(ctx, log, ldg, eng)
		if err := reports.Replay(ctx, eng); err != nil {
			return fmt.Errorf("reporting replay: %w", err)
		}
	}
	log.Info("state recovered", "engine_tick", eng.LastTick(), "ledger_tick", ldg.LastTick())

	gw := gateway.New(eng, ldg, reports, gateway.Config{
		Tickers:        cfg.tickers,
		StartingCash:   cfg.startingCash,
		AllowedOrigins: cfg.origins,
		TrustedProxies: cfg.proxies,
		Users:          stores.users,
		Logger:         log,
	})

	if cfg.adminPassword != "" {
		if _, err := gw.EnsureUser(ctx, cfg.adminUsername, cfg.adminPassword, gateway.RoleAdmin); err != nil {
			return fmt.Errorf("creating admin user: %w", err)
		}
	} else {
		log.Warn("T3_ADMIN_PASSWORD not set; no admin user, so nobody can seed shares")
	}
	if err := bootstrapMakers(ctx, cfg, gw, ldg); err != nil {
		return err
	}
	if cfg.makerPassword == "" {
		log.Warn("T3_MARKET_MAKER_PASSWORD not set; no market makers, so prices only move on player trades")
	}

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
	engineDone := make(chan struct{})
	go func() { eng.Run(ctx); close(engineDone) }()
	if stores.journal != nil {
		go func() {
			t := time.NewTicker(time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					reconcile(ctx, log, ldg, eng)
				}
			}
		}()
	}
	if stores.engine != nil && cfg.compactAfter > 0 {
		archive, err := archiver(cfg, log)
		if err != nil {
			return err
		}
		go compactLoop(ctx, log, stores.engine, ldg, cfg.compactAfter, archive)
	}
	log.Info("server started", "addr", cfg.addr, "trusted_proxies", cfg.proxies, "heartbeat", cfg.heartbeat, "symbols", listing.Symbols(cfg.tickers))

	select {
	case err := <-errc:
		stop()
		<-engineDone
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	<-engineDone // let an in-progress tick commit before the database closes
	return nil
}

// healthcheck probes this server's /readyz, for container health checks:
// the image has no shell or curl. It exits 0 when ready.
func healthcheck() int {
	_, port, err := net.SplitHostPort(env("T3_ADDR", ":8080"))
	if err != nil {
		return 1
	}
	client := http.Client{Timeout: 3 * time.Second}
	res, err := client.Get("http://127.0.0.1:" + port + "/readyz")
	if err != nil {
		return 1
	}
	res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

// bootstrapMakers creates each market maker user and seeds it with cash and
// every listed symbol's maker shares. References make seeding idempotent, so
// restarts never re-credit, and a newly listed symbol is seeded once.
func bootstrapMakers(ctx context.Context, cfg config, gw *gateway.Gateway, ldg *ledger.Ledger) error {
	if cfg.makerPassword == "" {
		return nil
	}
	for _, name := range cfg.makers {
		u, err := gw.EnsureUser(ctx, name, cfg.makerPassword, gateway.RoleMarketMaker)
		if err != nil {
			return fmt.Errorf("creating market maker %s: %w", name, err)
		}
		if cfg.makerCash > 0 {
			if err := ldg.Deposit(ctx, u.ID, cfg.makerCash, "seed-cash"); err != nil {
				return fmt.Errorf("seeding %s: %w", name, err)
			}
		}
		for _, t := range cfg.tickers {
			if t.MakerShares == 0 {
				continue
			}
			if err := ldg.DepositShares(ctx, u.ID, t.Symbol, t.MakerShares, "seed:"+t.Symbol); err != nil {
				return fmt.Errorf("seeding %s with %s: %w", name, t.Symbol, err)
			}
		}
	}
	return nil
}

// settle applies a tick to the ledger, catching up first if it missed any.
func settle(log *slog.Logger, ldg *ledger.Ledger, eng *engine.Orchestrator, tr engine.TickResult) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := ldg.ApplyTick(ctx, tr)
	if errors.Is(err, ledger.ErrTickOutOfOrder) {
		err = ldg.CatchUp(ctx, eng)
	}
	if err != nil {
		// Order entry halts once the lag exceeds the gateway's limit; the
		// next tick retries via catch-up.
		log.Error("ledger did not settle tick", "tick", tr.Tick, "ledger_tick", ldg.LastTick(), "err", err)
	}
}

// archiver returns where compaction archives each day's original ticks, or
// nil to discard them.
func archiver(cfg config, log *slog.Logger) (enginepg.ArchiveFunc, error) {
	if cfg.archiveDir == "" {
		log.Warn("T3_ARCHIVE_DIR not set; compaction discards expired order IDs without archiving the original ticks")
		return nil, nil
	}
	if err := os.MkdirAll(cfg.archiveDir, 0o750); err != nil {
		return nil, fmt.Errorf("T3_ARCHIVE_DIR: %w", err)
	}
	return tickarchive.Dir(cfg.archiveDir).Write, nil
}

// compactLoop compacts each day of engine history once it is older than
// after and the ledger has settled it, checking hourly.
func compactLoop(ctx context.Context, log *slog.Logger, j *enginepg.Journal, ldg *ledger.Ledger, after time.Duration, archive enginepg.ArchiveFunc) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		days, err := j.Compact(ctx, time.Now().UTC().Add(-after), ldg.LastTick(), archive)
		for _, d := range days {
			log.Info("compacted history", "day", d.Day.Format(time.DateOnly), "ticks", d.LastTick-d.FirstTick+1, "orders_deleted", d.OrdersDeleted)
		}
		if err != nil && ctx.Err() == nil {
			log.Error("compacting history; retrying next hour", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func reconcile(ctx context.Context, log *slog.Logger, ldg *ledger.Ledger, eng *engine.Orchestrator) {
	released, err := ldg.ReconcileHolds(ctx, eng, 30*time.Second)
	if err != nil {
		log.Error("reconciling holds", "err", err)
		return
	}
	if len(released) > 0 {
		log.Warn("released orphaned holds", "order_ids", released)
	}
}

type stores struct {
	journal engine.Journal
	// engine is journal's Postgres implementation, which can also compact.
	engine *enginepg.Journal
	ledger ledger.Store
	users  gateway.UserStore
}

// openStores connects each service to its own database role. Without any
// database URL the system runs in memory only.
func openStores(ctx context.Context, cfg config, log *slog.Logger) (stores, func(), error) {
	var s stores
	var pools []*pgxpool.Pool
	closeAll := func() {
		for _, p := range pools {
			p.Close()
		}
	}
	if cfg.engineDB == "" && cfg.ledgerDB == "" && cfg.gatewayDB == "" {
		log.Warn("no database configured; running in memory, all state is lost on exit")
		return s, closeAll, nil
	}
	connect := func(name, url string) (*pgxpool.Pool, error) {
		if url == "" {
			return nil, fmt.Errorf("no database URL for the %s; set T3_DATABASE_URL or T3_%s_DATABASE_URL", name, strings.ToUpper(name))
		}
		p, err := pgxpool.New(ctx, url)
		if err != nil {
			return nil, err
		}
		pools = append(pools, p)
		return p, p.Ping(ctx)
	}
	fail := func(err error) (stores, func(), error) { closeAll(); return stores{}, nil, err }

	p, err := connect("engine", cfg.engineDB)
	if err != nil {
		return fail(err)
	}
	if s.engine, err = enginepg.NewJournal(ctx, p); err != nil {
		return fail(err)
	}
	s.journal = s.engine
	if p, err = connect("ledger", cfg.ledgerDB); err != nil {
		return fail(err)
	}
	if s.ledger, err = ledgerpg.NewStore(ctx, p); err != nil {
		return fail(err)
	}
	if p, err = connect("gateway", cfg.gatewayDB); err != nil {
		return fail(err)
	}
	if s.users, err = gatewaypg.NewUserStore(ctx, p); err != nil {
		return fail(err)
	}
	return s, closeAll, nil
}
