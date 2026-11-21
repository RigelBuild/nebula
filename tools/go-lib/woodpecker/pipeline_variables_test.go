package woodpecker

import "testing"

func TestPipelineVariablesReadsStringValues(t *testing.T) {
	got, err := PipelineVariables(map[string]any{
		"variables": map[string]any{"PETREL_CD_DISPATCH_ID": "d1"},
	})
	if err != nil {
		t.Fatalf("PipelineVariables: %v", err)
	}
	if got["PETREL_CD_DISPATCH_ID"] != "d1" {
		t.Fatalf("variables = %v, want the dispatch id", got)
	}
}

func TestPipelineVariablesAbsentIsEmpty(t *testing.T) {
	got, err := PipelineVariables(map[string]any{})
	if err != nil {
		t.Fatalf("PipelineVariables: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("variables = %v, want none", got)
	}
}

func TestPipelineVariablesRejectsNonStringValue(t *testing.T) {
	if _, err := PipelineVariables(map[string]any{"variables": map[string]any{"X": 1.0}}); err == nil {
		t.Fatal("PipelineVariables accepted a non-string value")
	}
}
