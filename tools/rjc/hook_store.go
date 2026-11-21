package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type HookResult string

const (
	hookPass    HookResult = "pass"
	hookFail    HookResult = "fail"
	hookPartial HookResult = "partial"
	hookSkipped HookResult = "skipped"
	hookMainRed HookResult = "main-red"
	hookFixup   HookResult = "fixup"
	hookAbsent  HookResult = "absent"
)

type SkippedTarget struct {
	Target string `json:"target"`
	Reason string `json:"reason"`
}

type RunRef struct{ ID, Digest string }

type HookRecord struct {
	Repo           string          `json:"repo"`
	Head           string          `json:"head"`
	Base           string          `json:"base"`
	HookConfigBlob string          `json:"hookConfigBlob"`
	Result         HookResult      `json:"result"`
	Fixup          string          `json:"fixup,omitempty"`
	Skip           string          `json:"skip,omitempty"`
	FailedRun      string          `json:"failedRun,omitempty"`
	CoveredBy      string          `json:"coveredBy,omitempty"`
	MainTip        string          `json:"mainTip,omitempty"`
	FailedTargets  []string        `json:"failedTargets,omitempty"`
	FailedGates    []string        `json:"failedGates,omitempty"`
	SkippedTargets []SkippedTarget `json:"skippedTargets,omitempty"`
	Run            string          `json:"run,omitempty"`
	Digest         string          `json:"digest,omitempty"`
	At             time.Time       `json:"at"`
}

func (rec HookRecord) Current(repo, head, base, blob string) bool {
	return rec.Repo == repo && rec.Head == head && rec.Base == base && rec.HookConfigBlob == blob
}

type localPrePush struct{ root string }
type hookFile struct {
	Latest  HookRecord   `json:"latest"`
	History []HookRecord `json:"history"`
}

var objectID = regexp.MustCompile(`^[0-9a-f]{40}$`)

func stateRepoPath(root, category, repo string) (string, error) {
	parts := strings.Split(repo, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." || parts[1] == "." || parts[1] == ".." || strings.ContainsAny(repo, `\`) {
		return "", fmt.Errorf("invalid repository %q", repo)
	}
	return filepath.Join(root, category, parts[0], parts[1]), nil
}

func (s localPrePush) Save(rec HookRecord) error {
	path, err := s.path(rec.Repo, rec.Head)
	if err != nil {
		return err
	}
	file, _, err := s.load(rec.Repo, rec.Head)
	if err != nil {
		return err
	}
	if rec.Result == hookPass {
		file.History = nil
	}
	if rec.Result == hookFail || rec.Result == hookPartial {
		file.History = append(file.History, rec)
	}
	file.Latest = rec
	data, err := json.Marshal(file)
	if err != nil {
		return fmt.Errorf("encode hook record: %w", err)
	}
	return atomicWrite(path, string(data))
}

func (s localPrePush) History(repo, head string) ([]HookRecord, error) {
	file, _, err := s.load(repo, head)
	return file.History, err
}

func (s localPrePush) Publish(_ context.Context, repo string, _ int, rec HookRecord) error {
	if repo != rec.Repo {
		return errors.New("hook record repository mismatch")
	}
	return nil
}

func (s localPrePush) Read(_ context.Context, repo string, _ int, head string) (HookRecord, bool, error) {
	file, ok, err := s.load(repo, head)
	return file.Latest, ok, err
}

func (s localPrePush) path(repo, head string) (string, error) {
	dir, err := stateRepoPath(s.root, "hooks", repo)
	if err != nil {
		return "", err
	}
	if !objectID.MatchString(head) {
		return "", fmt.Errorf("invalid hook head %q", head)
	}
	return filepath.Join(dir, head+".json"), nil
}

func (s localPrePush) load(repo, head string) (hookFile, bool, error) {
	path, err := s.path(repo, head)
	if err != nil {
		return hookFile{}, false, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // The repository and head are validated above.
	if errors.Is(err, os.ErrNotExist) {
		return hookFile{}, false, nil
	}
	if err != nil {
		return hookFile{}, false, fmt.Errorf("read hook record: %w", err)
	}
	var file hookFile
	if err := json.Unmarshal(data, &file); err != nil {
		return hookFile{}, false, fmt.Errorf("decode hook record: %w", err)
	}
	if file.Latest.Repo != repo || file.Latest.Head != head {
		return hookFile{}, false, nil
	}
	return file, true, nil
}
