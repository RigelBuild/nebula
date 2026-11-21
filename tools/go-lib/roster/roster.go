// SPDX-License-Identifier: MIT OR Apache-2.0

package roster

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// MaxAge is the maximum accepted age of a generated roster.
const MaxAge = 5 * time.Minute

var (
	errInvalidRosterSchema = errors.New("invalid roster schema")
	errInvalidRosterAgent  = errors.New("invalid roster agent")
	errStaleRoster         = errors.New("stale roster")
	errDigestMismatch      = errors.New("roster digest mismatch")
)

// Agent retains its source object so fields unknown to this package survive.
type Agent struct {
	Name string         `json:"name"`
	Raw  map[string]any `json:"-"`
}

// File is a parsed roster document.
type File struct {
	SchemaVersion  string  `json:"schemaVersion"`
	SourceRevision string  `json:"sourceRevision"`
	GeneratedAt    string  `json:"generatedAt"`
	Provenance     string  `json:"provenance"`
	Agents         []Agent `json:"agents"`
	Digest         string  `json:"digest,omitempty"`
}

// Snapshot holds a validated roster and its computed digest.
type Snapshot struct {
	Roster File
	Digest string
	Path   string
}

// Path returns the configured roster path or the user's state-file path.
func Path(getenv func(string) string) string {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	if path := getenv("ROSTER_PATH"); path != "" {
		return path
	}
	home := getenv("HOME")
	if home == "" {
		home = "~"
	}
	return home + "/agents/state/roster.json"
}

// Names returns agent names in the order stored in the roster.
func (s Snapshot) Names() []string {
	names := make([]string, 0, len(s.Roster.Agents))
	for _, agent := range s.Roster.Agents {
		names = append(names, agent.Name)
	}
	return names
}

// Digest computes the JavaScript-compatible canonical JSON SHA-256 digest.
func Digest(value any) (string, error) {
	var canonical strings.Builder
	if err := writeCanonical(&canonical, value, true); err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(canonical.String()))
	return hex.EncodeToString(digest[:]), nil
}

// Load reads and validates a roster against the supplied current time.
func Load(ctx context.Context, path string, now time.Time) (Snapshot, error) {
	if ctx == nil {
		return Snapshot{}, errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("load roster: %w", err)
	}
	//nolint:gosec // The caller supplies the configured roster file path.
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, fmt.Errorf("read roster %q: %w", path, err)
	}
	if err := ctx.Err(); err != nil {
		return Snapshot{}, fmt.Errorf("load roster: %w", err)
	}
	value, err := decodeJSON(data)
	if err != nil {
		return Snapshot{}, fmt.Errorf("parse roster JSON: %w", err)
	}
	file, err := parseRoster(value)
	if err != nil {
		return Snapshot{}, err
	}
	generated, err := parseGeneratedAt(file.GeneratedAt)
	if err != nil {
		return Snapshot{}, errStaleRoster
	}
	if now.Sub(generated) > MaxAge || generated.After(now.Add(30*time.Second)) {
		return Snapshot{}, errStaleRoster
	}
	digest, err := Digest(value)
	if err != nil {
		return Snapshot{}, fmt.Errorf("digest roster: %w", err)
	}
	object, _ := value.(map[string]any)
	if declaredDigest, ok := object["digest"].(string); ok && declaredDigest != digest {
		return Snapshot{}, errDigestMismatch
	}
	return Snapshot{Roster: file, Digest: digest, Path: path}, nil
}

