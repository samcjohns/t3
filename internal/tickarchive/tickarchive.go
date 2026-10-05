// Package tickarchive keeps the engine's original tick results, one gzipped
// JSON Lines file per UTC day, before the engine compacts them.
package tickarchive

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"time"
)

// Dir is a directory of daily archives named ticks-YYYY-MM-DD.jsonl.gz.
type Dir string

// Path is the archive file for day.
func (d Dir) Path(day time.Time) string {
	return filepath.Join(string(d), "ticks-"+day.UTC().Format(time.DateOnly)+".jsonl.gz")
}

// Write archives one day's results, one per line, replacing any earlier
// archive of that day. The file only appears once it is complete and synced.
func (d Dir) Write(_ context.Context, day time.Time, results iter.Seq2[json.RawMessage, error]) error {
	f, err := os.CreateTemp(string(d), ".ticks-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // fails harmlessly once renamed
	defer f.Close()

	zw := gzip.NewWriter(f)
	for raw, err := range results {
		if err != nil {
			return err
		}
		if _, err := zw.Write(append(raw, '\n')); err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), d.Path(day)); err != nil {
		return err
	}
	// Sync the directory so the rename itself survives a crash.
	dir, err := os.Open(string(d))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
