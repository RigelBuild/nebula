package inventory

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

var blobPattern = regexp.MustCompile(`blob:sha256:([0-9a-fA-F]{64})`)
var artifactPattern = regexp.MustCompile(`artifact://([0-9]+)`)

func (s *walkState) assignRecords() {
	rootIDs := make(map[string]string)
	rootStems := make(map[string]string)
	s.indexRootSessions(rootIDs, rootStems)
	s.assignSessionRecords(rootIDs, rootStems)
}

func (s *walkState) indexRootSessions(rootIDs, rootStems map[string]string) {
	for _, line := range s.inventory.Lines {
		if !isSessionsRoot(s.relative(line.Path)) || !isRootSession(s.relative(line.Path)) {
			continue
		}
		if line.SessionID == "" {
			s.inventory.Errors = append(s.inventory.Errors, AssignError{Path: line.Path, Reason: "root session has no session id"})
			continue
		}
		if previousPath, exists := rootIDs[line.SessionID]; exists {
			s.inventory.Errors = append(s.inventory.Errors, AssignError{Path: line.Path, Reason: "duplicate root session id also used by " + previousPath})
			continue
		}
		rootIDs[line.SessionID] = line.Path
		stem := strings.TrimSuffix(filepath.ToSlash(line.Path), filepath.Ext(line.Path))
		rootStems[stem] = "omp-session/" + line.SessionID
	}
}

func (s *walkState) assignSessionRecords(rootIDs, rootStems map[string]string) {
	for index := range s.inventory.Lines {
		line := &s.inventory.Lines[index]
		if !isSessionsRoot(s.relative(line.Path)) {
			line.Record = "tree/" + line.Root
			continue
		}
		if isRootSession(s.relative(line.Path)) {
			if line.SessionID != "" {
				line.Record = "omp-session/" + line.SessionID
			}
			continue
		}
		stem, record := longestStem(line.Path, rootStems)
		if stem == "" {
			line.Record = "tree/unassigned-" + line.Root
		} else {
			line.Record = record
			if line.SessionID != "" && rootIDs[line.SessionID] != "" {
				s.inventory.Errors = append(s.inventory.Errors, AssignError{Path: line.Path, Reason: "nested session id matches root session " + rootIDs[line.SessionID]})
			}
		}
	}
}

func longestStem(path string, stems map[string]string) (string, string) {
	var selected string
	var record string
	for stem, candidate := range stems {
		if strings.HasPrefix(path, stem+"/") && len(stem) > len(selected) {
			selected = stem
			record = candidate
		}
	}
	return selected, record
}

func (s *walkState) relative(path string) string {
	rel, err := filepath.Rel(s.cfg.Root, path)
	if err != nil {
		return path
	}
	return rel
}

func isSessionsRoot(path string) bool {
	return slices.Contains(strings.Split(filepath.ToSlash(path), "/"), "sessions")
}

func isRootSession(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	for index, part := range parts {
		if part == "sessions" {
			return len(parts)-index == 3 && strings.HasSuffix(strings.ToLower(parts[len(parts)-1]), ".jsonl")
		}
	}
	return false
}

func (s *walkState) checkClosure() {
	blobFiles := make(map[string]Line)
	artifacts := indexArtifacts(s.inventory.Lines)
	for _, line := range s.inventory.Lines {
		if strings.Contains(filepath.ToSlash(line.Path), "/blobs/") || strings.HasPrefix(filepath.ToSlash(line.Path), "blobs/") {
			blobFiles[filepath.Base(line.Path)] = line
		}
	}
	for _, line := range s.inventory.Lines {
		s.scanClosureFile(line, blobFiles, artifacts)
	}
}

func (s *walkState) scanClosureFile(line Line, blobs map[string]Line, artifacts map[string]map[string]struct{}) {
	if line.Type != "file" || line.Class == "final-only" {
		return
	}
	file, err := os.Open(s.absolute[line.Path])
	if err != nil {
		s.inventory.Errors = append(s.inventory.Errors, AssignError{Path: line.Path, Reason: "open file for closure scan"})
		return
	}
	reader := bufio.NewReader(file)
	for {
		chunk, readErr := reader.ReadString('\n')
		if strings.Contains(chunk, "blob:sha256:") || strings.Contains(chunk, "artifact://") {
			s.checkReferences(line, chunk, blobs, artifacts)
		}
		if readErr != nil {
			break
		}
	}
	if err := file.Close(); err != nil {
		s.inventory.Errors = append(s.inventory.Errors, AssignError{Path: line.Path, Reason: "close file in closure scan"})
	}
}

func indexArtifacts(lines []Line) map[string]map[string]struct{} {
	artifacts := make(map[string]map[string]struct{})
	for _, line := range lines {
		base := filepath.Base(line.Path)
		id, _, ok := strings.Cut(base, ".")
		if !ok {
			continue
		}
		for dir := filepath.Dir(line.Path); dir != "." && dir != "/"; dir = filepath.Dir(dir) {
			key := line.Record + "\x00" + filepath.ToSlash(dir)
			if artifacts[key] == nil {
				artifacts[key] = make(map[string]struct{})
			}
			artifacts[key][id] = struct{}{}
		}
	}
	return artifacts
}

func (s *walkState) checkReferences(line Line, text string, blobs map[string]Line, artifacts map[string]map[string]struct{}) {
	for _, match := range blobPattern.FindAllStringSubmatch(text, -1) {
		digest := strings.ToLower(match[1])
		blob, ok := blobs[digest]
		if !ok {
			s.inventory.Dangling = append(s.inventory.Dangling, Dangling{Path: line.Path, Record: line.Record, Reference: match[0], Reason: "missing"})
			continue
		}
		if fmt.Sprintf("%x", blob.SHA256) != digest {
			s.inventory.Dangling = append(s.inventory.Dangling, Dangling{Path: line.Path, Record: line.Record, Reference: match[0], Reason: "digest-mismatch"})
		}
	}
	stem := strings.TrimSuffix(filepath.ToSlash(line.Path), filepath.Ext(line.Path))
	for _, match := range artifactPattern.FindAllStringSubmatch(text, -1) {
		if _, found := artifacts[line.Record+"\x00"+stem][match[1]]; !found {
			s.inventory.Dangling = append(s.inventory.Dangling, Dangling{Path: line.Path, Record: line.Record, Reference: match[0], Reason: "artifact-missing"})
		}
	}

}
