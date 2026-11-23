package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec" //nolint:depguard // coverage lists restic snapshots; stderr is discarded and no secret is in argv.
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/coverage"
	"github.com/RigelBuild/nebula/tools/transcript-archive/internal/resticenv"
)

type historyPass struct {
	ID    string `json:"passId"`
	Event string `json:"event"`
}

type resticSnapshot struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Tags    []string  `json:"tags"`
	PassID  string    `json:"-"`
	Outcome string    `json:"-"`
}

func resticOutput(opts coverageOptions, subcommand string, args ...string) ([]byte, error) {
	cmd, err := resticCommand(opts, subcommand, args...)
	if err != nil {
		return nil, err
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("restic %s: %w", subcommand, err)
	}
	return output, nil
}

// resticCommand builds a restic child with a filtered environment and the password file by base name.
func resticCommand(opts coverageOptions, subcommand string, args ...string) (*exec.Cmd, error) {
	repo := opts.repo
	var err error
	if localPath, ok := strings.CutPrefix(repo, "local:"); ok {
		repo, err = filepath.Abs(localPath)
		if err == nil {
			repo = "local:" + repo
		}
	} else if !resticenv.IsBackend(repo) {
		repo, err = filepath.Abs(repo)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve restic repository: %w", err)
	}
	arguments := make([]string, 0, 7+len(args))
	arguments = append(arguments, "--repo", repo, "--password-file", filepath.Base(opts.passwordFile))
	// State-dir coverage caches tree blobs in the state dir, so each batch's tree walk reads S3 once.
	if opts.cacheDir != "" {
		arguments = append(arguments, "--cache-dir", opts.cacheDir, subcommand)
	} else {
		arguments = append(arguments, "--no-cache", subcommand)
	}
	arguments = append(arguments, args...)
	binary := opts.restic
	if binary == "" {
		binary = "restic"
	}
	cmd := exec.Command(binary, arguments...) //nolint:gosec // Arguments are separate and supplied by the operator.
	cmd.Dir = filepath.Dir(opts.passwordFile)
	cmd.Env = resticenv.Filter("")
	cmd.Stderr = io.Discard
	return cmd, nil
}

func resticListing(opts coverageOptions, snapshot string, digests map[string]coverage.FileDigest) (coverage.SnapshotListing, error) {
	output, err := resticOutput(opts, "ls", "--json", "--no-lock", snapshot)
	if err != nil {
		return coverage.SnapshotListing{}, err
	}
	listing := coverage.SnapshotListing{SnapshotID: snapshot, PathsOnly: digests == nil, Files: map[string][32]byte{}}
	decoder := json.NewDecoder(bytes.NewReader(output))
	missing := 0
	for {
		var node struct {
			StructType string `json:"struct_type"`
			Type       string `json:"type"`
			Path       string `json:"path"`
		}
		if err := decoder.Decode(&node); err != nil {
			if err == io.EOF {
				break
			}
			return coverage.SnapshotListing{}, fmt.Errorf("decode restic listing: %w", err)
		}
		if node.StructType != "node" || node.Type != "file" {
			continue
		}
		if digests == nil {
			listing.Files[node.Path] = [32]byte{}
			continue
		}
		digest, ok := digests[node.Path]
		if !ok {
			missing++
			continue
		}
		listing.Files[node.Path] = digest.SHA256
	}
	if missing != 0 {
		return coverage.SnapshotListing{}, fmt.Errorf("missing digests for %d snapshot path(s)", missing)
	}
	return listing, nil
}

func readHistoryPasses(path string) (passes []historyPass, err error) {
	file, err := os.Open(path) //nolint:gosec // Operator-supplied journal path.
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	passes = []historyPass{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var pass historyPass
		if err := json.Unmarshal(scanner.Bytes(), &pass); err != nil {
			return nil, fmt.Errorf("decode journal: %w", err)
		}
		switch pass.Event {
		case "pass-acked", "pass-incomplete", "pass-stored":
			if pass.ID == "" {
				return nil, fmt.Errorf("journal %s missing pass ID", pass.Event)
			}
			passes = append(passes, pass)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read journal: %w", err)
	}
	return passes, nil
}

func resticHistory(opts coverageOptions) ([]coverage.SnapshotListing, error) {
	passes := make(map[string]string)
	for _, journal := range opts.journals {
		items, err := readHistoryPasses(journal)
		if err != nil {
			return nil, err
		}
		for _, pass := range items {
			passes[pass.ID] = pass.Event
		}
	}
	output, err := resticOutput(opts, "snapshots", "--json", "--no-lock", "--host", opts.scope)
	if err != nil {
		return nil, err
	}
	var found []resticSnapshot
	if err := json.Unmarshal(output, &found); err != nil {
		return nil, fmt.Errorf("decode restic snapshots: %w", err)
	}
	snapshots := make([]resticSnapshot, 0, len(found))
	for _, snapshot := range found {
		for _, tag := range snapshot.Tags {
			id, ok := strings.CutPrefix(tag, "pass-")
			if outcome, exists := passes[id]; ok && exists {
				snapshot.Outcome, snapshot.PassID = outcome, id
				snapshots = append(snapshots, snapshot)
				break
			}
		}
	}
	sort.SliceStable(snapshots, func(i, j int) bool { return snapshots[i].Time.After(snapshots[j].Time) })
	history := make([]coverage.SnapshotListing, 0, len(snapshots))
	for _, snapshot := range snapshots {
		listing, err := resticListing(opts, snapshot.ID, nil)
		if err != nil {
			return nil, err
		}
		listing.Outcome = snapshot.Outcome
		listing.PassID = snapshot.PassID
		history = append(history, listing)
	}
	return history, nil
}
