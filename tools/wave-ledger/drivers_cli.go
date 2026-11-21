package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
)

type driversJSON struct {
	From                string              `json:"from"`
	To                  string              `json:"to"`
	Skipped             int                 `json:"skipped"`
	BadFrames           int                 `json:"bad_frames"`
	NoDigest            int                 `json:"no_digest"`
	BadDigest           int                 `json:"bad_digest"`
	BadDigestShapes     []DigestShape       `json:"bad_digest_shapes"`
	MissingPromptFile   int                 `json:"missing_prompt_file"`
	BadPromptFile       int                 `json:"bad_prompt_file"`
	InvalidAgentRows    int                 `json:"invalid_agent_rows"`
	ConflictingSessions int                 `json:"conflicting_sessions"`
	ConflictingRows     int                 `json:"conflicting_rows"`
	Unattributed        []UnattributedAgent `json:"unattributed"`
	Systems             []SystemDriver      `json:"systems"`
	Drivers             []Driver            `json:"drivers"`
}

func runDrivers(args []string, stdout, stderr io.Writer) error {
	flags := flag.NewFlagSet("drivers", flag.ContinueOnError)
	flags.SetOutput(stderr)
	fromText := flags.String("from", "", "start UTC date (YYYY-MM-DD)")
	toText := flags.String("to", "", "end UTC date (YYYY-MM-DD)")
	dir := flags.String("dir", "", "ledger directory (required)")
	jsonOutput := flags.Bool("json", false, "write JSON; share is source bytes divided by content plus system bytes")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *dir == "" {
		return errors.New("--dir is required")
	}
	from, err := parseDate(*fromText)
	if err != nil {
		return fmt.Errorf("invalid --from: %w", err)
	}
	to, err := parseDate(*toText)
	if err != nil {
		return fmt.Errorf("invalid --to: %w", err)
	}
	if to.Before(from) {
		return errors.New("--to must be on or after --from")
	}
	root := *dir
	rows, skipped, err := ReadRows(root, from, to)
	if err != nil {
		return err
	}
	bad := 0
	drivers, systems, counts, err := Drivers(rows, root, func(fn func(ContentRow) error) error {
		var readErr error
		bad, readErr = ReadContent(root, from, to, fn)
		return readErr
	})
	if err != nil {
		return err
	}
	report := driversJSON{From: *fromText, To: *toText, Skipped: skipped, BadFrames: bad, NoDigest: counts.NoDigest, BadDigest: counts.BadDigest, BadDigestShapes: counts.BadDigestShapes, MissingPromptFile: counts.MissingPromptFile, BadPromptFile: counts.BadPromptFile, InvalidAgentRows: counts.InvalidAgentRows, ConflictingSessions: counts.ConflictingSessions, ConflictingRows: counts.ConflictingRows, Unattributed: counts.Unattributed, Systems: systems, Drivers: drivers}
	return writeDrivers(stdout, report, *jsonOutput)
}

func writeDrivers(out io.Writer, report driversJSON, asJSON bool) error {
	if asJSON {
		encoder := json.NewEncoder(out)
		encoder.SetIndent("", "  ")
		return encoder.Encode(report)
	}
	if _, err := fmt.Fprintf(out, "from: %s\nto: %s\nskipped: %d\nbad_frames: %d\nno_digest: %d\nbad_digest: %d\nmissing_prompt_file: %d\n", report.From, report.To, report.Skipped, report.BadFrames, report.NoDigest, report.BadDigest, report.MissingPromptFile); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "bad_prompt_file: %d\ninvalid_agent_rows: %d\nconflicting_sessions: %d\nconflicting_rows: %d\n", report.BadPromptFile, report.InvalidAgentRows, report.ConflictingSessions, report.ConflictingRows); err != nil {
		return err
	}
	for _, agent := range report.Unattributed {
		if _, err := fmt.Fprintf(out, "unattributed agent=%s input_tokens=%d\n", agent.Agent, agent.InputTokens); err != nil {
			return err
		}
	}
	for _, shape := range report.BadDigestShapes {
		if _, err := fmt.Fprintf(out, "bad_digest_shape length=%d class=%s rows=%d\n", shape.Length, shape.Class, shape.Rows); err != nil {
			return err
		}
	}
	for _, system := range report.Systems {
		if _, err := fmt.Fprintf(out, "agent=%s source=system bytes=%d rows=%d share=%.6f est_input_tokens=%d\n", system.Agent, system.Bytes, system.Rows, system.Share, system.EstInputTokens); err != nil {
			return err
		}
	}
	for _, driver := range report.Drivers {
		if _, err := fmt.Fprintf(out, "agent=%s source=%s bytes=%d share=%.6f est_input_tokens=%d\n", driver.Key.Agent, driver.Key.Source, driver.Bytes, driver.Share, driver.EstInputTokens); err != nil {
			return err
		}
	}
	return nil
}
