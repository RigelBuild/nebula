// SPDX-License-Identifier: MIT OR Apache-2.0

// Package serviceurls parses and checks the service URL inventory.
package serviceurls

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	toml "github.com/pelletier/go-toml/v2"
)

const (
	// DataPath holds the inventory: groups.toml declares groups; other TOML files hold links.
	DataPath    = "docs/specs/infrastructure/service-urls"
	PagePath    = "docs/specs/infrastructure/links.md"
	httpScheme  = "http"
	httpsScheme = "https"
)

type Access string

type Kind string

const (
	AccessTailnet          Access = "tailnet"
	AccessCloudflareAccess Access = "cloudflare-access"
	AccessPublic           Access = "public"
	KindWeb                Kind   = "web"
	KindRedirect           Kind   = "redirect"
	KindEndpoint           Kind   = "endpoint"
)

type Group struct {
	ID    string `json:"id"    toml:"id"`
	Title string `json:"title" toml:"title"`
}

type Link struct {
	Group  string `json:"group"            toml:"group"`
	Name   string `json:"name"             toml:"name"`
	URL    string `json:"url"              toml:"url"`
	What   string `json:"what"             toml:"what"`
	Access Access `json:"access"           toml:"access"`
	Login  string `json:"login,omitempty"  toml:"login,omitempty"`
	Node   string `json:"node,omitempty"   toml:"node,omitempty"`
	Kind   Kind   `json:"kind"             toml:"kind"`
	Via    string `json:"via"              toml:"via"`
	Source string `json:"source,omitempty" toml:"source,omitempty"`
}

// Options configure inventory validation.
type Options struct {
	// InternalHostSuffixes are the DNS suffixes, such as "internal.example.com",
	// whose hosts are reachable only on the tailnet. A link whose host equals a
	// suffix or ends in "."+suffix must use tailnet access. At least one is
	// required; there is no default.
	InternalHostSuffixes []string
}

// suffixes returns the normalized internal host suffixes, or an error when none
// is set.
func (o Options) suffixes() ([]string, error) {
	out := make([]string, 0, len(o.InternalHostSuffixes))
	for _, suffix := range o.InternalHostSuffixes {
		suffix = strings.Trim(strings.ToLower(strings.TrimSpace(suffix)), ".")
		if suffix == "" {
			return nil, errors.New("service URL inventory options: internal host suffix is empty")
		}
		out = append(out, suffix)
	}
	if len(out) == 0 {
		return nil, errors.New("service URL inventory options: at least one internal host suffix is required")
	}
	return out, nil
}

type Inventory struct {
	Groups []Group `json:"groups"`
	Links  []Link  `json:"links"`
}
type inventoryDocument struct {
	tables       map[string]any
	groupOrigins []rowOrigin
	linkOrigins  []rowOrigin
}

type rowOrigin struct {
	file  string
	index int
}

func ParseInventory(text string, opts Options) (Inventory, error) {
	suffixes, err := opts.suffixes()
	if err != nil {
		return Inventory{}, err
	}
	document, err := decodeDocument(text)
	if err != nil {
		return Inventory{}, err
	}
	return inventoryFromDocument(inventoryDocument{tables: document}, suffixes)
}

// ReadInventory validates the TOML files directly under dir as one inventory.
// Files are read in name order. groups.toml declares the groups; the other files hold link rows.
func ReadInventory(dir string, opts Options) (Inventory, error) {
	suffixes, err := opts.suffixes()
	if err != nil {
		return Inventory{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Inventory{}, fmt.Errorf("read service URL inventory directory: %w", err)
	}
	groupsFileFound := false
	for _, entry := range entries {
		if entry.Name() == "groups.toml" && !entry.IsDir() {
			groupsFileFound = true
			break
		}
	}
	if !groupsFileFound {
		return Inventory{}, errors.New("service URL inventory directory is missing groups.toml")
	}

	document := inventoryDocument{tables: map[string]any{"group": []any{}, "link": []any{}}}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".toml" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name())) //nolint:gosec // Each name is listed from the inventory directory.
		if err != nil {
			return Inventory{}, fmt.Errorf("%s: read service URL inventory file: %w", entry.Name(), err)
		}
		part, err := decodeDocument(string(data))
		if err != nil {
			return Inventory{}, fmt.Errorf("%s: %w", entry.Name(), err)
		}
		expectedTable := "link"
		if entry.Name() == "groups.toml" {
			expectedTable = "group"
		}
		if err := appendTables(&document, part, entry.Name(), expectedTable); err != nil {
			return Inventory{}, fmt.Errorf("%s: %w", entry.Name(), err)
		}
	}
	return inventoryFromDocument(document, suffixes)
}

