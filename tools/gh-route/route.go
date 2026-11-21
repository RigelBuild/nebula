package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type Config struct {
	Floor    int64
	CacheTTL int64
	MaxWait  int64
}

type Bucket struct {
	Remaining *int64 `json:"remaining,omitempty"`
	Limit     *int64 `json:"limit,omitempty"`
	Reset     *int64 `json:"reset,omitempty"`
}

type Resources struct {
	Core    *Bucket `json:"core,omitempty"`
	GraphQL *Bucket `json:"graphql,omitempty"`
}

type RateLimit struct {
	Resources *Resources `json:"resources,omitempty"`
	FetchedAt int64      `json:"_fetched_at,omitempty"`
}

func intEnv(value string, exists bool, fallback int64) int64 {
	if !exists || value == "" {
		return fallback
	}
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	end := 0
	if value[0] == '+' || value[0] == '-' {
		end = 1
	}
	startDigits := end
	for end < len(value) && value[end] >= '0' && value[end] <= '9' {
		end++
	}
	if end == startDigits {
		return fallback
	}
	parsed, err := strconv.ParseInt(value[:end], 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func readConfig(getenv func(string) (string, bool)) Config {
	floor, hasFloor := getenv("GH_ROUTE_FLOOR")
	cacheTTL, hasCacheTTL := getenv("GH_ROUTE_CACHE_TTL")
	maxWait, hasMaxWait := getenv("GH_ROUTE_MAX_WAIT")
	return Config{
		Floor:    intEnv(floor, hasFloor, 200),
		CacheTTL: intEnv(cacheTTL, hasCacheTTL, 15),
		MaxWait:  intEnv(maxWait, hasMaxWait, 300),
	}
}

func value(pointer *int64) int64 {
	if pointer == nil {
		return 0
	}
	return *pointer
}

func fraction(bucket *Bucket) float64 {
	if bucket == nil || value(bucket.Limit) <= 0 {
		return 0
	}
	return float64(value(bucket.Remaining)) * 1000 / float64(value(bucket.Limit))
}

func pick(rateLimit RateLimit, floor int64) string {
	var rest, graphql *Bucket
	if rateLimit.Resources != nil {
		rest = rateLimit.Resources.Core
		graphql = rateLimit.Resources.GraphQL
	}
	restRemaining := value(bucketRemaining(rest))
	graphqlRemaining := value(bucketRemaining(graphql))
	if graphqlRemaining <= floor && restRemaining > floor {
		return "rest"
	}
	if restRemaining <= floor && graphqlRemaining > floor {
		return graphqlCommand
	}
	if fraction(graphql) > fraction(rest) {
		return graphqlCommand
	}
	return "rest"
}

func bucketRemaining(bucket *Bucket) *int64 {
	if bucket == nil {
		return nil
	}
	return bucket.Remaining
}

func remaining(rateLimit RateLimit, which string) int64 {
	if rateLimit.Resources == nil {
		return 0
	}
	if which == graphqlCommand {
		return value(bucketRemaining(rateLimit.Resources.GraphQL))
	}
	return value(bucketRemaining(rateLimit.Resources.Core))
}

type Repo struct {
	Owner string
	Name  string
}

func splitRepo(repo string) Repo {
	owner, _, found := strings.Cut(repo, "/")
	if !found {
		return Repo{Owner: repo, Name: repo}
	}
	_, name, found := strings.CutLast(repo, "/")
	if !found {
		return Repo{Owner: owner, Name: repo}
	}
	return Repo{Owner: owner, Name: name}
}

func parseJSONStream(text string) []any {
	values := []any{}
	start := -1
	stack := []byte{}
	inString := false
	escaped := false
	for index := range len(text) {
		character := text[index]
		if start == -1 {
			if character != '{' && character != '[' {
				continue
			}
			start = index
			stack = append(stack, character)
			continue
		}
		if inString {
			switch {
			case escaped:
				escaped = false
			case character == '\\':
				escaped = true
			case character == '"':
				inString = false
			}
			continue
		}
		switch character {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, character)
		case '}', ']':
			if len(stack) == 0 || (character == '}' && stack[len(stack)-1] != '{') || (character == ']' && stack[len(stack)-1] != '[') {
				start = -1
				stack = []byte{}
				continue
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				var parsed any
				if err := json.Unmarshal([]byte(text[start:index+1]), &parsed); err == nil {
					values = append(values, parsed)
				}
				start = -1
			}
		}
	}
	return values
}

func slurpAdd(pages []any) []map[string]any {
	out := []map[string]any{}
	for _, page := range pages {
		items, ok := page.([]any)
		if !ok {
			continue
		}
		for _, item := range items {
			out = append(out, asRecord(item))
		}
	}
	return out
}

func asRecord(value any) map[string]any {
	record, ok := value.(map[string]any)
	if !ok || record == nil {
		return map[string]any{}
	}
	return record
}

func asArray(value any) []any {
	array, ok := value.([]any)
	if !ok || array == nil {
		return []any{}
	}
	return array
}

func asString(value any) string {
	stringValue, ok := value.(string)
	if !ok {
		return ""
	}
	return stringValue
}

func gqlLogin(author any) string {
	record, ok := author.(map[string]any)
	if !ok || record == nil {
		return ""
	}
	login := asString(record["login"])
	if record["__typename"] == "Bot" {
		return login + "[bot]"
	}
	return login
}

func hasNextPage(value any) bool {
	switch node := value.(type) {
	case []any:
		return slices.ContainsFunc(node, hasNextPage)
	case map[string]any:
		if node["hasNextPage"] == true {
			return true
		}
		for _, child := range node {
			if hasNextPage(child) {
				return true
			}
		}
	}
	return false
}

func nested(value any, keys ...string) any {
	current := value
	for _, key := range keys {
		record, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = record[key]
	}
	return current
}

func objectAt(value any, keys ...string) map[string]any {
	return asRecord(nested(value, keys...))
}
func parseRateLimit(raw string) (RateLimit, error) {
	var rateLimit RateLimit
	if err := json.Unmarshal([]byte(raw), &rateLimit); err != nil {
		return RateLimit{}, fmt.Errorf("decoding rate-limit response: %w", err)
	}
	return rateLimit, nil
}
