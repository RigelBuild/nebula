// SPDX-License-Identifier: MIT OR Apache-2.0

package relaunch

import (
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
)

const sessionHeaderLimit = 64 * 1024

type SessionHeader struct {
	ID            string `json:"id,omitempty"`
	Title         string `json:"title,omitempty"`
	CWD           string `json:"cwd,omitempty"`
	ParentSession string `json:"parentSession,omitempty"`
}

type SessionPick struct {
	ID      string
	File    string
	MtimeMS int64
}

type SessionResult struct {
	ID    string
	Stale bool
}

type WaveAgent struct {
	Name      string `json:"name"`
	SessionID string `json:"sessionId"`
	Stale     bool   `json:"stale,omitempty"`
}

type SessionDeps struct {
	FileSystem FileSystem
	ReadDir    func(path string) ([]string, error)
	Stat       func(path string) (int64, error)
	ReadHeader func(path string, maxBytes int) (any, error)
}

func readPrefix(fileSystem FileSystem, path string, maxBytes int) ([]byte, error) {
	if reader, ok := fileSystem.(interface {
		ReadPrefix(path string, maxBytes int) ([]byte, error)
	}); ok {
		return reader.ReadPrefix(path, maxBytes)
	}
	data, err := fileSystem.ReadFile(path)
	if err != nil || len(data) <= maxBytes {
		return data, err
	}
	return data[:maxBytes], nil
}

// ReadTail reads at most the last maxBytes of a file.
func ReadTail(fileSystem FileSystem, path string, maxBytes int) ([]byte, error) {
	if reader, ok := fileSystem.(TailReader); ok {
		return reader.ReadTail(path, maxBytes)
	}
	data, err := fileSystem.ReadFile(path)
	if err != nil || len(data) <= maxBytes {
		return data, err
	}
	return data[len(data)-maxBytes:], nil
}

// ReadSessionHeader reads a session file's header from a bounded prefix, never the whole file.
func ReadSessionHeader(fileSystem FileSystem, path string) (SessionHeader, bool) {
	return readHeader(fileSystem, path)
}

// ReadHeaderFromText reads session fields through the first session row.
func ReadHeaderFromText(text string) SessionHeader {
	var header SessionHeader
	for line := range strings.SplitSeq(text, "\n") {
		if line == "" {
			continue
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(line), &row); err != nil || row == nil {
			continue
		}
		updateSessionHeader(&header, row)
		if row["type"] == "session" {
			return header
		}
	}
	return header
}

func updateSessionHeader(header *SessionHeader, row map[string]any) {
	if row["type"] == "title" {
		if title, ok := row["title"].(string); ok {
			header.Title = title
		}
	}
	if id, ok := row["id"].(string); ok {
		header.ID = id
		if cwd, ok := row["cwd"].(string); ok {
			header.CWD = cwd
		}
		if title, ok := row["title"].(string); ok {
			header.Title = title
		}
		if parent, ok := row["parentSession"].(string); ok {
			header.ParentSession = parent
		}
	}
}

func readHeader(fileSystem FileSystem, path string) (SessionHeader, bool) {
	data, err := readPrefix(fileSystem, path, sessionHeaderLimit)
	if err != nil {
		return SessionHeader{}, false
	}
	return ReadHeaderFromText(string(data)), true
}

// SessionAttributable reports whether a session belongs to the given seat and cwd.
func SessionAttributable(fileSystem FileSystem, file, title, cwd string) bool {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	header, ok := readHeader(fileSystem, file)
	return ok && headerMatches(header, title, cwd)
}

// headerMatches reports whether a header is the seat's session: its title is the
// seat name exactly, and its cwd is cwd exactly.
func headerMatches(header SessionHeader, title, cwd string) bool {
	return header.ID != "" && header.Title == title && header.CWD == cwd
}

func headerValue(fileSystem FileSystem, path string, deps SessionDeps) (SessionHeader, bool) {
	if deps.ReadHeader == nil {
		return readHeader(fileSystem, path)
	}
	value, err := deps.ReadHeader(path, sessionHeaderLimit)
	if err != nil {
		return SessionHeader{}, false
	}
	return sessionHeaderFromAny(value)
}

func sessionHeaderFromAny(value any) (SessionHeader, bool) {
	row, ok := value.(map[string]any)
	if !ok {
		if header, ok := value.(SessionHeader); ok {
			return header, true
		}
		return SessionHeader{}, false
	}
	var header SessionHeader
	header.ID, _ = row["id"].(string)
	header.Title, _ = row["title"].(string)
	header.CWD, _ = row["cwd"].(string)
	header.ParentSession, _ = row["parentSession"].(string)
	return header, true
}

func readDirNames(fileSystem FileSystem, path string) ([]string, error) {
	entries, err := fileSystem.ReadDir(path)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names, nil
}

