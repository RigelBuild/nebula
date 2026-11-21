package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// EnsureMacBinary installs the content-addressed macOS executable when needed.
// The returned path is relative to the remote user's home directory.
func EnsureMacBinary(ctx context.Context, r Remote, sshPath, target, localBin string) (string, error) {
	file, err := os.Open(localBin) //nolint:gosec // The operator selects the binary.
	if err != nil {
		return "", fmt.Errorf("open mac binary: %w", err)
	}
	hash := sha256.New()
	_, readErr := io.Copy(hash, file)
	closeErr := file.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", fmt.Errorf("hash mac binary: %w", err)
	}
	digest := hex.EncodeToString(hash.Sum(nil))
	remotePath := ".local/state/nemo-move/bin/nemo-move-" + digest[:12]
	remoteHash, err := r.Hash(ctx, sshPath, target, remotePath)
	if err == nil && remoteHash == digest {
		return remotePath, nil
	}
	if err := r.Upload(ctx, sshPath, target, localBin, remotePath, 0o700); err != nil {
		return "", fmt.Errorf("install mac binary: %w", err)
	}
	remoteHash, err = r.Hash(ctx, sshPath, target, remotePath)
	if err != nil {
		return "", fmt.Errorf("verify uploaded mac binary: %w", err)
	}
	if remoteHash != digest {
		return "", errors.New("uploaded mac binary hash mismatch")
	}
	return remotePath, nil
}
