// Command cotal-sys-creds-check logs days until the Cotal $SYS eviction creds expire.
// The CotalSysCredsExpiring alert reads the line it logs.
package main

import (
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

//go:embed contract.json
var contractJSON []byte

// contract is the log-line contract. The Grafana alert test reads the same file.
type contract struct {
	LinePrefix string `json:"linePrefix"`
	WarnDays   int    `json:"warnDays"`
}

// credsFiles are read in this order; a tie on expiry goes to the earlier name.
var credsFiles = [...]string{"membership-observer.creds", "connection-evictor.creds"}

const secondsPerDay = 86_400

var jwtBlock = regexp.MustCompile(`-----BEGIN NATS USER JWT-----\s+(\S+)\s+------END NATS USER JWT------`)

// creds is one creds file's name and contents.
type creds struct {
	name string
	text string
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, time.Now))
}

// run logs one check line for the space material dir in args[0].
// It returns 0 on success, 1 on a read or parse failure, and 2 on a missing argument.
func run(args []string, stdout, stderr io.Writer, now func() time.Time) int {
	// The stdlib logger writes the raw <3> journald priority prefix.
	logger := log.New(stderr, "", 0)
	c, err := loadContract()
	if err != nil {
		logger.Printf("<3>cotal-sys-creds: %v", err)
		return 1
	}
	if len(args) == 0 || args[0] == "" {
		logger.Print("<3>usage: cotal-sys-creds-check <space material dir>")
		return 2
	}
	files, err := readCreds(args[0])
	if err != nil {
		logger.Printf("<3>cotal-sys-creds: %v", err)
		return 1
	}
	line, warn, err := report(c, files, now().Unix())
	if err != nil {
		logger.Printf("<3>cotal-sys-creds: %v", err)
		return 1
	}
	// journald reads the <4> prefix as WARN priority.
	if warn {
		line = "<4>" + line
	}
	if _, err := fmt.Fprintln(stdout, line); err != nil {
		logger.Printf("<3>cotal-sys-creds: writing check line: %v", err)
		return 1
	}
	return 0
}

func loadContract() (contract, error) {
	var c contract
	if err := json.Unmarshal(contractJSON, &c); err != nil {
		return contract{}, fmt.Errorf("decoding embedded contract: %w", err)
	}
	return c, nil
}

// readCreds reads both creds files under dir, in credsFiles order.
func readCreds(dir string) ([]creds, error) {
	files := make([]creds, 0, len(credsFiles))
	for _, name := range credsFiles {
		data, err := os.ReadFile(filepath.Join(dir, name)) //nolint:gosec // G304: the operator passes the space material dir.
		if err != nil {
			return nil, err
		}
		files = append(files, creds{name: name, text: string(data)})
	}
	return files, nil
}

// credsExpiry returns the exp claim (epoch seconds) of the JWT in a NATS creds file.
func credsExpiry(text string) (float64, error) {
	match := jwtBlock.FindStringSubmatch(text)
	if match == nil {
		return 0, errors.New("no NATS user JWT block")
	}
	segments := strings.Split(match[1], ".")
	if len(segments) < 2 || segments[1] == "" {
		return 0, errors.New("JWT has no payload segment")
	}
	// Raw base64url, as NATS JWTs use; Bun was lenient on + and / and padding, this is strict.
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(segments[1], "="))
	if err != nil {
		return 0, fmt.Errorf("decoding JWT payload: %w", err)
	}
	var claims any
	if err := json.Unmarshal(raw, &claims); err != nil {
		return 0, fmt.Errorf("parsing JWT claims: %w", err)
	}
	object, _ := claims.(map[string]any)
	exp, ok := object["exp"].(float64)
	if !ok {
		return 0, errors.New("JWT has no numeric exp claim")
	}
	return exp, nil
}

// report returns the logfmt line for the creds file that expires first, and whether it warns.
func report(c contract, files []creds, nowSec int64) (string, bool, error) {
	if len(files) == 0 {
		return "", false, errors.New("no creds files given")
	}
	first := 0
	var firstExp float64
	for i, f := range files {
		exp, err := credsExpiry(f.text)
		if err != nil {
			return "", false, err
		}
		if i == 0 || exp < firstExp {
			first, firstExp = i, exp
		}
	}
	daysLeft := int64(math.Floor((firstExp - float64(nowSec)) / secondsPerDay))
	expires := time.UnixMilli(int64(firstExp * 1000)).UTC().Format("2006-01-02T15:04:05.000Z07:00")
	line := fmt.Sprintf("%s days_left=%d file=%s expires=%s", c.LinePrefix, daysLeft, files[first].name, expires)
	return line, daysLeft < int64(c.WarnDays), nil
}