func parseGeneratedAt(value string) (time.Time, error) {
	switch len(value) {
	case 4:
		if !allDigits(value) {
			return time.Time{}, errInvalidDateTime
		}
		return time.ParseInLocation("2006", value, time.UTC)
	case 7:
		if value[4] != '-' || !allDigits(value[:4]) || !allDigits(value[5:]) {
			return time.Time{}, errInvalidDateTime
		}
		return time.ParseInLocation("2006-01", value, time.UTC)
	case 10:
		if value[4] != '-' || value[7] != '-' || !allDigits(value[:4]) || !allDigits(value[5:7]) || !allDigits(value[8:]) {
			return time.Time{}, errInvalidDateTime
		}
		return time.ParseInLocation("2006-01-02", value, time.UTC)
	}

	dateTime, zoneLayout, zoneLength, err := splitGeneratedDateTime(value)
	if err != nil {
		return time.Time{}, err
	}
	layout, err := generatedDateTimeLayout(dateTime)
	if err != nil {
		return time.Time{}, err
	}
	layout += zoneLayout
	if zoneLength > 0 {
		return time.Parse(layout, value)
	}
	//nolint:gosmopolitan // The ECMAScript date-time format uses the process local timezone when no offset is present.
	return time.ParseInLocation(layout, value, time.Local)
}
func splitGeneratedDateTime(value string) (string, string, int, error) {
	zoneLayout := ""
	zoneLength := 0
	if strings.HasSuffix(value, "Z") {
		zoneLayout = "Z07:00"
		zoneLength = 1
	} else if len(value) >= 6 {
		zoneStart := len(value) - 6
		if (value[zoneStart] == '+' || value[zoneStart] == '-') && value[zoneStart+3] == ':' {
			zoneLayout = "Z07:00"
			zoneLength = 6
		}
	}
	dateTimeLength := len(value) - zoneLength
	if len(value) < 16 || dateTimeLength < 16 || !validDateTimePrefix(value) {
		return "", "", 0, errInvalidDateTime
	}
	return value[:dateTimeLength], zoneLayout, zoneLength, nil
}
func validDateTimePrefix(value string) bool {
	return value[4] == '-' &&
		value[7] == '-' &&
		value[10] == 'T' &&
		value[13] == ':' &&
		allDigits(value[:4]) &&
		allDigits(value[5:7]) &&
		allDigits(value[8:10]) &&
		allDigits(value[11:13]) &&
		allDigits(value[14:16])
}

func generatedDateTimeLayout(value string) (string, error) {
	switch len(value) {
	case 16:
		return "2006-01-02T15:04", nil
	case 19:
		return "2006-01-02T15:04:05", nil
	case 23:
		if value[19] != '.' || !allDigits(value[20:]) {
			return "", errInvalidDateTime
		}
		return "2006-01-02T15:04:05.000", nil
	default:
		return "", errInvalidDateTime
	}
}

var errInvalidDateTime = errors.New("invalid date-time")

func allDigits(value string) bool {
	for _, digit := range value {
		if digit < '0' || digit > '9' {
			return false
		}
	}
	return true
}

func parseRoster(value any) (File, error) {
	object, ok := value.(map[string]any)
	if !ok {
		return File{}, errInvalidRosterSchema
	}
	schemaVersion, schemaVersionOK := object["schemaVersion"].(string)
	sourceRevision, sourceRevisionOK := object["sourceRevision"].(string)
	generatedAt, generatedAtOK := object["generatedAt"].(string)
	provenance, provenanceOK := object["provenance"].(string)
	agentsValue, agentsOK := object["agents"].([]any)
	if !schemaVersionOK || !sourceRevisionOK || !generatedAtOK || !provenanceOK || !agentsOK {
		return File{}, errInvalidRosterSchema
	}

	agents := make([]Agent, 0, len(agentsValue))
	for _, value := range agentsValue {
		agentObject, ok := value.(map[string]any)
		if !ok {
			return File{}, errInvalidRosterAgent
		}
		name, ok := agentObject["name"].(string)
		if !ok || !validName(name) {
			return File{}, errInvalidRosterAgent
		}
		agents = append(agents, Agent{Name: name, Raw: agentObject})
	}

	file := File{
		SchemaVersion:  schemaVersion,
		SourceRevision: sourceRevision,
		GeneratedAt:    generatedAt,
		Provenance:     provenance,
		Agents:         agents,
	}
	if digest, ok := object["digest"].(string); ok {
		file.Digest = digest
	}
	return file, nil
}

