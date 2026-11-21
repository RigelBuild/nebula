package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	kindMessage    = "message"
	kindCompaction = "compaction"
	kindModelUsage = "model_usage"
)

// Row mirrors the stable fields that the wave-ledger extension writes.
type Row struct {
	V                   int       `json:"v"`
	Kind                string    `json:"kind"`
	TS                  time.Time `json:"ts"`
	RowKey              string    `json:"row_key"`
	SessionID           string    `json:"session_id"`
	Seat                *string   `json:"seat"`
	Lane                *string   `json:"lane"`
	Agent               *string   `json:"agent"`
	RoleTier            *string   `json:"role_tier"`
	Profile             *string   `json:"profile"`
	Issue               *string   `json:"issue"`
	IssueSource         *string   `json:"issue_source"`
	PR                  *string   `json:"pr"`
	Bookmark            *string   `json:"bookmark"`
	Experiment          *string   `json:"experiment"`
	Provider            *string   `json:"provider"`
	Model               *string   `json:"model"`
	ThinkingLevel       *string   `json:"thinking_level"`
	CorpusRev           *string   `json:"corpus_rev"`
	CorpusDigest        *string   `json:"corpus_digest"`
	PromptDigest        *string   `json:"prompt_digest"`
	PromptArchiveDigest *string   `json:"prompt_archive_digest"`
	Input               *int64    `json:"input"`
	Output              *int64    `json:"output"`
	CacheRead           *int64    `json:"cache_read"`
	CacheWrite          *int64    `json:"cache_write"`
	Reasoning           *int64    `json:"reasoning"`
	TokensBefore        *int64    `json:"tokens_before"`
	CostUSD             *float64  `json:"cost_usd"`
	PriceStatus         *string   `json:"price_status"`
	StopReason          *string   `json:"stop_reason"`
	ErrorClass          *string   `json:"error_class"`
	FindingsHigh        *int      `json:"findings_high"`
	FindingsMedium      *int      `json:"findings_medium"`
	FindingsLow         *int      `json:"findings_low"`
	ReviewedSHA         *string   `json:"reviewed_sha"`
}

// ReadRows reads UTC-dated files in order so the first copy of a row key wins.
func ReadRows(dir string, from, to time.Time) (rows []Row, skipped int, err error) {
	if to.Before(from) {
		return nil, 0, errors.New("end date precedes start date")
	}
	rows = make([]Row, 0)
	seen := make(map[string]struct{})
	start := utcDay(from)
	end := utcDay(to)
	for day := start; !day.After(end); day = day.AddDate(0, 0, 1) {
		path := filepath.Join(dir, day.Format("2006-01-02")+".jsonl")
		file, openErr := os.Open(path) //nolint:gosec // operator-supplied path
		if openErr != nil {
			if errors.Is(openErr, os.ErrNotExist) {
				continue
			}
			return nil, skipped, errors.New("opening ledger file")
		}
		fileRows, fileSkipped, readErr := readRowsFile(file, seen)
		closeErr := file.Close()
		skipped += fileSkipped
		if readErr != nil {
			return nil, skipped, readErr
		}
		if closeErr != nil {
			return nil, skipped, errors.New("closing ledger file")
		}
		rows = append(rows, fileRows...)
	}
	return rows, skipped, nil
}

func utcDay(value time.Time) time.Time {
	utc := value.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC)
}

func readRowsFile(file *os.File, seen map[string]struct{}) ([]Row, int, error) {
	rows := make([]Row, 0)
	skipped := 0
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var row Row
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return nil, skipped, errors.New("decoding ledger row")
		}
		if row.V != 1 || !knownKind(row.Kind) {
			skipped++
			continue
		}
		if _, ok := seen[row.RowKey]; ok {
			continue
		}
		seen[row.RowKey] = struct{}{}
		rows = append(rows, row)
	}
	if err := scanner.Err(); err != nil {
		return nil, skipped, fmt.Errorf("reading ledger rows: %w", err)
	}
	return rows, skipped, nil
}

func knownKind(kind string) bool {
	switch kind {
	case kindMessage, kindModelUsage, kindCompaction, "link", "review":
		return true
	default:
		return false
	}
}