// appendTables adds one file's rows to document, keeping their file order.
func appendTables(document *inventoryDocument, part map[string]any, fileName, expectedTable string) error {
	keys := make([]string, 0, len(part))
	for key := range part {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		value := part[key]
		if key != "group" && key != "link" {
			return fmt.Errorf("service URL inventory has unknown key %q", key)
		}
		if key != expectedTable {
			return fmt.Errorf("service URL inventory file must contain only [[%s]] tables, found [[%s]]", expectedTable, key)
		}
		rows, ok := value.([]any)
		if !ok {
			return fmt.Errorf("top-level %s must be an array of tables", key)
		}
		for index, row := range rows {
			if _, ok := row.(map[string]any); !ok {
				return fmt.Errorf("%s[%d] must be a table", key, index)
			}
		}
		existing, _ := document.tables[key].([]any)
		document.tables[key] = append(existing, rows...)
		origins := &document.linkOrigins
		if key == "group" {
			origins = &document.groupOrigins
		}
		for index := range rows {
			*origins = append(*origins, rowOrigin{file: fileName, index: index})
		}
	}
	return nil
}

func decodeDocument(text string) (map[string]any, error) {
	text = strings.TrimSpace(text)
	if _, err := strconv.Unquote(text); err == nil {
		return nil, errors.New("service URL inventory must be a TOML table")
	}
	var document map[string]any
	if err := toml.Unmarshal([]byte(text), &document); err != nil {
		return nil, fmt.Errorf("service URL inventory is invalid TOML: %w", err)
	}
	if document == nil {
		return nil, errors.New("service URL inventory must be a TOML table")
	}
	return document, nil
}

func inventoryFromDocument(document inventoryDocument, internalSuffixes []string) (Inventory, error) {
	violations := make([]string, 0)
	unknownKeys(document.tables, []string{"group", "link"}, "inventory", &violations)
	groups, groupIDs := parseGroups(document.tables, document.groupOrigins, &violations)
	links := parseLinks(document.tables, document.linkOrigins, groupIDs, internalSuffixes, &violations)
	if len(violations) > 0 {
		return Inventory{}, fmt.Errorf("%d service URL inventory violation(s):\n%s", len(violations), strings.Join(violations, "\n"))
	}
	return Inventory{Groups: groups, Links: links}, nil
}

func parseGroups(document map[string]any, origins []rowOrigin, violations *[]string) ([]Group, map[string]struct{}) {
	groups := make([]Group, 0)
	groupIDs := make(map[string]struct{})
	for index, row := range tableRows(document, "group", violations) {
		rowPath := inventoryRowPath("group", index, origins)
		unknownKeys(row, []string{"id", "title"}, rowPath, violations)
		id := stringField(row, "id", rowPath, violations)
		title := stringField(row, "title", rowPath, violations)
		if id != "" {
			if _, exists := groupIDs[id]; exists {
				*violations = append(*violations, rowPath+": duplicate group id "+strconv.Quote(id))
			}
			groupIDs[id] = struct{}{}
		}
		if id != "" && title != "" {
			groups = append(groups, Group{ID: id, Title: title})
		}
	}
	return groups, groupIDs
}

func inventoryRowPath(table string, index int, origins []rowOrigin) string {
	if index < len(origins) {
		origin := origins[index]
		return fmt.Sprintf("%s %s[%d]", origin.file, table, origin.index)
	}
	return fmt.Sprintf("%s[%d]", table, index)
}

type linkParser struct {
	groupIDs         map[string]struct{}
	seenURLs         map[string]struct{}
	internalSuffixes []string
	violations       *[]string
}

func parseLinks(document map[string]any, origins []rowOrigin, groupIDs map[string]struct{}, internalSuffixes []string, violations *[]string) []Link {
	links := make([]Link, 0)
	parser := linkParser{
		groupIDs:         groupIDs,
		seenURLs:         make(map[string]struct{}),
		internalSuffixes: internalSuffixes,
		violations:       violations,
	}
	for index, row := range tableRows(document, "link", violations) {
		rowPath := inventoryRowPath("link", index, origins)
		if link, ok := parser.parse(row, rowPath); ok {
			links = append(links, link)
		}
	}
	return links
}

