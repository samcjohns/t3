package listing

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Validate(Default()); err != nil {
		t.Fatal(err)
	}
	if n := len(Default()); n != 10 {
		t.Fatalf("default listing has %d tickers", n)
	}
}

func TestValidate(t *testing.T) {
	bad := [][]Ticker{
		nil,
		{{Symbol: "acme", ReferencePrice: 1}},
		{{Symbol: "TOOLONG", ReferencePrice: 1}},
		{{Symbol: "A", ReferencePrice: 1}, {Symbol: "A", ReferencePrice: 1}},
		{{Symbol: "A", ReferencePrice: 0}},
		{{Symbol: "A", ReferencePrice: 1, MakerShares: -1}},
	}
	for _, ts := range bad {
		if Validate(ts) == nil {
			t.Errorf("Validate(%+v) accepted", ts)
		}
	}
}

func TestLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "tickers.json")
	os.WriteFile(p, []byte(`[{"symbol":"ABC","name":"ABC Co","reference_price":100,"maker_shares":5}]`), 0o600)
	ts, err := Load(p)
	if err != nil || len(ts) != 1 || ts[0].ReferencePrice != 100 {
		t.Fatalf("Load = %+v, %v", ts, err)
	}
	os.WriteFile(p, []byte(`{`), 0o600)
	if _, err := Load(p); err == nil {
		t.Fatal("Load accepted malformed JSON")
	}
}
