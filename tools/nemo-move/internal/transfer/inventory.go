package transfer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const maxInventoryBytes = 30 << 30

// InventoryRow is one home copy root in the remote inventory.
type InventoryRow struct {
	Row struct {
		Path string `json:"path"`
		Kind string `json:"kind"`
		Dest string `json:"dest"`
	} `json:"row"`
	Bytes    int64  `json:"bytes"`
	Readable bool   `json:"readable"`
	Error    string `json:"error,omitempty"`
}

// Inventory checks the durable copy roots using the macOS executable.
func Inventory(ctx context.Context, target, sshPath, binary string, remote Remote) ([]InventoryRow, error) {
	if _, err := splitTarget(target); err != nil {
		return nil, err
	}
	if remote == nil {
		return nil, errors.New("inventory remote is nil")
	}
	path, err := EnsureMacBinary(ctx, remote, sshPath, target, binary)
	if err != nil {
		return nil, err
	}
	output, remoteErr := runRemote(ctx, target, sshPath, remote, "\"$HOME/\""+ShellQuote(path)+" mac-side inventory")
	var report struct {
		Rows []InventoryRow `json:"rows"`
	}
	if err := json.Unmarshal([]byte(output), &report); err != nil {
		return nil, errors.Join(fmt.Errorf("decode durable-state inventory: %w", err), remoteErr)
	}
	if remoteErr != nil {
		return report.Rows, fmt.Errorf("read durable-state inventory: %w", remoteErr)
	}
	return report.Rows, nil
}

func splitTarget(target string) (string, error) {
	user, _, err := SplitSSHTarget(target)
	return user, err
}

// SplitSSHTarget validates a user@host SSH destination.
func SplitSSHTarget(target string) (string, string, error) {
	user, host, ok := strings.Cut(target, "@")
	if !ok || !safeTargetPart(user) || !safeTargetPart(host) || strings.Contains(host, "@") {
		return "", "", errors.New("--mac must be a user@host SSH target using letters, digits, dots, underscores, or hyphens and not starting with a hyphen")
	}
	return user, host, nil
}

func safeTargetPart(value string) bool {
	if value == "" || value[0] == '-' {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '.' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func runRemote(ctx context.Context, target, sshPath string, remote Remote, command string) (string, error) {
	if !strings.HasPrefix(sshPath, "/") {
		return "", errors.New("SSH executable must be an absolute path")
	}
	return remote.Run(ctx, sshPath, "-o", "BatchMode=yes", target, command)
}

// ShellQuote quotes one argument for a POSIX shell.
func ShellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}