func validName(name string) bool {
	if len(name) == 0 || name[0] < 'a' || name[0] > 'z' {
		return false
	}
	for i := 1; i < len(name); i++ {
		c := name[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return false
		}
	}
	return true
}

func writeCanonical(out *strings.Builder, value any, topLevel bool) error {
	switch value := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		out.WriteString(strconv.FormatBool(value))
	case string:
		writeJSONString(out, value)
	case json.Number:
		return writeNumber(out, string(value))
	case float32:
		return writeFloat(out, float64(value))
	case float64:
		return writeFloat(out, value)
	case int, int8, int16, int32, int64:
		return writeSignedInteger(out, value)
	case uint, uint8, uint16, uint32, uint64:
		return writeUnsignedInteger(out, value)
	case json.RawMessage:
		parsed, err := decodeJSON(value)
		if err != nil {
			return fmt.Errorf("parse raw JSON value: %w", err)
		}
		return writeCanonical(out, parsed, topLevel)
	case Agent:
		return writeCanonical(out, value.Raw, topLevel)
	case []Agent:
		out.WriteByte('[')
		for i, agent := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, agent.Raw, false); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case File:
		object := map[string]any{
			"schemaVersion":  value.SchemaVersion,
			"sourceRevision": value.SourceRevision,
			"generatedAt":    value.GeneratedAt,
			"provenance":     value.Provenance,
			"agents":         value.Agents,
		}
		if value.Digest != "" {
			object["digest"] = value.Digest
		}
		return writeCanonical(out, object, topLevel)
	case map[string]any:
		return writeObject(out, value, topLevel)
	case []any:
		out.WriteByte('[')
		for i, child := range value {
			if i > 0 {
				out.WriteByte(',')
			}
			if err := writeCanonical(out, child, false); err != nil {
				return err
			}
		}
		out.WriteByte(']')

	default:
		return marshalCanonical(out, value, topLevel)
	}
	return nil
}

func writeSignedInteger(out *strings.Builder, value any) error {
	switch value := value.(type) {
	case int:
		return writeFloat(out, float64(value))
	case int8:
		return writeFloat(out, float64(value))
	case int16:
		return writeFloat(out, float64(value))
	case int32:
		return writeFloat(out, float64(value))
	case int64:
		return writeFloat(out, float64(value))
	default:
		return errors.New("unsupported signed integer")
	}
}

func writeUnsignedInteger(out *strings.Builder, value any) error {
	switch value := value.(type) {
	case uint:
		return writeFloat(out, float64(value))
	case uint8:
		return writeFloat(out, float64(value))
	case uint16:
		return writeFloat(out, float64(value))
	case uint32:
		return writeFloat(out, float64(value))
	case uint64:
		return writeFloat(out, float64(value))
	default:
		return errors.New("unsupported unsigned integer")
	}
}
func marshalCanonical(out *strings.Builder, value any, topLevel bool) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Errorf("encode JSON value: %w", err)
	}
	parsed, err := decodeJSON(encoded)
	if err != nil {
		return fmt.Errorf("decode JSON value: %w", err)
	}
	return writeCanonical(out, parsed, topLevel)
}
func writeObject(out *strings.Builder, object map[string]any, topLevel bool) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		if !topLevel || key != "digest" {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return lessUTF16(keys[i], keys[j]) })

	out.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			out.WriteByte(',')
		}
		writeJSONString(out, key)
		out.WriteByte(':')
		if err := writeCanonical(out, object[key], false); err != nil {
			return err
		}
	}
	out.WriteByte('}')
	return nil
}

