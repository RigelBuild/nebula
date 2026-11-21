package cdgate

import (
	"encoding/json"
	"testing"
)

// decodeAny decodes a JSON document into the generic shape the API reader gets.
func decodeAny(text string) (any, error) {
	var v any
	err := json.Unmarshal([]byte(text), &v)
	return v, err
}

// pipelineDetailWithWorkflows is a detail body with the given workflows
// JSON array text.
func pipelineDetailWithWorkflows(workflows string) string {
	return `{"number":12,"commit":"` + shaA + `","branch":"main","event":"push","workflows":` + workflows + `}`
}

func TestParsePipelineDetailRejectsNonListWorkflows(t *testing.T) {
	raw, err := decodeAny(`{"number":12,"commit":"` + shaA + `","branch":"main","event":"push","workflows":{"name":"a"}}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePipelineDetail(raw); err == nil {
		t.Fatal("parsePipelineDetail accepted a non-list workflows value, want an error")
	}
}

func TestParsePipelineDetailRejectsNonObjectWorkflow(t *testing.T) {
	raw, err := decodeAny(pipelineDetailWithWorkflows(`["a"]`))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parsePipelineDetail(raw); err == nil {
		t.Fatal("parsePipelineDetail accepted a non-object workflow, want an error")
	}
}
