package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/samber/oops"
)

const stackStart = "<!-- start jj-vine stack -->"
const stackEnd = "<!-- end jj-vine stack -->"
const fallbackTemplate = "## What does this do\n\n<!-- Describe the behavior change in 1–3 plain sentences, using words a user or operator would use. -->\n\n## Why do we need it\n\n<!-- Describe the problem and what breaks or costs money without this change. -->\n\n## Summary\n\n## Verification\n\n## Risks\n"

var commentRE = regexp.MustCompile(`(?s)<!--.*?-->`)
var skippedChecksRE = regexp.MustCompile(`(?m)^Checks skipped: [^\r\n]*\r?\n?`)

func splitBody(body string) (string, string, error) {
	starts, ends := strings.Count(body, stackStart), strings.Count(body, stackEnd)
	if starts == 0 && ends == 0 {
		return "", strings.TrimSpace(body) + "\n", nil
	}
	if starts != 1 || ends != 1 {
		return "", "", refusal("metadata", "stack-markers", "ambiguous jj-vine stack markers", "repair the remote body before rerunning rjc submit")
	}
	start, end := strings.Index(body, stackStart), strings.Index(body, stackEnd)
	if start > end {
		return "", "", refusal("metadata", "stack-markers", "reversed jj-vine stack markers", "repair the remote body before rerunning rjc submit")
	}
	end += len(stackEnd)
	authored := strings.TrimSpace(body[:start]+body[end:]) + "\n"
	return body[start:end], authored, nil
}

type bodySection struct {
	heading    string
	start, end int
}

func sections(body string) ([]bodySection, error) {
	result := []bodySection{}
	offset := 0
	for line := range strings.SplitSeq(body, "\n") {
		if strings.HasPrefix(line, "## ") {
			heading := strings.TrimSpace(line)
			if slices.ContainsFunc(result, func(section bodySection) bool { return section.heading == heading }) {
				return nil, oops.Errorf("duplicate body heading %q", heading)
			}
			if len(result) > 0 {
				result[len(result)-1].end = offset
			}
			result = append(result, bodySection{heading: heading, start: offset + len(line), end: len(body)})
		}
		offset += len(line) + 1
	}
	return result, nil
}

func ComposeBody(keys issueKeys, template, authored string, issues []int, coAuthor string) (string, error) {
	if len(issues) == 0 || coAuthor == "" {
		return "", oops.Errorf("body requires an issue and co-author")
	}
	for _, issue := range issues {
		if issue <= 0 {
			return "", oops.Errorf("body requires a positive issue")
		}
	}
	if strings.Contains(authored, stackStart) || strings.Contains(authored, stackEnd) {
		return "", oops.Errorf("body file must not contain generated stack markers")
	}
	authored = strings.TrimSpace(keys.trailingRE.ReplaceAllString(commentRE.ReplaceAllString(authored, ""), ""))
	required, err := sections(commentRE.ReplaceAllString(template, ""))
	if err != nil {
		return "", err
	}
	actual, err := sections(authored)
	if err != nil {
		return "", err
	}
	cursor := 0
	for _, requiredSection := range required {
		index := slices.IndexFunc(actual[cursor:], func(section bodySection) bool { return section.heading == requiredSection.heading })
		if index < 0 {
			return "", oops.Errorf("body file must fill template section %q in order", requiredSection.heading)
		}
		section := actual[cursor+index]
		cursor += index + 1
		if strings.TrimSpace(authored[section.start:section.end]) == "" {
			return "", oops.Errorf("body section %q is empty", section.heading)
		}
	}
	var footer strings.Builder
	for _, issue := range issues {
		fmt.Fprintf(&footer, "Refs %s-%d\n", keys.prefix, issue)
	}
	return fmt.Sprintf("%s\n\n%s\nCo-authored-by: %s\n", authored, footer.String(), coAuthor), nil
}

func hashAuthored(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

func guardAuthored(baseline Published, exists bool, remote, ack string) error {
	if exists && remote == baseline.Authored {
		return nil
	}
	hash := hashAuthored(remote)
	if ack != "" && ack == hash {
		return nil
	}
	return refusal("gate-state", "human-body-edit", fmt.Sprintf("remote authored body differs from published baseline\n--- published\n%s\n+++ remote\n%s\nremote SHA-256: %s", baseline.Authored, remote, hash), "merge the remote body into --body-file, then rerun with --ack-remote "+hash)
}
