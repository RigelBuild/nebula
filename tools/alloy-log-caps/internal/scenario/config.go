package scenario

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v4"
)

var forwardToRe = regexp.MustCompile(`(?m)^\s*forward_to\s*=.*$`)

// yamlStage reads one extraLogProcessingStages string from a rendered chart.
func yamlStage(path string, keys ...string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is a chart render the flake check passes on argv.
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("parse %s: %w", path, err)
	}
	v, ok := nested(doc, keys...)
	if !ok {
		return "", fmt.Errorf("%s: missing %s", path, strings.Join(keys, "."))
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%s: %s is not a string", path, strings.Join(keys, "."))
	}
	return s, nil
}

// nested walks keys through maps; ok is false when a key or map is missing.
func nested(doc map[string]any, keys ...string) (any, bool) {
	var cur any = doc
	for _, k := range keys {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[k]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// readHostStages returns the journal loki.process body of a rendered host config.
func readHostStages(path string) (string, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is the Alloy config the caller passes on argv.
	if err != nil {
		return "", fmt.Errorf("read host config: %w", err)
	}
	return journalBody(string(raw))
}

// journalBody cuts the journal loki.process body out of a rendered Alloy config
// and drops its first forward_to line.
func journalBody(config string) (string, error) {
	const missing = "no journal loki.process in host config"
	start := strings.Index(config, `loki.process "journal" {`)
	if start < 0 {
		return "", errors.New(missing)
	}
	end := strings.Index(config[start:], "\n}")
	if end < 0 {
		return "", errors.New(missing)
	}
	end += start
	open := strings.Index(config[start:], "{") + start
	body := ""
	if end > open+1 {
		body = config[open+1 : end]
	}
	if loc := forwardToRe.FindStringIndex(body); loc != nil {
		body = body[:loc[0]] + body[loc[1]:]
	}
	return body, nil
}
