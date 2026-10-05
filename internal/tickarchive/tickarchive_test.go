package tickarchive

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"os"
	"testing"
	"time"
)

func seq(lines ...string) iter.Seq2[json.RawMessage, error] {
	return func(yield func(json.RawMessage, error) bool) {
		for _, l := range lines {
			if !yield(json.RawMessage(l), nil) {
				return
			}
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteReplacesDayAtomically(t *testing.T) {
	d := Dir(t.TempDir())
	day := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	ctx := context.Background()

	if err := d.Write(ctx, day, seq(`{"tick":1}`, `{"tick":2}`)); err != nil {
		t.Fatal(err)
	}
	if d.Path(day) != string(d)+"/ticks-2026-10-04.jsonl.gz" {
		t.Fatalf("Path = %s", d.Path(day))
	}
	if got := read(t, d.Path(day)); got != "{\"tick\":1}\n{\"tick\":2}\n" {
		t.Fatalf("archive = %q", got)
	}

	// A failed rewrite leaves the earlier archive and no temporary file.
	boom := errors.New("query failed")
	failing := func(yield func(json.RawMessage, error) bool) {
		if yield(json.RawMessage(`{"tick":1}`), nil) {
			yield(nil, boom)
		}
	}
	if err := d.Write(ctx, day, failing); !errors.Is(err, boom) {
		t.Fatalf("Write = %v, want %v", err, boom)
	}
	if err := d.Write(ctx, day, seq(`{"tick":3}`)); err != nil {
		t.Fatal(err)
	}
	if got := read(t, d.Path(day)); got != "{\"tick\":3}\n" {
		t.Fatalf("archive after rewrite = %q", got)
	}
	entries, err := os.ReadDir(string(d))
	if err != nil || len(entries) != 1 {
		t.Fatalf("directory holds %d entries, %v", len(entries), err)
	}
}
