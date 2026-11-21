package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"

	"github.com/klauspost/compress/zstd"
)

type DriverKey struct {
	Agent  string `json:"agent"`
	Source string `json:"source"`
}
type Driver struct {
	Key   DriverKey `json:"key"`
	Bytes int64     `json:"bytes"`
	// Share divides this source's bytes by content plus system bytes for the agent.
	Share          float64 `json:"share"`
	EstInputTokens int64   `json:"est_input_tokens"`
}

type SystemDriver struct {
	Agent string `json:"agent"`
	Bytes int64  `json:"bytes"`
	// Share uses the same content-plus-system byte total as Driver.Share.
	Share          float64 `json:"share"`
	EstInputTokens int64   `json:"est_input_tokens"`
	Rows           int     `json:"rows"`
}

type DigestShape struct {
	Length int    `json:"length"`
	Class  string `json:"class"`
	Rows   int    `json:"rows"`
}

type PromptCounts struct {
	NoDigest            int                 `json:"no_digest"`
	BadDigest           int                 `json:"bad_digest"`
	BadDigestShapes     []DigestShape       `json:"bad_digest_shapes"`
	MissingPromptFile   int                 `json:"missing_prompt_file"`
	BadPromptFile       int                 `json:"bad_prompt_file"`
	InvalidAgentRows    int                 `json:"invalid_agent_rows"`
	ConflictingSessions int                 `json:"conflicting_sessions"`
	ConflictingRows     int                 `json:"conflicting_rows"`
	Unattributed        []UnattributedAgent `json:"unattributed"`
}

type UnattributedAgent struct {
	Agent       string `json:"agent"`
	InputTokens int64  `json:"input_tokens"`
}

type driverTotals struct {
	sessions            map[string]string
	inputs              map[string]int64
	bytes               map[DriverKey]int64
	systems             map[string]SystemDriver
	invalidAgentRows    int
	conflictingSessions map[string]struct{}
	conflictingRows     int
}

func newDriverTotals(rows []Row) *driverTotals {
	d := &driverTotals{
		sessions:            make(map[string]string),
		inputs:              make(map[string]int64),
		bytes:               make(map[DriverKey]int64),
		systems:             make(map[string]SystemDriver),
		conflictingSessions: make(map[string]struct{}),
	}
	for _, row := range rows {
		if row.Kind != kindMessage && row.Kind != kindModelUsage {
			continue
		}
		if !usageDriverRow(row) {
			d.invalidAgentRows++
			continue
		}
		if agent, ok := d.sessions[row.SessionID]; ok && agent != *row.Agent {
			d.conflictingSessions[row.SessionID] = struct{}{}
			d.conflictingRows++
			continue
		}
		d.sessions[row.SessionID] = *row.Agent
		d.inputs[*row.Agent] += int64Value(row.Input)
	}
	return d
}

func Drivers(rows []Row, dir string, content func(func(ContentRow) error) error) ([]Driver, []SystemDriver, PromptCounts, error) {
	d := newDriverTotals(rows)
	counts := d.addPrompts(dir, rows)
	counts.InvalidAgentRows = d.invalidAgentRows
	counts.ConflictingSessions = len(d.conflictingSessions)
	counts.ConflictingRows = d.conflictingRows
	if err := content(d.addContent); err != nil {
		return nil, nil, counts, err
	}
	drivers := d.result()
	systems := d.systemResults()
	for agent, input := range d.inputs {
		if input != 0 && d.totalBytes(agent) == 0 {
			counts.Unattributed = append(counts.Unattributed, UnattributedAgent{Agent: agent, InputTokens: input})
		}
	}
	sort.Slice(counts.Unattributed, func(i, j int) bool { return counts.Unattributed[i].Agent < counts.Unattributed[j].Agent })
	return drivers, systems, counts, nil
}

func usageDriverRow(row Row) bool {
	return (row.Kind == kindMessage || row.Kind == kindModelUsage) && row.Agent != nil && toolLabel.MatchString(*row.Agent)
}

var toolLabel = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func (d *driverTotals) addContent(row ContentRow) error {
	agent, ok := d.sessions[row.SessionID]
	if !ok {
		return nil
	}
	type attribution struct {
		key  DriverKey
		size int64
	}
	var additions []attribution
	for _, raw := range row.Parts {
		var part struct {
			Type string          `json:"type"`
			Text string          `json:"text"`
			Args json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(raw, &part); err != nil {
			return fmt.Errorf("%w: decoding content part", errBadContentRow)
		}
		source, size := contentSource(row, part.Type, part.Text, part.Args)
		if source != "" && size > 0 {
			additions = append(additions, attribution{key: DriverKey{Agent: agent, Source: source}, size: size})
		}
	}
	for _, addition := range additions {
		d.bytes[addition.key] += addition.size
	}
	return nil
}

func contentSource(row ContentRow, kind, text string, args json.RawMessage) (string, int64) {
	if row.Role == "toolResult" {
		name := "unknown"
		if row.ToolName != nil && toolLabel.MatchString(*row.ToolName) {
			name = *row.ToolName
		}
		return "tool_result:" + name, int64(len(text))
	}
	if row.Role == "compaction" {
		return "compaction", int64(len(text))
	}
	if row.Role == "user" {
		return "user", int64(len(text))
	}
	if row.Role != "assistant" {
		return "", 0
	}
	switch kind {
	case "text":
		return "assistant_text", int64(len(text))
	case "thinking":
		return "thinking", int64(len(text))
	case "toolCall":
		return "tool_call", int64(len(args))
	default:
		return "", 0
	}
}

func (d *driverTotals) result() []Driver {
	totals := make(map[string]int64)
	for key, size := range d.bytes {
		totals[key.Agent] += size
	}
	for agent, system := range d.systems {
		totals[agent] += system.Bytes
	}
	result := make([]Driver, 0, len(d.bytes))
	for key, size := range d.bytes {
		var share float64
		if totals[key.Agent] > 0 {
			share = float64(size) / float64(totals[key.Agent])
		}
		tokens := int64(float64(d.inputs[key.Agent]) * share)
		result = append(result, Driver{Key: key, Bytes: size, Share: share, EstInputTokens: tokens})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Key.Agent != result[j].Key.Agent {
			return result[i].Key.Agent < result[j].Key.Agent
		}
		return result[i].Key.Source < result[j].Key.Source
	})
	return result
}

