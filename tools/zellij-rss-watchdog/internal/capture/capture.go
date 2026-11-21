package capture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/command"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/proc"
	"github.com/RigelBuild/nebula/tools/zellij-rss-watchdog/internal/seam"
)

const (
	maxMappingBytes = int64(512) << 20
	minMappingBytes = int64(64) << 20
	maxMappings     = 4
	MaxCaptures     = 3
	// MaxLogBytes caps the zellij log tail copied into one capture.
	MaxLogBytes     int64 = 20 << 20
	userTimeoutSecs       = 25
	// EnvCommand is the argv head that sets a child environment before exec.
	EnvCommand = "env"
)

// userTimeoutArgv bounds a user command; runuser execs it so the command's own timeout fires first.
var userTimeoutArgv = []string{"timeout", "--kill-after=2", "25"}

// RunAsUser runs argv as the configured server user under userTimeoutArgv. Its 124/137 exits read as timeouts.
func RunAsUser(ctx context.Context, io seam.IO, argv []string) (command.Result, error) {
	args := append([]string{"-u", io.Config().User, "--"}, userTimeoutArgv...)
	args = append(args, argv...)
	result, err := io.Run(ctx, "runuser", args, command.CommandTimeout)
	if err == nil {
		return result, nil
	}
	var cmdErr *command.Error
	if errors.As(err, &cmdErr) && isUserTimeoutStatus(cmdErr.Status) {
		return result, fmt.Errorf("command timed out after %ds as %s (%w)", userTimeoutSecs, io.Config().User, err)
	}
	return result, err
}

// isUserTimeoutStatus matches the exit codes and signal names that mean the user timeout fired.
func isUserTimeoutStatus(status string) bool {
	return status == "124" || status == "137" || status == "SIGKILL"
}

// PruneCaptures returns the directories beyond the newest MaxCaptures, to be removed.
func PruneCaptures(captureDirs []string) []string {
	ordered := slices.Clone(captureDirs)
	slices.SortFunc(ordered, func(left, right string) int {
		return strings.Compare(right, left)
	})
	if len(ordered) <= MaxCaptures {
		return nil
	}
	return ordered[MaxCaptures:]
}

// CaptureName names a capture directory by its UTC time, which sorts lexically.
func CaptureName(ms int64) string {
	return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z")
}

