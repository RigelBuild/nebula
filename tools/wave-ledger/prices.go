package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"math"
	"os"
)

//go:embed prices.json
var defaultPriceData []byte

// Price stores per-million-token rates in USD.
type Price struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Basis      string  `json:"basis"`
}

type PriceTable map[string]Price

func LoadPrices(path string) (PriceTable, error) {
	data := defaultPriceData
	if path != "" {
		loaded, err := os.ReadFile(path) //nolint:gosec // operator-supplied path
		if err != nil {
			return nil, errors.New("reading price table")
		}
		data = loaded
	}
	var table PriceTable
	if err := json.Unmarshal(data, &table); err != nil {
		return nil, errors.New("decoding price table")
	}
	if table == nil {
		return nil, errors.New("price table must be a JSON object")
	}
	for _, price := range table {
		if !finite(price.Input) || !finite(price.Output) || !finite(price.CacheRead) || !finite(price.CacheWrite) {
			return nil, errors.New("price table contains invalid rate")
		}
	}
	return table, nil
}

// Reprice trusts explicit rates, then fixed catalog costs, and leaves other rows unpriced.
func Reprice(row Row, table PriceTable) (*float64, string) {
	if row.Model != nil {
		if price, ok := table[*row.Model]; ok {
			return tableCost(row, price), "table"
		}
	}
	if row.PriceStatus != nil && *row.PriceStatus == "fixed" && row.CostUSD != nil {
		return row.CostUSD, "catalog"
	}
	return nil, "none"
}

func tableCost(row Row, price Price) *float64 {
	var total float64
	if row.Input != nil {
		total += float64(*row.Input) * price.Input
	}
	if row.Output != nil {
		total += float64(*row.Output) * price.Output
	}
	if row.CacheRead != nil {
		total += float64(*row.CacheRead) * price.CacheRead
	}
	if row.CacheWrite != nil {
		total += float64(*row.CacheWrite) * price.CacheWrite
	}
	cost := total / 1_000_000
	return &cost
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
