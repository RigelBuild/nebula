// SPDX-License-Identifier: MIT OR Apache-2.0

// Woodpecker pipeline reads for the CD gate's decision input.
package cdgate

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"github.com/RigelBuild/nebula/tools/go-lib/woodpecker"
)

// pageSize is the Woodpecker list page size. A short page ends the scan.
const pageSize = 50

// PipelineItem is one row of a pipeline list.
type PipelineItem struct {
	Number    int64
	Commit    string
	Branch    string
	Event     string
	Variables map[string]string
}

// PipelineDetail is one pipeline with its workflow states, keyed by name.
type PipelineDetail struct {
	Number    int64
	Commit    string
	Workflows map[string]string
}

// PipelineAPI reads pipelines from one Woodpecker repo.
type PipelineAPI struct {
	Client *woodpecker.Client
	RepoID int64
}

// Fetch reads one pipeline and its workflow states.
func (p PipelineAPI) Fetch(ctx context.Context, number int64) (PipelineDetail, error) {
	raw, err := p.Client.FetchJSON(ctx, "GET", fmt.Sprintf("/api/repos/%d/pipelines/%d", p.RepoID, number), nil, 0)
	if err != nil {
		return PipelineDetail{}, woodpecker.RejectedCredential(err)
	}
	detail, err := parsePipelineDetail(raw)
	if err != nil {
		return PipelineDetail{}, err
	}
	if detail.Number != number {
		return PipelineDetail{}, errors.New("pipeline number mismatch")
	}
	return detail, nil
}

// MainPipelines lists main's push and manual pipelines, newest first, until a
// pipeline older than ownNumber appears. The own pipeline itself is skipped.
func (p PipelineAPI) MainPipelines(ctx context.Context, ownNumber int64) ([]PipelineItem, error) {
	var out []PipelineItem
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("branch", "main")
		q.Set("event", "push,manual")
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", strconv.Itoa(pageSize))
		raw, err := p.Client.FetchJSON(ctx, "GET", fmt.Sprintf("/api/repos/%d/pipelines?%s", p.RepoID, q.Encode()), nil, 0)
		if err != nil {
			return nil, woodpecker.RejectedCredential(err)
		}
		items, ok := raw.([]any)
		if !ok {
			return nil, errors.New("invalid pipeline list")
		}
		for _, value := range items {
			item, err := parsePipelineItem(value)
			if err != nil {
				return nil, err
			}
			if item.Number < ownNumber {
				return out, nil
			}
			if item.Number == ownNumber {
				continue
			}
			if item.Branch == "main" && (item.Event == "push" || item.Event == "manual") {
				out = append(out, item)
			}
		}
		if len(items) < pageSize {
			return out, nil
		}
	}
}

// IsNewestRelease reports whether own is the newest release: the newest manual
// main pipeline that carries a release dispatch id. It scans newest first and
// stops at the first release, or at the first pipeline at or below own.
func (p PipelineAPI) IsNewestRelease(ctx context.Context, own int64) (bool, error) {
	for page := 1; ; page++ {
		q := url.Values{}
		q.Set("branch", "main")
		q.Set("event", "manual")
		q.Set("page", strconv.Itoa(page))
		q.Set("perPage", strconv.Itoa(pageSize))
		raw, err := p.Client.FetchJSON(ctx, "GET", fmt.Sprintf("/api/repos/%d/pipelines?%s", p.RepoID, q.Encode()), nil, 0)
		if err != nil {
			return false, woodpecker.RejectedCredential(err)
		}
		items, ok := raw.([]any)
		if !ok {
			return false, errors.New("invalid pipeline list")
		}
		for _, value := range items {
			item, err := parsePipelineItem(value)
			if err != nil {
				return false, err
			}
			if item.Variables[DispatchIDVariable] != "" {
				return item.Number == own, nil
			}
			if item.Number <= own {
				return false, nil
			}
		}
		if len(items) < pageSize {
			return false, nil
		}
	}
}

func parsePipelineItem(value any) (PipelineItem, error) {
	obj, ok := value.(map[string]any)
	if !ok {
		return PipelineItem{}, errors.New("invalid pipeline list item")
	}
	number, ok := wholeNumber(obj["number"])
	if !ok {
		return PipelineItem{}, errors.New("invalid pipeline list item")
	}
	commit, ok := obj["commit"].(string)
	if !ok || commit == "" {
		return PipelineItem{}, errors.New("invalid pipeline list item fields")
	}
	branch, ok := obj["branch"].(string)
	if !ok {
		return PipelineItem{}, errors.New("invalid pipeline list item fields")
	}
	event, ok := obj["event"].(string)
	if !ok {
		return PipelineItem{}, errors.New("invalid pipeline list item fields")
	}
	variables, err := woodpecker.PipelineVariables(obj)
	if err != nil {
		return PipelineItem{}, err
	}
	return PipelineItem{Number: number, Commit: commit, Branch: branch, Event: event, Variables: variables}, nil
}

func parsePipelineDetail(value any) (PipelineDetail, error) {
	item, err := parsePipelineItem(value)
	if err != nil {
		return PipelineDetail{}, err
	}
	obj, ok := value.(map[string]any)
	if !ok {
		return PipelineDetail{}, errors.New("invalid pipeline list item")
	}
	workflows := map[string]string{}
	if raw, present := obj["workflows"]; present {
		list, ok := raw.([]any)
		if !ok {
			return PipelineDetail{}, errors.New("missing workflows")
		}
		for _, wv := range list {
			w, ok := wv.(map[string]any)
			if !ok {
				return PipelineDetail{}, errors.New("invalid workflow")
			}
			name, okName := w["name"].(string)
			state, okState := w["state"].(string)
			if !okName || !okState {
				return PipelineDetail{}, errors.New("invalid workflow")
			}
			workflows[name] = state
		}
	}
	return PipelineDetail{Number: item.Number, Commit: item.Commit, Workflows: workflows}, nil
}

// wholeNumber accepts a JSON number that is a whole integer in [1, 2^53-1],
// the range TS parsePipelineListItem allows for a pipeline number.
func wholeNumber(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) || f < 1 || f > 1<<53-1 {
		return 0, false
	}
	return int64(f), true
}
