// Command marketmaker runs one internal market maker against the public API.
// Run one process (container) per maker account.
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
	strategy := env("T3_MM_STRATEGY", "liquidity")
	seed, err := strconv.ParseUint(env("T3_MM_SEED", strconv.FormatInt(time.Now().UnixNano(), 10)), 10, 64)
	if err != nil {
		return fmt.Errorf("T3_MM_SEED must be an unsigned integer")
	}
	if user == "" || pass == "" {
		return fmt.Errorf("T3_MM_USERNAME and T3_MM_PASSWORD are required")
	}
	log = log.With("maker", user, "strategy", strategy)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	client := marketmaker.NewHTTPClient(api, user, pass)

	// The server may still be starting; retry until the listing loads.
	var tickers []marketmaker.Ticker
	for {
		if tickers, err = client.Tickers(ctx); err == nil {
			break
		}
		log.Info("waiting for API", "err", err)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}

	var s marketmaker.Strategy
	switch strategy {
	case "liquidity":
		l := marketmaker.NewLiquidity(tickers, seed)
		if v := os.Getenv("T3_MM_DEPTH"); v != "" {
			if l.Depth, err = strconv.ParseInt(v, 10, 64); err != nil || l.Depth <= 0 {
				return fmt.Errorf("T3_MM_DEPTH must be a positive integer of cents")
			}
		}
		s = l
	case "flow":
		s = marketmaker.NewFlow(seed)
	case "momentum":
		s = marketmaker.NewMomentum(tickers, seed)
	case "news":
		s = marketmaker.NewNews(tickers, seed)
	default:
		return fmt.Errorf("T3_MM_STRATEGY must be liquidity, flow, momentum or news")
	}
	log.Info("market maker started", "api", api, "symbols", len(tickers))
	return marketmaker.Run(ctx, client, s, marketmaker.Config{Logger: log})
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
