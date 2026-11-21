package fence

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec" //nolint:depguard // Fence verifies the installed OMP version before taking its locks.
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// OMPVersion is the `omp --version` string the fence accepts. ci/omp-pin.json
	// records the same value and the rev both flake locks pin.
	OMPVersion       = "omp/18.22.2-rigel.1"
	lockPollInterval = 200 * time.Millisecond
)

// Options configures the gc lock fence.
type Options struct {
	AgentDirs []string
	Wait      time.Duration
}

// ExitError carries a process exit code for the CLI integration.
type ExitError struct {
	Code    int
	Message string
}

func (e *ExitError) Error() string { return e.Message }

// ExitCode returns the process exit code associated with the failure.
func (e *ExitError) ExitCode() int { return e.Code }

// Run verifies OMP, locks every requested agent directory, and holds them until ctx ends.
// It returns nil when ctx ends while every lock is held, and an error when ctx
// ends first. Its locks are released before it returns.
func Run(ctx context.Context, options Options) (runErr error) {
	if options.Wait < 0 {
		return errors.New("wait duration must not be negative")
	}
	if err := checkOMPVersion(ctx); err != nil {
		return err
	}

	agentDirs := cleanAgentDirs(options.AgentDirs)
	if len(agentDirs) == 0 {
		var err error
		agentDirs, err = discoverAgentDirs()
		if err != nil {
			return err
		}
	}
	agentDirs, err := uniqueAgentDirs(agentDirs)
	if err != nil {
		return err
	}

	deadline := time.Now().Add(options.Wait)
	locks := make([]*heldLock, 0, len(agentDirs))
	defer func() {
		for _, lock := range slices.Backward(locks) {
			runErr = errors.Join(runErr, lock.release())
		}
	}()

	for _, agentDir := range agentDirs {
		lock, err := acquire(ctx, agentDir, deadline)
		if err != nil {
			return err
		}
		locks = append(locks, lock)
	}

	<-ctx.Done()
	return nil
}

func checkOMPVersion(ctx context.Context) error {
	output, err := exec.CommandContext(ctx, "omp", "--version").Output()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("check omp version: %w", ctxErr)
	}
	if err != nil {
		return &ExitError{Code: 3, Message: fmt.Sprintf("run omp --version: %v", err)}
	}
	if strings.TrimSpace(string(output)) != OMPVersion {
		return &ExitError{Code: 3, Message: "unsupported omp version"}
	}
	return nil
}

func cleanAgentDirs(agentDirs []string) []string {
	cleaned := make([]string, 0, len(agentDirs))
	seen := make(map[string]struct{}, len(agentDirs))
	for _, agentDir := range agentDirs {
		if agentDir == "" {
			continue
		}
		path := filepath.Clean(agentDir)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		cleaned = append(cleaned, path)
	}
	return cleaned
}

func discoverAgentDirs() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find home directory: %w", err)
	}
	agentDirs := []string{filepath.Join(home, ".omp", "agent")}
	profilesDir := filepath.Join(home, ".omp", "profiles")
	profiles, err := os.ReadDir(profilesDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read OMP profiles: %w", err)
	}
	for _, profile := range profiles {
		profilePath := filepath.Join(profilesDir, profile.Name())
		profileInfo, err := os.Stat(profilePath)
		if err != nil {
			return nil, fmt.Errorf("stat OMP profile: %w", err)
		}
		if profileInfo.IsDir() {
			agentDirs = append(agentDirs, filepath.Join(profilePath, "agent"))
		}
	}
	return cleanAgentDirs(agentDirs), nil
}

// uniqueAgentDirs keeps the first entry of each directory that several entries
// name, such as a symlink and its target. Each directory is created first so
// os.SameFile has a stat result to compare. Locking one directory twice would
// make the fence wait on its own lock.
func uniqueAgentDirs(agentDirs []string) ([]string, error) {
	unique := make([]string, 0, len(agentDirs))
	seen := make([]os.FileInfo, 0, len(agentDirs))
	for _, agentDir := range agentDirs {
		if err := os.MkdirAll(agentDir, 0o750); err != nil {
			return nil, fmt.Errorf("create agent directory: %w", err)
		}
		info, err := os.Stat(agentDir)
		if err != nil {
			return nil, fmt.Errorf("stat agent directory: %w", err)
		}
		if slices.ContainsFunc(seen, func(previous os.FileInfo) bool { return os.SameFile(previous, info) }) {
			continue
		}
		seen = append(seen, info)
		unique = append(unique, agentDir)
	}
	return unique, nil
}
