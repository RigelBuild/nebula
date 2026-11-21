// SPDX-License-Identifier: MIT OR Apache-2.0

package githubapptoken

import (
	"errors"
	"fmt"
	"os"
)

// KeyFile reads the current contents of a private-key file on each call.
type KeyFile struct {
	path string
}

// NewKeyFile returns a reader for the private-key file at path.
func NewKeyFile(path string) *KeyFile {
	return &KeyFile{path: path}
}

// Read returns the current private-key file contents without retaining them.
func (k *KeyFile) Read() ([]byte, error) {
	if k == nil || k.path == "" {
		return nil, errors.New("private key path is required")
	}
	data, err := os.ReadFile(k.path)
	if err != nil {
		return nil, fmt.Errorf("read private key file: %w", err)
	}
	return data, nil
}
