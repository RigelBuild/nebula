package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

const summaryPrefix = "wave-supervisor-relaunch: outcome="
const failedMark = "wave-supervisor-relaunch: outcome=failed"

type Outcome struct {
	Outcome   string
	Reason    string
	State     string
	Mode      string
	Session   string
	Detail    string
	ElapsedMS int64
}

var plainField = regexp.MustCompile(`^[A-Za-z0-9._:-]+$`)

func field(value string) string {
	if plainField.MatchString(value) {
		return value
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return `""`
	}
	return string(encoded)
}

func summaryLine(outcome Outcome) string {
	prefix := ""
	if outcome.Outcome == outcomeFailed {
		prefix = "<4> "
	}
	fields := []string{
		"outcome=" + outcome.Outcome,
		"reason=" + field(outcome.Reason),
	}
	if outcome.State != "" {
		fields = append(fields, "state="+outcome.State)
	}
	if outcome.Mode != "" {
		fields = append(fields, "mode="+outcome.Mode)
	}
	if outcome.Session != "" {
		session := outcome.Session
		if len(session) > 8 {
			session = session[:8]
		}
		fields = append(fields, "session="+field(session))
	}
	elapsed := max(outcome.ElapsedMS, 0)
	fields = append(fields, fmt.Sprintf("elapsed_ms=%d", elapsed))
	if outcome.Detail != "" {
		fields = append(fields, "detail="+field(outcome.Detail))
	}
	return prefix + summaryPrefix + strings.TrimPrefix(strings.Join(fields, " "), "outcome=")
}

func stopPostLine(serviceResult, exitStatus string) string {
	if serviceResult == "" || serviceResult == "success" || serviceResult == "exit-code" || serviceResult == "signal" && exitStatus == "TERM" {
		return ""
	}
	return summaryLine(Outcome{Outcome: outcomeFailed, Reason: "unit-result"})
}
