// Command linkbot runs the link maker: a market maker, independent of the
// others, that pulls linked symbols' prices towards outside values. See
// pkg/link and links.example.json.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/samcjohns/t3/pkg/link"
	"github.com/samcjohns/t3/pkg/marketmaker"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil && err != context.Canceled {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	api := env("T3_API_URL", "http://localhost:8080")
	user, pass := os.Getenv("T3_MM_USERNAME"), os.Getenv("T3_MM_PASSWORD")
	seed, err := strconv.ParseUint(env("T3_MM_SEED", strconv.FormatInt(time.Now().UnixNano(), 10)), 10, 64)
	if err != nil {
		return fmt.Errorf("T3_MM_SEED must be an unsigned integer")
	}
	if user == "" || pass == "" {
		return fmt.Errorf("T3_MM_USERNAME and T3_MM_PASSWORD are required")
	}
	log = log.With("maker", user, "strategy", "link")

	src := link.NewFetcher(os.Getenv("T3_FINNHUB_API_KEY"), os.Getenv("T3_FRED_API_KEY"))
	m, err := link.New(env("T3_LINK_FILE", "/etc/t3/links/links.json"), env("T3_LINK_STATE", "/var/lib/t3/link/state.json"), src, seed, log)
	if err != nil {
		return fmt.Errorf("loading link state: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go m.Watch(ctx, 15*time.Minute)

	client := marketmaker.NewHTTPClient(api, user, pass)
	for {
		if _, err = client.Tickers(ctx); err == nil {
			break
		}
		log.Info("waiting for API", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	log.Info("link maker started", "api", api)
	defer m.Save()
	return marketmaker.Run(ctx, client, m, marketmaker.Config{Strategy: "link", Logger: log})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
