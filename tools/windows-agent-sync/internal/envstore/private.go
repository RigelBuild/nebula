package envstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// privateSDDL is a protected DACL that grants full access only to the object
// owner (OWNER RIGHTS) and SYSTEM, inherited by child files and directories.
const privateSDDL = "D:P(A;OICI;FA;;;OW)(A;OICI;FA;;;SY)"

// privateNameAttempts bounds retries when a random temp name already exists.
const privateNameAttempts = 100

var errPrivateNameExhausted = errors.New("could not find an unused private file name")

// privateTempName returns prefix + random + suffix for a pattern containing one "*".
func privateTempName(pattern string) (string, error) {
	prefix, suffix, _ := strings.Cut(pattern, "*")
	var random [8]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]) + suffix, nil
}
