package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"filippo.io/age"
)

// fileEscrower age-encrypts the scope password to one file and records the plaintext sha256 beside it.
type fileEscrower struct {
	out       string
	recipient age.Recipient
}

func newFileEscrower(out, recipient string) (*fileEscrower, error) {
	parsed, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("parse escrow recipient: %w", err)
	}
	return &fileEscrower{out: out, recipient: parsed}, nil
}

// Put writes the escrow once; an existing escrow is accepted only for the same password.
func (e *fileEscrower) Put(ctx context.Context, _ string, body []byte) ([32]byte, error) {
	if err := ctx.Err(); err != nil {
		return [32]byte{}, err
	}
	plain := sha256.Sum256(body)
	digest := []byte(hex.EncodeToString(plain[:]) + "\n")
	if err := installOnce(e.out+".sha256", digest); err != nil {
		return [32]byte{}, err
	}
	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, e.recipient)
	if err != nil {
		return [32]byte{}, fmt.Errorf("encrypt escrow: %w", err)
	}
	if _, err := writer.Write(body); err != nil {
		return [32]byte{}, fmt.Errorf("encrypt escrow: %w", err)
	}
	if err := writer.Close(); err != nil {
		return [32]byte{}, fmt.Errorf("encrypt escrow: %w", err)
	}
	if err := installFile(e.out, ciphertext.Bytes()); err != nil && !errors.Is(err, os.ErrExist) {
		return [32]byte{}, err
	}
	// The sidecar matched, so an existing escrow file holds this password; report its bytes.
	stored, err := os.ReadFile(e.out)
	if err != nil {
		return [32]byte{}, fmt.Errorf("read escrow: %w", err)
	}
	return sha256.Sum256(stored), nil
}

// installOnce writes data at path, or accepts an existing file with identical bytes.
func installOnce(path string, data []byte) error {
	err := installFile(path, data)
	if !errors.Is(err, os.ErrExist) {
		return err
	}
	existing, readErr := os.ReadFile(path) //nolint:gosec // Operator-supplied escrow path.
	if readErr != nil {
		return fmt.Errorf("read escrow digest: %w", readErr)
	}
	if !bytes.Equal(existing, data) {
		return errors.New("escrow-out already holds a different password; refusing to overwrite")
	}
	return nil
}

// installFile fsyncs data to a temp file and hard-links it into place, failing with os.ErrExist.
func installFile(path string, data []byte) (resultErr error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create escrow directory: %w", err)
	}
	temp, err := os.CreateTemp(dir, ".escrow-*")
	if err != nil {
		return fmt.Errorf("create escrow: %w", err)
	}
	defer func() {
		if err := os.Remove(temp.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
			resultErr = errors.Join(resultErr, fmt.Errorf("remove escrow temp: %w", err))
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return errors.Join(fmt.Errorf("write escrow: %w", err), temp.Close())
	}
	if err := temp.Sync(); err != nil {
		return errors.Join(fmt.Errorf("sync escrow: %w", err), temp.Close())
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close escrow: %w", err)
	}
	if err := os.Link(temp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return os.ErrExist
		}
		return fmt.Errorf("install escrow: %w", err)
	}
	directory, err := os.Open(dir) //nolint:gosec // Escrow directory for fsync.
	if err != nil {
		return fmt.Errorf("open escrow directory: %w", err)
	}
	return errors.Join(directory.Sync(), directory.Close())
}
