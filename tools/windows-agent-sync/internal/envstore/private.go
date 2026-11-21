package envstore

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strings"
)

// privateSDDL returns a descriptor owned by userSID with a protected DACL that
// grants full access only to userSID and SYSTEM, inherited by child files and
// directories. It names the user SID, not OWNER RIGHTS, because an elevated run
// makes the Administrators group the default owner.
func privateSDDL(userSID string) string {
	return "O:" + userSID + "D:P(A;OICI;FA;;;" + userSID + ")(A;OICI;FA;;;SY)"
}

// privateNameAttempts bounds retries when a random temp name already exists.
const privateNameAttempts = 100

var errPrivateNameExhausted = errors.New("could not find an unused private file name")

// readRandom fills temp-name randomness; tests replace it to force collisions.
var readRandom = rand.Read

// privateTempName returns prefix + random + suffix for a pattern containing one "*".
func privateTempName(pattern string) (string, error) {
	prefix, suffix, _ := strings.Cut(pattern, "*")
	var random [8]byte
	if _, err := readRandom(random[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(random[:]) + suffix, nil
}
