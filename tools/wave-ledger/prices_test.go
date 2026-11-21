package main

import (
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadPricesAcceptsEmptyTable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prices.json")
	if err := os.WriteFile(path, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	table, err := LoadPrices(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(table) != 0 {
		t.Fatalf("LoadPrices() table has %d entries, want empty", len(table))
	}
}

func TestRepriceUsesTableCatalogOrNone(t *testing.T) {
	table := PriceTable{
		"model-a": {
			Input: 1, Output: 2, CacheRead: 0.5, CacheWrite: 1.5, Basis: "list",
		},
	}
	input, output, cacheRead, cacheWrite := int64(1_000_000), int64(500_000), int64(200_000), int64(100_000)
	catalogCost, fixed, notFixed := 9.0, "fixed", "unknown"
	tests := []struct {
		name       string
		row        Row
		wantCost   *float64
		wantSource string
	}{
		{
			name:       "table rates apply to token usage",
			row:        Row{Model: new("model-a"), Input: &input, Output: &output, CacheRead: &cacheRead, CacheWrite: &cacheWrite},
			wantCost:   new(2.25),
			wantSource: "table",
		},
		{
			name:       "fixed price uses catalog cost",
			row:        Row{Model: new("model-b"), CostUSD: &catalogCost, PriceStatus: &fixed},
			wantCost:   &catalogCost,
			wantSource: "catalog",
		},
		{
			name:       "unfixed price without table is unpriced",
			row:        Row{Model: new("model-b"), PriceStatus: &notFixed},
			wantCost:   nil,
			wantSource: "none",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotCost, gotSource := Reprice(test.row, table)
			if gotSource != test.wantSource {
				t.Fatalf("Reprice() source = %q, want %q", gotSource, test.wantSource)
			}
			if test.wantCost == nil {
				if gotCost != nil {
					t.Fatalf("Reprice() cost = %v, want nil", *gotCost)
				}
				return
			}
			if gotCost == nil || math.Abs(*gotCost-*test.wantCost) > 1e-9 {
				t.Fatalf("Reprice() cost = %v, want %v", gotCost, *test.wantCost)
			}
		})
	}
}