func (parser *linkParser) parse(row map[string]any, rowPath string) (Link, bool) {
	unknownKeys(row, []string{"group", "name", "url", "what", "access", "login", "node", "kind", "via", "source"}, rowPath, parser.violations)
	group := stringField(row, "group", rowPath, parser.violations)
	name := stringField(row, "name", rowPath, parser.violations)
	linkURL := stringField(row, "url", rowPath, parser.violations)
	what := stringField(row, "what", rowPath, parser.violations)
	rawAccess := stringField(row, "access", rowPath, parser.violations)
	login := optionalStringField(row, "login", rowPath, parser.violations)
	node := optionalStringField(row, "node", rowPath, parser.violations)
	rawKind := stringField(row, "kind", rowPath, parser.violations)
	via := stringField(row, "via", rowPath, parser.violations)
	source := optionalStringField(row, "source", rowPath, parser.violations)

	if group != "" {
		if _, exists := parser.groupIDs[group]; !exists {
			*parser.violations = append(*parser.violations, rowPath+".group references undeclared group "+strconv.Quote(group))
		}
	}
	access := Access(rawAccess)
	if rawAccess != "" && !slices.Contains([]Access{AccessTailnet, AccessCloudflareAccess, AccessPublic}, access) {
		*parser.violations = append(*parser.violations, rowPath+".access must be tailnet, cloudflare-access, or public")
	}
	kind := Kind(rawKind)
	if rawKind != "" && !slices.Contains([]Kind{KindWeb, KindRedirect, KindEndpoint}, kind) {
		*parser.violations = append(*parser.violations, rowPath+".kind must be web, redirect, or endpoint")
	}

	parsedURL := parser.parseURL(linkURL, rowPath)
	if source != "" && strings.Contains(source, "`") {
		*parser.violations = append(*parser.violations, rowPath+".source must not contain backticks")
	}
	validateLinkURL(parsedURL, kind, access, rowPath, parser.internalSuffixes, parser.violations)
	if group != "" && name != "" && linkURL != "" && what != "" && access != "" && kind != "" && via != "" {
		return Link{Group: group, Name: name, URL: linkURL, What: what, Access: access, Login: login, Node: node, Kind: kind, Via: via, Source: source}, true
	}
	return Link{}, false
}

func (parser *linkParser) parseURL(linkURL, rowPath string) *url.URL {
	if linkURL == "" {
		return nil
	}
	if strings.ContainsAny(linkURL, "<>| \t\r\n") {
		*parser.violations = append(*parser.violations, rowPath+".url must not contain angle brackets, pipes, or whitespace")
	}
	parsedURL, err := url.Parse(linkURL)
	if err != nil {
		*parser.violations = append(*parser.violations, rowPath+".url must be an absolute URL")
		return nil
	}
	if parsedURL.Scheme == "" || parsedURL.Host == "" {
		*parser.violations = append(*parser.violations, rowPath+".url must be an absolute URL")
		return nil
	}
	parsedURL.Scheme = strings.ToLower(parsedURL.Scheme)
	parsedURL.Host = canonicalHost(parsedURL)
	if parsedURL.Path == "" && (parsedURL.Scheme == httpsScheme || parsedURL.Scheme == httpScheme) {
		parsedURL.Path = "/"
	}
	// Match WHATWG href: dot segments, including %2e forms, resolve before the duplicate check.
	if escaped, ok := resolveDotSegments(parsedURL.EscapedPath()); ok {
		decoded, err := url.PathUnescape(escaped)
		if err != nil {
			*parser.violations = append(*parser.violations, rowPath+".url must be an absolute URL")
			return nil
		}
		parsedURL.Path, parsedURL.RawPath = decoded, escaped
	}
	canonical := parsedURL.String()
	if _, exists := parser.seenURLs[canonical]; exists {
		*parser.violations = append(*parser.violations, rowPath+": duplicate url "+strconv.Quote(canonical))
	}
	parser.seenURLs[canonical] = struct{}{}
	if parsedURL.User != nil {
		*parser.violations = append(*parser.violations, rowPath+".url must not carry credentials")
	}
	return parsedURL
}

// resolveDotSegments applies RFC 3986 remove_dot_segments to an escaped absolute path.
func resolveDotSegments(escapedPath string) (string, bool) {
	if !strings.HasPrefix(escapedPath, "/") {
		return escapedPath, false
	}
	segments := strings.Split(escapedPath[1:], "/")
	output := make([]string, 0, len(segments))
	for index, segment := range segments {
		last := index == len(segments)-1
		switch strings.ToLower(segment) {
		case ".", "%2e":
			if last {
				output = append(output, "")
			}
		case "..", ".%2e", "%2e.", "%2e%2e":
			if len(output) > 0 {
				output = output[:len(output)-1]
			}
			if last {
				output = append(output, "")
			}
		default:
			output = append(output, segment)
		}
	}
	return "/" + strings.Join(output, "/"), true
}