func lessUTF16(left, right string) bool {
	leftOffset, rightOffset := 0, 0
	var leftLow, rightLow int32
	for {
		leftHasUnit := leftOffset < len(left) || leftLow != 0
		rightHasUnit := rightOffset < len(right) || rightLow != 0
		if !leftHasUnit || !rightHasUnit {
			if leftHasUnit != rightHasUnit {
				return !leftHasUnit
			}
			return left < right
		}
		leftUnit, leftSize := nextUTF16Unit(left, leftOffset, &leftLow)
		rightUnit, rightSize := nextUTF16Unit(right, rightOffset, &rightLow)
		if leftUnit != rightUnit {
			return leftUnit < rightUnit
		}
		leftOffset += leftSize
		rightOffset += rightSize
	}
}

func nextUTF16Unit(value string, offset int, pendingLow *int32) (int32, int) {
	if *pendingLow != 0 {
		unit := *pendingLow
		*pendingLow = 0
		return unit, 0
	}
	if surrogate, size, ok := encodedSurrogate(value[offset:]); ok {
		return int32(surrogate), size
	}
	r, size := utf8.DecodeRuneInString(value[offset:])
	if r <= 0xffff {
		return r, size
	}
	r -= 0x10000
	*pendingLow = (r & 0x3ff) + 0xdc00
	return (r >> 10) + 0xd800, size
}

func writeNumber(out *strings.Builder, number string) error {
	value, err := strconv.ParseFloat(number, 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return fmt.Errorf("parse JSON number %q: %w", number, err)
	}
	if math.IsInf(value, 0) {
		return errors.New("non-finite number")
	}
	return writeFloat(out, value)
}

func writeFloat(out *strings.Builder, value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return errors.New("non-finite number")
	}
	if value == 0 {
		out.WriteByte('0')
		return nil
	}
	magnitude := math.Abs(value)
	if magnitude >= 1e-6 && magnitude < 1e21 {
		out.WriteString(strconv.FormatFloat(value, 'f', -1, 64))
		return nil
	}
	formatted := strconv.FormatFloat(value, 'e', -1, 64)
	separator := strings.LastIndexByte(formatted, 'e')
	mantissa := formatted[:separator]
	exponent, err := strconv.Atoi(formatted[separator+1:])
	if err != nil {
		return fmt.Errorf("format JSON number %q: %w", formatted, err)
	}
	out.WriteString(mantissa)
	out.WriteByte('e')
	if exponent >= 0 {
		out.WriteByte('+')
	}
	out.WriteString(strconv.Itoa(exponent))
	return nil
}