// Logfmt quotes a value as a JSON string so one log line stays parseable.
func Logfmt(value string) string {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	// Encoding a string cannot fail; the fallback keeps the line well-formed if it ever did.
	if err := encoder.Encode(value); err != nil {
		return strconv.Quote(value)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// captureStep runs one capture step and records its failure without stopping the others.
func captureStep(name string, errs *[]string, operation func() error) {
	if err := operation(); err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %s", name, err))
	}
}

type mappingRange struct {
	start string
	end   string
}

var (
	mappingHeader = regexp.MustCompile(`(?m)^[0-9a-f]+-[0-9a-f]+\s`)
	rwHeader      = regexp.MustCompile(`(?m)^([0-9a-f]+)-([0-9a-f]+)\s+rw[^\n]*$`)
	sizeLine      = regexp.MustCompile(`(?m)^Size:\s+(\d+)\s+kB$`)
)

// mappingRanges picks the four largest anonymous rw regions of at least 64 MiB, each capped at 512 MiB.
func mappingRanges(smaps string) []mappingRange {
	type candidate struct {
		start, end, size *big.Int
	}
	var candidates []candidate
	for _, chunk := range splitSmaps(smaps) {
		header := rwHeader.FindStringSubmatch(chunk)
		sizeMatch := sizeLine.FindStringSubmatch(chunk)
		if header == nil || sizeMatch == nil {
			continue
		}
		pathname := pathnameOf(chunk)
		if pathname != "" && pathname != "[anon]" && !strings.HasPrefix(pathname, "[anon:") && pathname != "[heap]" {
			continue
		}
		kilobytes, ok := new(big.Int).SetString(sizeMatch[1], 10)
		if !ok {
			continue
		}
		size := new(big.Int).Mul(kilobytes, big.NewInt(1024))
		if size.Cmp(big.NewInt(minMappingBytes)) < 0 {
			continue
		}
		start, _ := new(big.Int).SetString(header[1], 16)
		end, _ := new(big.Int).SetString(header[2], 16)
		candidates = append(candidates, candidate{start: start, end: end, size: size})
	}
	slices.SortStableFunc(candidates, func(left, right candidate) int {
		return right.size.Cmp(left.size)
	})
	if len(candidates) > maxMappings {
		candidates = candidates[:maxMappings]
	}
	ranges := make([]mappingRange, 0, len(candidates))
	for _, c := range candidates {
		limit := new(big.Int).Add(c.start, big.NewInt(maxMappingBytes))
		end := c.end
		if end.Cmp(limit) >= 0 {
			end = limit
		}
		ranges = append(ranges, mappingRange{start: c.start.Text(16), end: end.Text(16)})
	}
	return ranges
}

// splitSmaps splits smaps text into per-mapping chunks, each starting at a "<hex>-<hex>" header line.
func splitSmaps(smaps string) []string {
	starts := mappingHeader.FindAllStringIndex(smaps, -1)
	chunks := make([]string, 0, len(starts))
	for i, span := range starts {
		end := len(smaps)
		if i+1 < len(starts) {
			end = starts[i+1][0]
		}
		chunks = append(chunks, smaps[span[0]:end])
	}
	return chunks
}

// pathnameOf returns the mapping's pathname from its header line, from column 6 onward.
func pathnameOf(chunk string) string {
	firstLine, _, _ := strings.Cut(chunk, "\n")
	fields := strings.Fields(strings.TrimSpace(firstLine))
	if len(fields) <= 5 {
		return ""
	}
	return strings.Join(fields[5:], " ")
}

// parseSsPeers returns the server rows and the peer pids that share a local socket inode.
func parseSsPeers(sockets, serverPID string) (serverLines []string, peerPIDs []string) {
	lines := strings.Split(sockets, "\n")
	for _, line := range lines {
		if strings.Contains(line, "pid="+serverPID+",") {
			serverLines = append(serverLines, line)
		}
	}
	localInodes := map[string]bool{}
	for _, line := range serverLines {
		if addresses, ok := inodesFor(line); ok && proc.IsDigits(addresses.local) {
			localInodes[addresses.local] = true
		}
	}
	seen := map[string]bool{}
	for _, line := range lines {
		addresses, ok := inodesFor(line)
		if !ok || !localInodes[addresses.peer] {
			continue
		}
		for _, match := range pidPattern.FindAllStringSubmatch(line, -1) {
			pid := match[1]
			if pid != serverPID && !seen[pid] {
				seen[pid] = true
				peerPIDs = append(peerPIDs, pid)
			}
		}
	}
	return serverLines, peerPIDs
}

var pidPattern = regexp.MustCompile(`pid=(\d+),`)

type socketInodes struct {
	local string
	peer  string
}

// inodesFor reads the local and peer inode columns from one ss -xp row. Field order is fixed by ss.
func inodesFor(line string) (socketInodes, bool) {
	before, _, found := strings.Cut(line, "users:")
	if !found {
		return socketInodes{}, false
	}
	fields := strings.Fields(before)
	const localInodeIndex = 5
	const peerInodeIndex = 7
	if len(fields) <= peerInodeIndex {
		return socketInodes{}, false
	}
	return socketInodes{local: fields[localInodeIndex], peer: fields[peerInodeIndex]}, true
}

// verifyIdentity fails the step when the server identity moved since the sample.
func verifyIdentity(io seam.IO, identity, message string) error {
	if io.Identity() != identity {
		return errors.New(message)
	}
	return nil
}

// captureMaps dumps and reduces up to four large anonymous mappings, one at a time.
func captureMaps(ctx context.Context, io seam.IO, pid, directory, identity string, errs *[]string) error {
	if err := verifyIdentity(io, identity, "server identity changed before smaps read"); err != nil {
		return err
	}
	smaps, err := io.ReadText(io.Config().ProcRoot + "/" + pid + "/smaps")
	if err != nil {
		return err
	}
	for index, r := range mappingRanges(smaps) {
		if err := verifyIdentity(io, identity, "server identity changed before proc memory attach"); err != nil {
			return err
		}
		start, _ := new(big.Int).SetString(r.start, 16)
		end, _ := new(big.Int).SetString(r.end, 16)
		rawFile := filepath.Join(directory, fmt.Sprintf("mapping-%d.raw", index))
		if reduceErr := reduceMapping(ctx, io, pid, rawFile, directory, index, start, end, identity); reduceErr != nil {
			*errs = append(*errs, fmt.Sprintf("mapping-%d: %s", index, reduceErr))
		}
		if removeErr := io.Remove(rawFile); removeErr != nil {
			*errs = append(*errs, fmt.Sprintf("mapping-%d cleanup: %s", index, removeErr))
		}
	}
	return nil
}

// reduceMapping copies one range to rawFile and writes its reduced strings beside it.
func reduceMapping(ctx context.Context, io seam.IO, pid, rawFile, directory string, index int, start, end *big.Int, identity string) error {
	if err := io.CopyMemory(pid, start.Uint64(), end.Uint64(), rawFile, identity); err != nil {
		return err
	}
	result, err := io.Run(ctx, "bash", []string{
		"-c",
		`set -o pipefail; strings -n 8 "$1" | sort -T "$2" | uniq -c | sort -T "$2" -rn | { head -n 500; cat >/dev/null; }`,
		"watchdog",
		rawFile,
		directory,
	}, command.MappingTimeout)
	if err != nil {
		return err
	}
	return io.WriteText(filepath.Join(directory, fmt.Sprintf("mapping-%d.strings.txt", index)), result.Stdout)
}
