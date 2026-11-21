package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

var isoDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$`)

const genericError = "Error"

func parseWatchlist(prefixes issuePrefixes, raw json.RawMessage) (Watchlist, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return Watchlist{}, errors.New("watchlist has an invalid schemaVersion")
	}
	var value struct {
		SchemaVersion string            `json:"schemaVersion"`
		Issues        []json.RawMessage `json:"issues"`
	}
	if err := json.Unmarshal(fields["schemaVersion"], &value.SchemaVersion); err != nil || value.SchemaVersion != "watchlist-v1" {
		return Watchlist{}, errors.New("watchlist has an invalid schemaVersion")
	}
	issuesRaw, ok := fields["issues"]
	if !ok || len(issuesRaw) == 0 || issuesRaw[0] != '[' {
		return Watchlist{}, errors.New("watchlist issues must be an array")
	}
	if err := json.Unmarshal(issuesRaw, &value.Issues); err != nil {
		return Watchlist{}, errors.New("watchlist issues must be an array")
	}
	issues := make([]WatchEntry, 0, len(value.Issues))
	for index, rawEntry := range value.Issues {
		var entryFields map[string]json.RawMessage
		if err := json.Unmarshal(rawEntry, &entryFields); err != nil || entryFields == nil {
			return Watchlist{}, fmt.Errorf("watchlist issue %d must be an object", index+1)
		}
		var id string
		if err := json.Unmarshal(entryFields["id"], &id); err != nil {
			return Watchlist{}, fmt.Errorf("watchlist issue %d has an invalid id", index+1)
		}
		id, valid := prefixes.normaliseIssueID(id)
		if !valid {
			return Watchlist{}, fmt.Errorf("watchlist issue %d has an invalid id", index+1)
		}
		watermarkRaw, ok := entryFields["watermark"]
		if !ok {
			return Watchlist{}, fmt.Errorf("watchlist issue %d has an invalid watermark", index+1)
		}
		var watermark *string
		if string(watermarkRaw) != "null" {
			var value string
			if err := json.Unmarshal(watermarkRaw, &value); err != nil || !isoDatePattern.MatchString(value) || !validDate(value) {
				return Watchlist{}, fmt.Errorf("watchlist issue %d has an invalid watermark", index+1)
			}
			watermark = new(value)
		}
		issues = append(issues, WatchEntry{ID: id, Watermark: watermark})
	}
	return Watchlist{SchemaVersion: "watchlist-v1", Issues: issues}, nil
}

func readWatchlists(prefixes issuePrefixes, root string) (WatchlistsRead, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return WatchlistsRead{}, err
	}
	directories := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			directories = append(directories, entry.Name())
		}
	}
	sort.Strings(directories)
	result := WatchlistsRead{
		Agents:   []AgentWatchlist{},
		IDs:      []string{},
		Warnings: []string{},
		Skipped:  []string{},
		Present:  directories,
	}
	ids := map[string]struct{}{}
	for _, codename := range directories {
		path := filepath.Join(root, codename, "watchlist.json")
		contents, err := os.ReadFile(path) //nolint:gosec // The watchlist is under the operator-selected workspace root.
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			result.Warnings = append(result.Warnings, codename+"/watchlist.json: "+err.Error())
			result.Skipped = append(result.Skipped, codename)
			continue
		}
		var raw json.RawMessage
		if err := json.Unmarshal(contents, &raw); err != nil {
			result.Warnings = append(result.Warnings, codename+"/watchlist.json: "+syntaxErrorMessage(contents, err))
			result.Skipped = append(result.Skipped, codename)
			continue
		}
		watchlist, err := parseWatchlist(prefixes, raw)
		if err != nil {
			result.Warnings = append(result.Warnings, codename+"/watchlist.json: "+err.Error())
			result.Skipped = append(result.Skipped, codename)
			continue
		}
		result.Agents = append(result.Agents, AgentWatchlist{Codename: codename, Watchlist: watchlist})
		for _, issue := range watchlist.Issues {
			ids[issue.ID] = struct{}{}
		}
	}
	for id := range ids {
		result.IDs = append(result.IDs, id)
	}
	sort.Strings(result.IDs)
	return result, nil
}

func errorCause(err error) string {
	if err == nil {
		return genericError
	}
	if _, ok := errors.AsType[*net.OpError](err); ok {
		if errors.Is(err, syscall.ECONNRESET) {
			return "TypeError ECONNRESET"
		}
		if errors.Is(err, syscall.ECONNREFUSED) {
			return "TypeError ECONNREFUSED"
		}
	}
	if dnsError, ok := errors.AsType[*net.DNSError](err); ok && dnsError.IsNotFound {
		return "TypeError ENOTFOUND"
	}
	name := genericError
	if _, ok := errors.AsType[*os.PathError](err); ok {
		name = genericError
	} else if _, ok := errors.AsType[*json.SyntaxError](err); ok {
		name = "SyntaxError"
	} else if _, ok := errors.AsType[*json.UnmarshalTypeError](err); ok {
		name = "SyntaxError"
	}
	if pathError, ok := errors.AsType[*os.PathError](err); ok {
		code := strings.ToUpper(strings.ReplaceAll(pathError.Err.Error(), " ", "_"))
		if strings.HasPrefix(code, "E") && code == strings.ToUpper(code) {
			return name + " " + code
		}
	}
	return name
}

func syntaxErrorMessage(contents []byte, err error) string {
	if _, ok := errors.AsType[*json.SyntaxError](err); !ok {
		return err.Error()
	}
	fields := strings.Fields(string(contents))
	if len(fields) > 0 {
		identifier := fields[0]
		first := identifier[0]
		isIdentifier := first == '_' || first == '$' || first >= 'a' && first <= 'z' || first >= 'A' && first <= 'Z'
		if isIdentifier && identifier != "true" && identifier != "false" && identifier != "null" {
			return `JSON Parse error: Unexpected identifier "` + identifier + `"`
		}
	}
	return "JSON Parse error: " + err.Error()
}
