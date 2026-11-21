package main

import (
	"sort"
	"strings"
)

type Key string

type Group struct {
	Key            map[Key]string `json:"key"`
	Calls          int            `json:"calls"`
	ErrorCalls     int            `json:"error_calls"`
	Compactions    int            `json:"compactions"`
	Input          int64          `json:"input"`
	Output         int64          `json:"output"`
	CacheRead      int64          `json:"cache_read"`
	CacheWrite     int64          `json:"cache_write"`
	Reasoning      int64          `json:"reasoning"`
	TokensBefore   int64          `json:"tokens_before"`
	CostUSD        float64        `json:"cost_usd"`
	PriceSources   map[string]int `json:"price_sources"`
	UnpricedTokens int64          `json:"unpriced_tokens"`
}

var groupKeys = map[Key]func(Row) *string{
	"issue":          func(row Row) *string { return row.Issue },
	"issue_source":   func(row Row) *string { return row.IssueSource },
	"pr":             func(row Row) *string { return row.PR },
	"agent":          func(row Row) *string { return row.Agent },
	"role_tier":      func(row Row) *string { return row.RoleTier },
	"profile":        func(row Row) *string { return row.Profile },
	"model":          func(row Row) *string { return row.Model },
	"thinking_level": func(row Row) *string { return row.ThinkingLevel },
	"seat":           func(row Row) *string { return row.Seat },
	"lane":           func(row Row) *string { return row.Lane },
	"corpus_rev":     func(row Row) *string { return row.CorpusRev },
	"corpus_digest":  func(row Row) *string { return row.CorpusDigest },
	"prompt_digest":  func(row Row) *string { return row.PromptDigest },
	"experiment":     func(row Row) *string { return row.Experiment },
}

func Aggregate(rows []Row, table PriceTable, by []Key) []Group {
	groups := make(map[string]*Group)
	for _, row := range rows {
		if row.Kind != kindMessage && row.Kind != kindModelUsage && row.Kind != kindCompaction {
			continue
		}
		keyValues := make(map[Key]string, len(by))
		parts := make([]string, 0, len(by))
		for _, key := range by {
			value := "untagged"
			if field, ok := groupKeys[key]; ok {
				if fieldValue := field(row); fieldValue != nil {
					value = *fieldValue
				}
			}
			keyValues[key] = value
			parts = append(parts, string(key)+"="+value)
		}
		keyString := strings.Join(parts, "\x00")
		group := groups[keyString]
		if group == nil {
			group = &Group{Key: keyValues, PriceSources: make(map[string]int)}
			groups[keyString] = group
		}
		if row.Kind == kindCompaction {
			group.Compactions++
			if row.TokensBefore != nil {
				group.TokensBefore += *row.TokensBefore
			}
			continue
		}
		group.Calls++
		if row.ErrorClass != nil && *row.ErrorClass != "" {
			group.ErrorCalls++
		}
		group.Input += int64Value(row.Input)
		group.Output += int64Value(row.Output)
		group.CacheRead += int64Value(row.CacheRead)
		group.CacheWrite += int64Value(row.CacheWrite)
		group.Reasoning += int64Value(row.Reasoning)
		cost, source := Reprice(row, table)
		group.PriceSources[source]++
		if cost != nil {
			group.CostUSD += *cost
		} else {
			group.UnpricedTokens += int64Value(row.Input) + int64Value(row.Output) + int64Value(row.CacheRead) + int64Value(row.CacheWrite)
		}
	}
	result := make([]Group, 0, len(groups))
	for _, group := range groups {
		result = append(result, *group)
	}
	sort.Slice(result, func(i, j int) bool {
		return groupIdentity(result[i].Key, by) < groupIdentity(result[j].Key, by)
	})
	return result
}

func groupIdentity(values map[Key]string, by []Key) string {
	parts := make([]string, 0, len(by))
	for _, key := range by {
		parts = append(parts, string(key)+"="+values[key])
	}
	return strings.Join(parts, "\x00")
}

func int64Value(value *int64) int64 {
	if value == nil {
		return 0
	}
	return *value
}
