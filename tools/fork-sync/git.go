package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

var patchBlobIDs = regexp.MustCompile(`^index ([0-9a-f]+)\.\.([0-9a-f]+)`)

func git(ctx context.Context, directory string, input []byte, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "git", args...) //nolint:gosec // Git is fixed; arguments are passed without a shell.
	command.Dir = directory
	command.Stdin = bytes.NewReader(input)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err == nil {
		return output, nil
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return output, fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return output, fmt.Errorf("run git %s: %w", strings.Join(args, " "), err)
}

func gitExitCode(err error) int {
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

// validateRepo accepts only the record repo forms git fetches over the network: https://, ssh://, and the
// scp-style git@host:path. Any other value, such as one starting with "-", would be read by git as an option.
func validateRepo(repo string) error {
	if strings.HasPrefix(repo, "https://") || strings.HasPrefix(repo, "ssh://") || strings.HasPrefix(repo, "git@") {
		return nil
	}
	return fmt.Errorf("refused upstream repo %q: want https://, ssh:// or git@host:path", repo)
}

// validateOutbox refuses an --outbox value that git would read as an option.
func validateOutbox(remote string) error {
	if strings.HasPrefix(remote, "-") {
		return fmt.Errorf("refused outbox %q: must not start with -", remote)
	}
	return nil
}

func ensureClean(ctx context.Context, root string) error {
	output, err := git(ctx, root, nil, "status", "--porcelain")
	if err != nil {
		return err
	}
	if len(output) != 0 {
		return errors.New("worktree must be clean")
	}
	return nil
}

func commitIdentityEnv() []string {
	return append(os.Environ(), "GIT_AUTHOR_NAME=mintaka", "GIT_AUTHOR_EMAIL=mintaka@rigel.build", "GIT_COMMITTER_NAME=mintaka", "GIT_COMMITTER_EMAIL=mintaka@rigel.build")
}

func transferPatchBlobs(ctx context.Context, source, destination string, patch []byte) error {
	blobs := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(patch))
	for scanner.Scan() {
		match := patchBlobIDs.FindStringSubmatch(scanner.Text())
		if len(match) != 3 {
			continue
		}
		for _, blob := range match[1:] {
			if strings.Trim(blob, "0") != "" {
				blobs[blob] = struct{}{}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan diff blob references: %w", err)
	}
	for blob := range blobs {
		content, err := git(ctx, source, nil, "cat-file", "blob", blob)
		if err != nil {
			return fmt.Errorf("read diff blob %s: %w", blob, err)
		}
		if _, err := git(ctx, destination, content, "hash-object", "-w", "--stdin"); err != nil {
			return fmt.Errorf("write diff blob %s: %w", blob, err)
		}
	}
	return nil
}

func warnGitlink(path string) {
	fmt.Fprintf(os.Stderr, "warning: skipping gitlink %s\n", path)
}