func (d *driverTotals) totalBytes(agent string) int64 {
	total := d.systems[agent].Bytes
	for key, size := range d.bytes {
		if key.Agent == agent {
			total += size
		}
	}
	return total
}

func (d *driverTotals) systemResults() []SystemDriver {
	systems := make([]SystemDriver, 0, len(d.systems))
	for agent, system := range d.systems {
		if total := d.totalBytes(agent); total > 0 {
			system.Share = float64(system.Bytes) / float64(total)
			system.EstInputTokens = int64(float64(d.inputs[agent]) * system.Share)
		}
		systems = append(systems, system)
	}
	sort.Slice(systems, func(i, j int) bool { return systems[i].Agent < systems[j].Agent })
	return systems
}

// Each usage row contributes its prompt size; digest identity is only a cache key.
func (d *driverTotals) addPrompts(dir string, rows []Row) PromptCounts {
	type promptSize struct {
		size    int64
		missing bool
		bad     bool
	}
	cache := make(map[string]promptSize)
	shapes := make(map[DigestShape]int)
	counts := PromptCounts{}
	for _, row := range rows {
		if !usageDriverRow(row) {
			continue
		}
		if agent, ok := d.sessions[row.SessionID]; !ok || agent != *row.Agent {
			continue
		}
		digestPtr := row.PromptArchiveDigest
		if digestPtr == nil || *digestPtr == "" {
			digestPtr = row.PromptDigest
		}
		if digestPtr == nil || *digestPtr == "" {
			counts.NoDigest++
			continue
		}
		digest := *digestPtr
		if !promptDigest.MatchString(digest) {
			counts.BadDigest++
			shape := digestShape(digest)
			shapes[shape]++
			continue
		}
		size, ok := cache[digest]
		if !ok {
			var err error
			size.size, size.missing, err = readPromptSize(dir, digest)
			size.bad = err != nil
			cache[digest] = size
		}
		if size.bad {
			counts.BadPromptFile++
			continue
		}
		if size.missing {
			counts.MissingPromptFile++
			continue
		}
		system := d.systems[*row.Agent]
		system.Agent = *row.Agent
		system.Bytes += size.size
		system.Rows++
		d.systems[*row.Agent] = system
	}
	for shape, count := range shapes {
		shape.Rows = count
		counts.BadDigestShapes = append(counts.BadDigestShapes, shape)
	}
	sort.Slice(counts.BadDigestShapes, func(i, j int) bool {
		a, b := counts.BadDigestShapes[i], counts.BadDigestShapes[j]
		if a.Rows != b.Rows {
			return a.Rows > b.Rows
		}
		if a.Length != b.Length {
			return a.Length < b.Length
		}
		return a.Class < b.Class
	})
	if len(counts.BadDigestShapes) > 3 {
		counts.BadDigestShapes = counts.BadDigestShapes[:3]
	}
	return counts
}

var promptDigest = regexp.MustCompile(`^[a-f0-9]{1,16}$`)

func digestShape(digest string) DigestShape {
	prefix := "nonhex"
	if len(digest) > 0 && ((digest[0] >= '0' && digest[0] <= '9') || (digest[0] >= 'a' && digest[0] <= 'f')) {
		prefix = "hex"
	}
	return DigestShape{Length: len(digest), Class: prefix}
}

func readPromptSize(dir, digest string) (size int64, missing bool, resultErr error) {
	file, err := os.Open(filepath.Join(dir, "content", "prompts", digest+".txt.zst")) //nolint:gosec // digest is validated
	if os.IsNotExist(err) {
		return 0, true, nil
	}
	if err != nil {
		return 0, false, fmt.Errorf("opening prompt: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	decoder, err := zstd.NewReader(file, zstd.WithDecoderConcurrency(1))
	if err != nil {
		return 0, false, fmt.Errorf("opening prompt decoder: %w", err)
	}
	defer decoder.Close()
	size, err = io.Copy(io.Discard, decoder)
	if err != nil {
		return 0, false, fmt.Errorf("reading prompt: %w", err)
	}
	return size, false, nil
}
