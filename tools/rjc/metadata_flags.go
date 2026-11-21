package main

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"github.com/spf13/cobra"
)

type sectionFlag struct {
	flag, heading, prefix string
	value                 *string
}

type namedSection struct {
	title, value string
	replaceOnly  bool
}

type metadataFlags struct {
	typ, scope                               string
	breaking                                 bool
	summary, verification, risks, specImpact string
	sectionArgs                              []string
	cmd                                      *cobra.Command
}

func (f *metadataFlags) sectionFlags() []sectionFlag {
	return []sectionFlag{
		{flag: "summary", heading: "Summary", value: &f.summary},
		{flag: "verification", heading: "Verification", value: &f.verification},
		{flag: "risks", heading: "Risks", value: &f.risks},
		{flag: "spec-impact", heading: "Spec impact", value: &f.specImpact, prefix: "Spec-impact:"},
	}
}

func (f *metadataFlags) bind(cmd *cobra.Command) {
	f.cmd = cmd
	flags := cmd.Flags()
	flags.StringVar(&f.typ, "type", "", "Conventional Commits type.")
	flags.StringVar(&f.scope, "scope", "", "Conventional Commits scope (requires --type).")
	flags.BoolVar(&f.breaking, "breaking", false, "Mark the title as breaking (requires --type).")
	for _, item := range f.sectionFlags() {
		flags.StringVar(item.value, item.flag, "", "Replace "+item.heading+" with a literal or @file.")
	}
	flags.StringArrayVar(&f.sectionArgs, "section", nil, "Add or replace Title=literal or Title=@file; last wins. Named flags take precedence.")
}

var scopeRE = regexp.MustCompile(`^[a-z0-9-]+$`)

func (f *metadataFlags) composeTitle(title string) (string, error) {
	command := f.commandName()
	title = strings.TrimSpace(title)
	if title == "" {
		return "", metadataUsage(command, "--title must not be empty")
	}
	if f.typ == "" {
		if f.scope != "" || f.breaking {
			return "", metadataUsage(command, "--scope and --breaking require --type")
		}
		return title, nil
	}
	if !slices.Contains(conventionalTypes, f.typ) {
		return "", metadataUsage(command, fmt.Sprintf("unknown conventional commit type %q", f.typ))
	}
	if f.scope != "" && !scopeRE.MatchString(f.scope) {
		return "", metadataUsage(command, "--scope must contain only lowercase letters, digits, or hyphens")
	}
	if titleTypePrefixRE.MatchString(title) {
		return "", metadataUsage(command, "--type cannot be used with a title that already has a type prefix")
	}
	prefix := f.typ
	if f.scope != "" {
		prefix += "(" + f.scope + ")"
	}
	if f.breaking {
		prefix += "!"
	}
	title = prefix + ": " + title
	return title, nil
}

func (f *metadataFlags) commandName() string {
	if f.cmd == nil {
		return submitCommandName
	}
	return f.cmd.Name()
}

func metadataUsage(command, reason string) error {
	return &failure{stage: usageStage, gate: "metadata", reason: reason, fix: "see rjc " + command + " --help", code: 2}
}

func sectionValue(value string) (string, error) {
	if !strings.HasPrefix(value, "@") {
		return value, nil
	}
	data, err := os.ReadFile(value[1:])
	if err != nil {
		return "", refusal("metadata", "section-file", fmt.Sprintf("read section file: %v", err), "supply a readable @file")
	}
	return string(data), nil
}

func (f *metadataFlags) resolve() ([]namedSection, error) {
	resolved := make([]namedSection, 0, len(f.sectionArgs)+len(f.sectionFlags()))
	for _, entry := range f.sectionArgs {
		title, value, ok := strings.Cut(entry, "=")
		title = strings.TrimSpace(title)
		if !ok || title == "" || strings.ContainsAny(title, "\r\n") {
			return nil, metadataUsage(f.commandName(), "--section requires Title=value")
		}
		value, err := sectionValue(value)
		if err != nil {
			return nil, err
		}
		resolved = append(resolved, namedSection{title: title, value: value})
	}
	for _, item := range f.sectionFlags() {
		if f.cmd == nil || !f.cmd.Flags().Changed(item.flag) {
			continue
		}
		value, err := sectionValue(*item.value)
		if err != nil {
			return nil, err
		}
		if item.prefix != "" && !strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), strings.ToLower(item.prefix)) {
			value = item.prefix + " " + value
		}
		resolved = append(resolved, namedSection{title: item.heading, value: value, replaceOnly: true})
	}
	return resolved, nil
}

func metadataBody(template, bodyFile string, resolved []namedSection) (string, error) {
	body := template
	if bodyFile != "" {
		data, err := os.ReadFile(bodyFile) //nolint:gosec // The caller selects the body file.
		if err != nil {
			return "", refusal("metadata", "body-file", fmt.Sprintf("read body file: %v", err), "supply a readable --body-file")
		}
		body = string(data)
	}
	for _, item := range resolved {
		var err error
		body, err = replaceSection(body, item.title, item.value, item.replaceOnly)
		if err != nil {
			return "", err
		}
	}
	return body, nil
}

func replaceSection(body, title, value string, replaceOnly bool) (string, error) {
	if strings.Contains(body, stackStart) || strings.Contains(body, stackEnd) {
		return "", refusal("metadata", "body-template", "body file must not contain generated stack markers", "remove generated stack markers")
	}
	body = commentRE.ReplaceAllString(body, "")
	found, err := sections(body)
	if err != nil {
		return "", refusal("metadata", "body-template", err.Error(), "remove duplicate body headings")
	}
	heading := "## " + title
	for _, section := range found {
		if section.heading == heading {
			return body[:section.start] + "\n\n" + strings.TrimSpace(value) + "\n\n" + body[section.end:], nil
		}
	}
	if replaceOnly {
		return "", refusal("metadata", "section", fmt.Sprintf("body has no section %q", title), "add the section with --section")
	}
	return strings.TrimSpace(body) + "\n\n" + heading + "\n\n" + strings.TrimSpace(value) + "\n", nil
}