func validateLinkURL(parsedURL *url.URL, kind Kind, access Access, rowPath string, internalSuffixes []string, violations *[]string) {
	if parsedURL != nil && kind != KindEndpoint && (kind == KindWeb || kind == KindRedirect) && parsedURL.Scheme != httpsScheme {
		*violations = append(*violations, rowPath+".url must use https for kind "+string(kind))
	}
	if parsedURL == nil || access == "" {
		return
	}
	host := strings.TrimSuffix(strings.ToLower(parsedURL.Hostname()), ".")
	internal := slices.ContainsFunc(internalSuffixes, func(suffix string) bool {
		return host == suffix || strings.HasSuffix(host, "."+suffix)
	})
	if internal && access != AccessTailnet {
		*violations = append(*violations, rowPath+".access must be tailnet for internal host "+strconv.Quote(host))
	}
}

func canonicalHost(value *url.URL) string {
	host := strings.ToLower(value.Hostname())
	port := value.Port()
	if (value.Scheme == httpsScheme && port == "443") || (value.Scheme == httpScheme && port == "80") {
		port = ""
	}
	if port != "" {
		return net.JoinHostPort(host, port)
	}
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

func unknownKeys(value map[string]any, allowed []string, rowPath string, violations *[]string) {
	for key := range value {
		if !slices.Contains(allowed, key) {
			*violations = append(*violations, rowPath+" has unknown key "+strconv.Quote(key))
		}
	}
}

func tableRows(value map[string]any, field string, violations *[]string) []map[string]any {
	raw, ok := value[field].([]any)
	if !ok {
		*violations = append(*violations, "top-level "+field+" must be an array of tables")
		return nil
	}
	rows := make([]map[string]any, 0, len(raw))
	for index, item := range raw {
		row, ok := item.(map[string]any)
		if !ok {
			*violations = append(*violations, fmt.Sprintf("%s[%d] must be a table", field, index))
			continue
		}
		rows = append(rows, row)
	}
	return rows
}

func stringField(value map[string]any, field, rowPath string, violations *[]string) string {
	text, ok := value[field].(string)
	if !ok {
		*violations = append(*violations, rowPath+"."+field+" must be a string")
		return ""
	}
	if strings.TrimSpace(text) == "" {
		*violations = append(*violations, rowPath+"."+field+" is empty")
		return ""
	}
	return text
}

func optionalStringField(value map[string]any, field, rowPath string, violations *[]string) string {
	if _, ok := value[field]; !ok {
		return ""
	}
	return stringField(value, field, rowPath, violations)
}

func LinkHost(link Link) (string, int, error) {
	parsed, err := url.Parse(link.URL)
	if err != nil {
		return "", 0, fmt.Errorf("parse service URL: %w", err)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	port := 443
	if parsed.Scheme == httpScheme {
		port = 80
	}
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			return "", 0, fmt.Errorf("parse service URL port: %w", err)
		}
	}
	return host, port, nil
}

func LinkRepo(link Link) (string, bool, error) {
	parsed, err := url.Parse(link.URL)
	if err != nil {
		return "", false, fmt.Errorf("parse service URL: %w", err)
	}
	if parsed.Scheme != httpsScheme || !strings.EqualFold(parsed.Hostname(), "github.com") {
		return "", false, nil
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	if len(parts) != 2 || parts[0] != "RigelBuild" {
		return "", false, nil
	}
	return strings.ToLower(parts[1]), true, nil
}

func DanglingSources(inventory Inventory, tracked []string) []string {
	paths := make(map[string]struct{}, len(tracked))
	for _, trackedPath := range tracked {
		paths[trackedPath] = struct{}{}
	}
	dangling := make([]string, 0)
	for _, link := range inventory.Links {
		if link.Source == "" {
			continue
		}
		if _, exists := paths[link.Source]; exists {
			continue
		}
		prefix := strings.TrimSuffix(link.Source, "/") + "/"
		found := false
		for trackedPath := range paths {
			if strings.HasPrefix(trackedPath, prefix) {
				found = true
				break
			}
		}
		if !found {
			dangling = append(dangling, link.Source)
		}
	}
	return dangling
}

func RepositoryPath(root, relative string) string {
	return path.Join(root, relative)
}
