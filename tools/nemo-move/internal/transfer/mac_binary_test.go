package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type binaryRemote struct {
	fakeRemote
	hash      string
	hashErr   error
	uploadErr error
	uploaded  bool
	path      string
	mode      os.FileMode
	wantHash  string
}

func (r *binaryRemote) Hash(_ context.Context, _, _, path string) (string, error) {
	r.path = path
	return r.hash, r.hashErr
}

func (r *binaryRemote) Upload(_ context.Context, _, _, _, path string, mode os.FileMode) error {
	r.uploaded = true
	r.path = path
	r.mode = mode
	if r.uploadErr == nil {
		r.hash = r.wantHash
		r.hashErr = nil
	}
	return r.uploadErr
}

func TestEnsureMacBinary(t *testing.T) {
	content := []byte("mac binary")
	hash := sha256.Sum256(content)
	digest := hex.EncodeToString(hash[:])
	for _, tc := range []struct {
		name       string
		hash       string
		hashErr    error
		uploadErr  error
		wantUpload bool
		wantErr    bool
	}{
		{name: "matching hash skips upload", hash: digest},
		{name: "missing binary uploads", hashErr: errors.New("missing"), wantUpload: true},
		{name: "upload failure returns error", hash: "different", uploadErr: errors.New("upload failed"), wantUpload: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := filepath.Join(t.TempDir(), "nemo-move")
			if err := os.WriteFile(bin, content, 0o600); err != nil {
				t.Fatal(err)
			}
			remote := &binaryRemote{hash: tc.hash, hashErr: tc.hashErr, uploadErr: tc.uploadErr, wantHash: digest}
			path, err := EnsureMacBinary(t.Context(), remote, "/ssh", "example@192.0.2.10", bin)
			if (err != nil) != tc.wantErr {
				t.Fatalf("EnsureMacBinary error = %v", err)
			}
			want := ".local/state/nemo-move/bin/nemo-move-" + digest[:12]
			if remote.path != want || (!tc.wantErr && path != want) {
				t.Fatalf("paths = %q, %q; want %q", remote.path, path, want)
			}
			if remote.uploaded != tc.wantUpload || (remote.uploaded && remote.mode != 0o700) {
				t.Fatalf("upload = %v, mode = %v", remote.uploaded, remote.mode)
			}
		})
	}
}