func jsonlFiles(directory string, fileSystem FileSystem, recursive bool) []string {
	names, err := readDirNames(fileSystem, directory)
	if err != nil {
		return []string{}
	}
	files := make([]string, 0)
	for _, name := range names {
		path := filepath.Join(directory, name)
		info, err := fileSystem.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() && recursive {
			files = append(files, jsonlFiles(path, fileSystem, true)...)
		} else if !info.IsDir() && strings.HasSuffix(name, ".jsonl") {
			files = append(files, path)
		}
	}
	return files
}

// TopLevelSessionFiles is TopLevelSessionFilesErr with errors ignored; any
// read or Stat error yields an empty list.
func TopLevelSessionFiles(directory string, fileSystem FileSystem) []string {
	files, _ := TopLevelSessionFilesErr(directory, fileSystem)
	return files
}

// TopLevelSessionFilesErr lists top-level session files and reports read and stat errors.
func TopLevelSessionFilesErr(directory string, fileSystem FileSystem) ([]string, error) {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	names, err := readDirNames(fileSystem, directory)
	if err != nil {
		return []string{}, err
	}
	files := make([]string, 0)
	for _, name := range names {
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		path := filepath.Join(directory, name)
		info, err := fileSystem.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			files = append(files, path)
		}
	}
	return files, nil
}

func SnapshotSessionFiles(directory string, fileSystem FileSystem) map[string]struct{} {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	files := jsonlFiles(directory, fileSystem, true)
	result := make(map[string]struct{}, len(files))
	for _, file := range files {
		result[file] = struct{}{}
	}
	return result
}

// SessionDirFor maps process cwd to its omp session directory.
func SessionDirFor(cwd, home, root string) string {
	relative := cwd
	if home != "" && (cwd == home || strings.HasPrefix(cwd, home+string(filepath.Separator))) {
		relative = cwd[len(home):]
	}
	return filepath.Join(root, strings.ReplaceAll(relative, string(filepath.Separator), "-"))
}

// NewestSession returns the newest session in dir whose header title is title and
// whose header cwd is cwd, skipping burned session ids. Ties keep the first file.
func NewestSession(dir, title, cwd string, burned map[string]struct{}, deps SessionDeps) (*SessionPick, error) {
	fileSystem := deps.FileSystem
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	names, readable := sessionDirNames(fileSystem, dir, deps)
	var err error
	if !readable {
		return nil, nil //nolint:nilnil // An unreadable directory intentionally means no session candidate, not a caller error.
	}
	var best *SessionPick
	for _, name := range names {
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		file := filepath.Join(dir, name)
		var mtimeMS int64
		if deps.Stat != nil {
			mtimeMS, err = deps.Stat(file)
		} else {
			var info fs.FileInfo
			info, err = fileSystem.Stat(file)
			if err == nil {
				mtimeMS = info.ModTime().UnixMilli()
			}
		}
		if err != nil {
			continue
		}
		header, ok := headerValue(fileSystem, file, deps)
		if !ok || !headerMatches(header, title, cwd) {
			continue
		}
		if _, isBurned := burned[header.ID]; isBurned {
			continue
		}
		if best == nil || mtimeMS > best.MtimeMS {
			best = &SessionPick{ID: header.ID, File: file, MtimeMS: mtimeMS}
		}
	}
	return best, nil
}

// NewestSeatSession returns the session a seat at cwd resumes. A continued seat
// session forks from a header that names the parent workspace root, so it is filed
// under the parent cwd's session directory, which is scanned too. A parent-cwd
// session is chosen only when a seat-cwd session exists and the parent session is
// strictly newer; an mtime tie keeps the seat-cwd session.
func NewestSeatSession(cwd, home, root, title string, burned map[string]struct{}, deps SessionDeps) (*SessionPick, error) {
	exact, err := NewestSession(SessionDirFor(cwd, home, root), title, cwd, burned, deps)
	if err != nil || exact == nil {
		return exact, err
	}
	parentCWD := filepath.Dir(cwd)
	parent, err := NewestSession(SessionDirFor(parentCWD, home, root), title, parentCWD, burned, deps)
	if err != nil {
		return nil, err
	}
	if parent != nil && parent.MtimeMS > exact.MtimeMS {
		return parent, nil
	}
	return exact, nil
}

// Unreadable directories have no session candidate, matching the session scan contract.
func sessionDirNames(fileSystem FileSystem, dir string, deps SessionDeps) ([]string, bool) {
	var names []string
	var err error
	if deps.ReadDir != nil {
		names, err = deps.ReadDir(dir)
	} else {
		names, err = readDirNames(fileSystem, dir)
	}
	return names, err == nil
}