func writeJSONString(out *strings.Builder, value string) {
	out.WriteByte('"')
	for i := 0; i < len(value); {
		if surrogate, size, ok := encodedSurrogate(value[i:]); ok {
			writeUnicodeEscape(out, surrogate)
			i += size
			continue
		}
		r, size := utf8.DecodeRuneInString(value[i:])
		if size == 1 && r == utf8.RuneError {
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		switch r {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\b':
			out.WriteString(`\b`)
		case '\f':
			out.WriteString(`\f`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		default:
			if r < 0x20 {
				writeUnicodeEscape(out, uint16(r&0xffff))
			} else {
				out.WriteString(value[i : i+size])
			}
		}
		i += size
	}
	out.WriteByte('"')
}

func writeUnicodeEscape(out *strings.Builder, value uint16) {
	const hex = "0123456789abcdef"
	out.WriteString(`\u`)
	out.WriteByte(hex[(value>>12)&0xf])
	out.WriteByte(hex[(value>>8)&0xf])
	out.WriteByte(hex[(value>>4)&0xf])
	out.WriteByte(hex[value&0xf])
}

func encodedSurrogate(value string) (uint16, int, bool) {
	if len(value) < 3 || value[0] != 0xed || value[1] < 0xa0 || value[1] > 0xbf || value[2] < 0x80 || value[2] > 0xbf {
		return 0, 0, false
	}
	unit := uint16(value[0]&0x0f)<<12 | uint16(value[1]&0x3f)<<6 | uint16(value[2]&0x3f)
	return unit, 3, true
}

type jsonParser struct {
	data   []byte
	offset int
}

func (p *jsonParser) parseValue() (any, error) {
	p.skipWhitespace()
	if p.offset == len(p.data) {
		return nil, errors.New("unexpected end of JSON")
	}
	switch p.data[p.offset] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	case '"':
		return p.parseString()
	case 't':
		return p.parseLiteral("true", true)
	case 'f':
		return p.parseLiteral("false", false)
	case 'n':
		return p.parseLiteral("null", nil)
	default:
		if p.data[p.offset] == '-' || isDigit(p.data[p.offset]) {
			return p.parseNumber()
		}
		return nil, fmt.Errorf("unexpected JSON byte at offset %d", p.offset)
	}
}

func (p *jsonParser) parseObject() (map[string]any, error) {
	p.offset++
	object := make(map[string]any)
	p.skipWhitespace()
	if p.consume('}') {
		return object, nil
	}
	for {
		p.skipWhitespace()
		if p.offset == len(p.data) || p.data[p.offset] != '"' {
			return nil, fmt.Errorf("expected JSON object key at offset %d", p.offset)
		}
		key, err := p.parseString()
		if err != nil {
			return nil, err
		}
		p.skipWhitespace()
		if !p.consume(':') {
			return nil, fmt.Errorf("expected colon at offset %d", p.offset)
		}
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		object[key] = value
		p.skipWhitespace()
		if p.consume('}') {
			return object, nil
		}
		if !p.consume(',') {
			return nil, fmt.Errorf("expected comma at offset %d", p.offset)
		}
	}
}

func (p *jsonParser) parseArray() ([]any, error) {
	p.offset++
	values := make([]any, 0)
	p.skipWhitespace()
	if p.consume(']') {
		return values, nil
	}
	for {
		value, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		values = append(values, value)
		p.skipWhitespace()
		if p.consume(']') {
			return values, nil
		}
		if !p.consume(',') {
			return nil, fmt.Errorf("expected comma at offset %d", p.offset)
		}
	}
}

func (p *jsonParser) parseString() (string, error) {
	p.offset++
	var value strings.Builder
	for p.offset < len(p.data) {
		current := p.data[p.offset]
		p.offset++
		switch {
		case current == '"':
			return value.String(), nil
		case current == '\\':
			if p.offset == len(p.data) {
				return "", errors.New("incomplete JSON escape")
			}
			escape := p.data[p.offset]
			p.offset++
			switch escape {
			case '"', '\\', '/':
				value.WriteByte(escape)
			case 'b':
				value.WriteByte('\b')
			case 'f':
				value.WriteByte('\f')
			case 'n':
				value.WriteByte('\n')
			case 'r':
				value.WriteByte('\r')
			case 't':
				value.WriteByte('\t')
			case 'u':
				unit, err := p.parseHexUnit()
				if err != nil {
					return "", err
				}
				switch {
				case unit >= 0xd800 && unit <= 0xdbff && p.nextLowSurrogateEscape():
					p.offset += 2
					low, err := p.parseHexUnit()
					if err != nil {
						return "", err
					}
					value.WriteRune(0x10000 + (rune(unit-0xd800) << 10) + rune(low-0xdc00))
				case unit >= 0xd800 && unit <= 0xdfff:
					writeSurrogate(&value, unit)
				default:
					value.WriteRune(rune(unit))
				}
			default:
				return "", fmt.Errorf("invalid JSON escape at offset %d", p.offset-1)
			}
		case current < 0x20:
			return "", fmt.Errorf("unescaped control character at offset %d", p.offset-1)
		default:
			p.offset--
			r, size := utf8.DecodeRune(p.data[p.offset:])
			if r == utf8.RuneError && size == 1 {
				return "", fmt.Errorf("invalid UTF-8 at offset %d", p.offset)
			}
			value.Write(p.data[p.offset : p.offset+size])
			p.offset += size
		}
	}
	return "", errors.New("unterminated JSON string")
}

func (p *jsonParser) parseHexUnit() (uint16, error) {
	if len(p.data)-p.offset < 4 {
		return 0, errors.New("incomplete Unicode escape")
	}
	var value uint16
	for _, digit := range p.data[p.offset : p.offset+4] {
		value <<= 4
		switch {
		case digit >= '0' && digit <= '9':
			value += uint16(digit - '0')
		case digit >= 'a' && digit <= 'f':
			value += uint16(digit-'a') + 10
		case digit >= 'A' && digit <= 'F':
			value += uint16(digit-'A') + 10
		default:
			return 0, fmt.Errorf("invalid Unicode escape at offset %d", p.offset)
		}
	}
	p.offset += 4
	return value, nil
}

func (p *jsonParser) nextLowSurrogateEscape() bool {
	if len(p.data)-p.offset < 6 || p.data[p.offset] != '\\' || p.data[p.offset+1] != 'u' {
		return false
	}
	for _, digit := range p.data[p.offset+2 : p.offset+6] {
		if !isHex(digit) {
			return false
		}
	}
	low, err := strconv.ParseUint(string(p.data[p.offset+2:p.offset+6]), 16, 16)
	return err == nil && low >= 0xdc00 && low <= 0xdfff
}

func writeSurrogate(out *strings.Builder, value uint16) {
	out.WriteByte(0xed)
	out.WriteByte(0xa0 + byte((value-0xd800)>>6))
	out.WriteByte(0x80 + byte(value&0x3f))
}

func (p *jsonParser) parseNumber() (json.Number, error) {
	start := p.offset
	p.consume('-')
	if p.consume('0') {
		if p.offset < len(p.data) && isDigit(p.data[p.offset]) {
			return "", fmt.Errorf("leading zero in JSON number at offset %d", p.offset)
		}
	} else {
		if p.offset == len(p.data) || p.data[p.offset] < '1' || p.data[p.offset] > '9' {
			return "", fmt.Errorf("invalid JSON number at offset %d", p.offset)
		}
		p.offset++
		p.consumeDigits()
	}
	if p.consume('.') && !p.consumeDigits() {
		return "", fmt.Errorf("invalid JSON fraction at offset %d", p.offset)
	}
	if p.consume('e') || p.consume('E') {
		if !p.consume('+') {
			p.consume('-')
		}
		if !p.consumeDigits() {
			return "", fmt.Errorf("invalid JSON exponent at offset %d", p.offset)
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(p.data[start:p.offset]))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return "", fmt.Errorf("decode JSON number: %w", err)
	}
	number, ok := value.(json.Number)
	if !ok {
		return "", errors.New("decoded JSON number has an unexpected type")
	}
	return number, nil
}

func (p *jsonParser) consumeDigits() bool {
	start := p.offset
	for p.offset < len(p.data) && isDigit(p.data[p.offset]) {
		p.offset++
	}
	return p.offset > start
}

func (p *jsonParser) parseLiteral(literal string, value any) (any, error) {
	if !bytes.HasPrefix(p.data[p.offset:], []byte(literal)) {
		return nil, fmt.Errorf("invalid JSON literal at offset %d", p.offset)
	}
	p.offset += len(literal)
	return value, nil
}

func (p *jsonParser) skipWhitespace() {
	for p.offset < len(p.data) {
		switch p.data[p.offset] {
		case ' ', '\t', '\n', '\r':
			p.offset++
		default:
			return
		}
	}
}

func (p *jsonParser) consume(expected byte) bool {
	if p.offset < len(p.data) && p.data[p.offset] == expected {
		p.offset++
		return true
	}
	return false
}

func isDigit(value byte) bool {
	return value >= '0' && value <= '9'
}

func isHex(value byte) bool {
	return isDigit(value) || value >= 'a' && value <= 'f' || value >= 'A' && value <= 'F'
}

func decodeJSON(data []byte) (any, error) {
	parser := jsonParser{data: data}
	value, err := parser.parseValue()
	if err != nil {
		return nil, err
	}
	parser.skipWhitespace()
	if parser.offset != len(data) {
		return nil, errors.New("unexpected trailing JSON data")
	}
	return value, nil
}
