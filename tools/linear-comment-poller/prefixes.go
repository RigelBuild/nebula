package main

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// issuePrefixes is the set of Linear team keys this poller watches, for example
// "EX" in "EX-42". It is built once at startup from the required --issue-prefix
// flag and then carried on SweepDeps, or passed to pure functions as a parameter.
type issuePrefixes struct {
	keys    []string
	pattern *regexp.Regexp
}

// parseIssuePrefixes reads team keys from repeated --issue-prefix values; each value
// may also hold a comma-separated list. The result must name at least one key. Each
// key is matched literally and case-insensitively.
func parseIssuePrefixes(values []string) (issuePrefixes, error) {
	var keys []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			key := strings.ToUpper(strings.TrimSpace(part))
			if key == "" {
				continue
			}
			if !validTeamKey(key) {
				return issuePrefixes{}, fmt.Errorf("issue prefix %q must be a letter followed by letters and digits", key)
			}
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return issuePrefixes{}, errors.New("--issue-prefix (or " + issuePrefixEnv + ") must name at least one team key")
	}
	alternatives := make([]string, len(keys))
	for index, key := range keys {
		alternatives[index] = regexp.QuoteMeta(key)
	}
	pattern := regexp.MustCompile(`(?i)^(?:` + strings.Join(alternatives, "|") + `)-(\d+)$`)
	return issuePrefixes{keys: keys, pattern: pattern}, nil
}

// issuePrefixEnv is the environment equivalent of --issue-prefix.
const issuePrefixEnv = "LINEAR_ISSUE_PREFIXES"

func validTeamKey(key string) bool {
	for index, r := range key {
		letter := r >= 'A' && r <= 'Z'
		if !letter && (index == 0 || r < '0' || r > '9') {
			return false
		}
	}
	return key != ""
}

// normaliseIssueID upper-cases the matched team key and returns the canonical
// identifier, such as "EX-42". It reports false when the identifier is not one of the
// configured keys, or when its number is not a positive safe integer.
func (prefixes issuePrefixes) normaliseIssueID(id string) (string, bool) {
	match := prefixes.pattern.FindStringSubmatch(id)
	if match == nil {
		return "", false
	}
	number, err := strconv.ParseUint(match[1], 10, 53)
	if err != nil || number < 1 {
		return "", false
	}
	key := strings.ToUpper(id[:len(id)-len(match[1])-1])
	return fmt.Sprintf("%s-%d", key, number), true
}

// hasKey reports whether an identifier belongs to one of the configured team keys.
func (prefixes issuePrefixes) hasKey(identifier string) bool {
	_, ok := prefixes.normaliseIssueID(identifier)
	return ok
}

// commentsQuery builds the GraphQL query that fetches watched issues by team key and number.
func commentsQuery(prefixes issuePrefixes) string {
	quoted := make([]string, len(prefixes.keys))
	for index, key := range prefixes.keys {
		quoted[index] = strconv.Quote(key)
	}
	return strings.Replace(commentsQueryTemplate, "{{TEAM_KEYS}}", strings.Join(quoted, ", "), 1)
}