// ResolveForkSession resolves explicit ids, open files, and fork-parent fallbacks.
func ResolveForkSession(openFiles, topLevelOpenFiles, files []string, sessionID, forkID string, fileSystem FileSystem, readHeader func(path string, maxBytes int) (any, error), mtime func(path string) (int64, error)) *SessionResult {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	headerFor := cachedSessionHeaders(fileSystem, readHeader)
	for _, file := range openFiles {
		if header, ok := headerFor(file); ok && sessionID != "" && header.ID == sessionID && header.ID != "" {
			return &SessionResult{ID: header.ID}
		}
	}
	if forkID != "" {
		if id := openForkSession(openFiles, topLevelOpenFiles, forkID, headerFor); id != "" {
			return &SessionResult{ID: id}
		}
	}
	if len(topLevelOpenFiles) == 1 {
		if header, ok := headerFor(topLevelOpenFiles[0]); ok && header.ID != "" {
			return &SessionResult{ID: header.ID}
		}
	}
	if forkID != "" {
		if newestID := newestForkSession(files, forkID, fileSystem, headerFor, mtime); newestID != "" {
			return &SessionResult{ID: newestID}
		}
		return &SessionResult{ID: forkID, Stale: true}
	}
	if sessionID != "" {
		return &SessionResult{ID: sessionID, Stale: true}
	}
	return nil
}

func openForkSession(openFiles, topLevelOpenFiles []string, forkID string, headerFor func(string) (SessionHeader, bool)) string {
	ordered := append([]string{}, topLevelOpenFiles...)
	for _, file := range openFiles {
		if !slices.Contains(topLevelOpenFiles, file) {
			ordered = append(ordered, file)
		}
	}
	for _, file := range ordered {
		if header, ok := headerFor(file); ok && header.ParentSession == forkID && header.ID != "" {
			return header.ID
		}
	}
	return ""
}

func readHeaderForPath(fileSystem FileSystem, path string) (SessionHeader, bool) {
	return readHeader(fileSystem, path)
}

func cachedSessionHeaders(fileSystem FileSystem, readHeader func(string, int) (any, error)) func(string) (SessionHeader, bool) {
	cache := make(map[string]SessionHeader)
	loaded := make(map[string]bool)
	return func(path string) (SessionHeader, bool) {
		if loaded[path] {
			header, ok := cache[path]
			return header, ok
		}
		loaded[path] = true
		header, ok := headerValue(fileSystem, path, SessionDeps{ReadHeader: readHeader})
		if ok {
			cache[path] = header
		}
		return header, ok
	}
}

func newestForkSession(
	files []string,
	forkID string,
	fileSystem FileSystem,
	headerFor func(string) (SessionHeader, bool),
	mtime func(string) (int64, error),
) string {
	var newestID string
	var newestMtime int64
	for _, file := range files {
		header, ok := headerFor(file)
		if !ok || header.ID == "" || header.ParentSession != forkID {
			continue
		}
		var modified int64
		var err error
		if mtime != nil {
			modified, err = mtime(file)
		} else {
			var info fs.FileInfo
			info, err = fileSystem.Stat(file)
			if err == nil {
				modified = info.ModTime().UnixMilli()
			}
		}
		if err != nil {
			continue
		}
		if newestID == "" || modified > newestMtime {
			newestID, newestMtime = header.ID, modified
		}
	}
	return newestID
}

func ReconcileSessions(before map[string]struct{}, after []string, eligibleNames map[string]struct{}, fileSystem FileSystem, readHeader func(path string) (any, error)) []WaveAgent {
	if fileSystem == nil {
		fileSystem = NodeFileSystem{}
	}
	reconciled := make([]WaveAgent, 0)
	for _, file := range after {
		if _, existed := before[file]; existed || !strings.HasSuffix(file, ".jsonl") {
			continue
		}
		var value any
		var err error
		if readHeader != nil {
			value, err = readHeader(file)
		} else {
			var header SessionHeader
			header, exists := readHeaderForPath(fileSystem, file)
			if exists {
				value = header
			} else {
				continue
			}
		}
		if err != nil {
			continue
		}
		header, ok := sessionHeaderFromAny(value)
		if !ok || header.Title == "" || header.ID == "" {
			continue
		}
		if _, eligible := eligibleNames[header.Title]; !eligible {
			continue
		}
		reconciled = append(reconciled, WaveAgent{Name: header.Title, SessionID: header.ID})
	}
	return reconciled
}

// SessionLive reports whether list-sessions shows a live session named name. An EXITED line is
// not live, and a line naming a longer session (a prefix sibling) does not match.
func SessionLive(listSessionsText, name string) bool {
	for line := range strings.SplitSeq(listSessionsText, "\n") {
		trimmed := strings.TrimLeft(line, " \t\r\n\v\f")
		if trimmed == name || strings.HasPrefix(trimmed, name+" ") || strings.HasPrefix(trimmed, name+"\t") {
			if !strings.Contains(strings.ToUpper(line), "EXITED") {
				return true
			}
		}
	}
	return false
}

// ReadPrefix is an optional FileSystem extension used by session scans.
type PrefixReader interface {
	ReadPrefix(path string, maxBytes int) ([]byte, error)
}

// TailReader is an optional FileSystem extension that reads the end of a file.
type TailReader interface {
	ReadTail(path string, maxBytes int) ([]byte, error)
}
