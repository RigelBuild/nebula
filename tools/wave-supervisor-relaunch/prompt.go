package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const promptASCIILimit = 2000

func ascii(value string) string {
	var builder strings.Builder
	for _, character := range value {
		if character < 128 {
			builder.WriteRune(character)
		} else {
			builder.WriteByte('?')
		}
	}
	return builder.String()
}

func zellijServerText(identity string) string {
	switch identity {
	case identityNone:
		return "not running"
	case identityUnavailable:
		return "unreadable"
	default:
		return "pid:starttime " + identity
	}
}

func unitFacts(trigger Trigger) string {
	units := make([]string, 0, len(trigger.Units)+1)
	for _, unit := range trigger.Units {
		start := unit.ActiveSince
		if start == "" {
			start = "unknown start"
		}
		invocation := unit.InvocationID
		if invocation == "" {
			invocation = factUnknown
		}
		active := unit.ActiveState
		if active == "" {
			active = factUnknown
		}
		result := unit.Result
		if result == "" {
			result = factUnknown
		}
		units = append(units, fmt.Sprintf("%s: %s, InvocationID %s, NRestarts %d, %s/%s", unit.Unit, start, invocation, unit.NRestarts, active, result))
	}
	units = append(units, "zellij server "+zellijServerText(trigger.ZellijServer))
	return strings.Join(units, "; ")
}

// journalFlags renders the configured journal units as journalctl -u flags, in config order.
func journalFlags(units []string) string {
	flags := make([]string, 0, 2*len(units))
	for _, unit := range units {
		flags = append(flags, "-u", unit)
	}
	return strings.Join(flags, " ")
}

func relaunchPrompt(cfg Config, trigger Trigger, manifestPath string, manifestWritten bool) string {
	earliest := time.Time{}
	for _, unit := range trigger.Units {
		parsed, err := time.Parse("Mon 2006-01-02 15:04:05 MST", unit.ActiveSince)
		if err == nil && (earliest.IsZero() || parsed.Before(earliest)) {
			earliest = parsed
		}
	}
	since := "30 minutes before the earliest known unit start"
	if !earliest.IsZero() {
		since = earliest.Add(-30 * time.Minute).UTC().Format("2006-01-02 15:04:05 UTC")
	}
	lines := make([]string, 0, 7)
	lines = append(lines,
		fmt.Sprintf("Wave auto-relaunch (%s). %s; boot %s, uptime %v.", cfg.RelaunchIssue, unitFacts(trigger), trigger.BootID, trigger.UptimeS),
		"You were relaunched in continue mode.",
	)
	if manifestWritten {
		lines[1] += " " + manifestPath + " records the seats that were stopped."
	}
	lines = append(lines,
		"1. Find why the wave restarted before anything else: journalctl -b -1 -p warning after a reboot, journalctl -k for OOM kills,",
		"   journalctl --user "+journalFlags(cfg.JournalUnits)+" since "+since+",",
		"   and "+cfg.ManagerLogPath+". Known cause: "+cfg.KnownCause+" ("+cfg.RelaunchIssue+").",
		"2. Report the cause with timestamps, PIDs and RSS on the open issue for that cause, or a new issue (Owner: "+cfg.CauseOwner+"). DM "+cfg.CauseOwner+" the link.",
		"3. Lanes relaunch from "+cfg.SeatsPath+"; do not spawn them. For a seat "+cfg.SeatCLI+" parks, run "+cfg.SeatCLI+" resume <name>.",
	)
	prompt := ascii(strings.Join(lines, "\n"))
	if utf8.RuneCountInString(prompt) > promptASCIILimit {
		prompt = strings.TrimRight(prompt[:promptASCIILimit-1], " \t\r\n") + "\n"
	}
	return prompt
}

func liveNotice(trigger Trigger) string {
	notice := ascii(fmt.Sprintf("Wave restarted; trigger %s. Manager/zellij unit facts: %s. Please investigate the restart and relaunch lanes from the board if needed.", trigger.BootID, unitFacts(trigger)))
	return firstWords(notice, 150)
}

func armedNotice(trigger Trigger) string {
	notice := ascii(fmt.Sprintf("Wave relaunch armed with no prior baseline; trigger %s. Manager/zellij unit facts: %s. A restart is unconfirmed: check the start times above; investigate only if they are recent.", trigger.BootID, unitFacts(trigger)))
	return firstWords(notice, 150)
}

func firstWords(value string, maximum int) string {
	words := strings.Fields(value)
	if len(words) > maximum {
		words = words[:maximum]
	}
	return strings.Join(words, " ")
}
