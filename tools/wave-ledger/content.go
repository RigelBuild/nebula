package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/klauspost/compress/zstd"
)

type ContentRow struct {
	V         int               `json:"v"`
	TS        time.Time         `json:"ts"`
	SessionID string            `json:"session_id"`
	RowKey    string            `json:"row_key"`
	Seq       int               `json:"seq"`
	Role      string            `json:"role"`
	ToolName  *string           `json:"tool_name"`
	Hash      string            `json:"hash"`
	Parts     []json.RawMessage `json:"parts"`
}

var (
	errBadContentRow   = errors.New("invalid content row")
	maxContentLineSize = 64 * 1024 * 1024
)

// ReadContent streams the content files in UTC date order.
func ReadContent(dir string, from, to time.Time, fn func(ContentRow) error) (int, error) {
	if to.Before(from) {
		return 0, errors.New("end date precedes start date")
	}
	bad := 0
	for day := utcDay(from); !day.After(utcDay(to)); day = day.AddDate(0, 0, 1) {
		path := filepath.Join(dir, "content", day.Format("2006-01-02")+".jsonl.zst")
		file, err := os.Open(path) //nolint:gosec // operator-supplied directory
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return bad, errors.New("opening content file")
		}
		broken, readErr := readContentFile(file, fn)
		closeErr := file.Close()
		if broken {
			bad++
		}
		if readErr != nil {
			return bad, readErr
		}
		if closeErr != nil {
			return bad, errors.New("closing content file")
		}
	}
	return bad, nil
}

func readContentFile(file io.Reader, fn func(ContentRow) error) (bool, error) {
	decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return false, fmt.Errorf("opening content decoder: %w", err)
	}
	defer decoder.Close()
	scanner := bufio.NewScanner(decoder)
	initialBufferSize := min(64*1024, maxContentLineSize)
	scanner.Buffer(make([]byte, initialBufferSize), maxContentLineSize)
	for scanner.Scan() {
		var row ContentRow
		if err := json.Unmarshal(scanner.Bytes(), &row); err != nil {
			return true, nil
		}
		if row.V == 1 {
			if err := fn(row); err != nil {
				if errors.Is(err, errBadContentRow) {
					return true, nil
				}
				return false, err
			}
		}
	}
	if scanner.Err() != nil {
		return true, nil
	}
	return false, nil
}
