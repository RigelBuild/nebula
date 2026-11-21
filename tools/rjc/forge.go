package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/samber/oops"
)

type Metadata struct {
	Title, Body, HeadRef, HeadSHA, BaseRef, BaseSHA string
	Labels                                          []string
	Draft                                           bool
}
type Published struct {
	Title    string `json:"title"`
	Authored string `json:"authored"`
}
type BaselineStore interface {
	Load(bookmark string) (Published, bool, error)
	Commit(bookmark string, published Published) error
}
type services struct {
	forge    *forgeClient
	store    BaselineStore
	bookmark string
	jj       *repository
	engine   JJVine
}
type servicesKey struct{}

func withServices(ctx context.Context, s *services) context.Context {
	return context.WithValue(ctx, servicesKey{}, s)
}
func service(ctx context.Context) (*services, error) {
	s, ok := ctx.Value(servicesKey{}).(*services)
	if !ok {
		return nil, oops.Errorf("rjc runtime is not configured")
	}
	return s, nil
}

type forgeClient struct {
	baseURL string
	client  *http.Client
	repo    *repository
	token   string
}

func (f *forgeClient) api(ctx context.Context, method, path string, payload any, out any) error {
	var data []byte
	var err error
	if payload != nil {
		data, err = json.Marshal(payload)
		if err != nil {
			return oops.Wrapf(err, "encode forge payload")
		}
	}
	var response []byte
	if f.client != nil {
		request, requestErr := http.NewRequestWithContext(ctx, method, f.baseURL+path, bytes.NewReader(data))
		if requestErr != nil {
			return oops.Wrapf(requestErr, "create forge request")
		}
		request.Header.Set("Content-Type", "application/json")
		res, requestErr := f.client.Do(request)
		if requestErr != nil {
			return oops.Wrapf(requestErr, "call forge")
		}
		response, err = io.ReadAll(res.Body)
		closeErr := res.Body.Close()
		if err != nil || closeErr != nil {
			return oops.Wrapf(errors.Join(err, closeErr), "read forge response")
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return forgeFailure(res.StatusCode, response, nil)
		}
	} else {
		args := []string{"api", "--include", "--method", method, "-H", "Accept: application/vnd.github+json", strings.TrimPrefix(path, "/")}
		if payload != nil {
			args = append(args, "--input", "-")
		}
		response, err = f.ghAPI(ctx, args, data)
		if err != nil {
			return oops.Wrapf(err, "call GitHub API")
		}
	}
	if out != nil && len(bytes.TrimSpace(response)) > 0 {
		if err := json.Unmarshal(response, out); err != nil {
			return oops.Wrapf(err, "decode forge response")
		}
	}
	return nil
}

func prPath(repo string, pr int) string { return fmt.Sprintf("/repos/%s/pulls/%d", repo, pr) }
func (f *forgeClient) metadata(ctx context.Context, repo string, pr int) (Metadata, error) {
	var raw struct {
		Title, Body string
		Draft       bool
		Head, Base  struct{ Ref, SHA string }
		Labels      []struct{ Name string }
	}
	if err := f.api(ctx, http.MethodGet, prPath(repo, pr), nil, &raw); err != nil {
		return Metadata{}, err
	}
	labels := make([]string, 0, len(raw.Labels))
	for _, label := range raw.Labels {
		labels = append(labels, label.Name)
	}
	return Metadata{Title: raw.Title, Body: raw.Body, Draft: raw.Draft, HeadRef: raw.Head.Ref, HeadSHA: raw.Head.SHA, BaseRef: raw.Base.Ref, BaseSHA: raw.Base.SHA, Labels: labels}, nil
}

func (f *forgeClient) findPR(ctx context.Context, repo, bookmark string) (int, error) {
	owner, _, _ := strings.Cut(repo, "/")
	var prs []struct {
		Number int
		Head   struct{ Ref string }
	}
	path := "/repos/" + repo + "/pulls?state=open&head=" + url.QueryEscape(owner+":"+bookmark) + "&per_page=100"
	if err := f.api(ctx, http.MethodGet, path, nil, &prs); err != nil {
		return 0, err
	}
	if len(prs) == 0 {
		return 0, nil
	}
	if len(prs) != 1 || prs[0].Head.Ref != bookmark {
		return 0, oops.Errorf("ambiguous PR ownership for bookmark")
	}
	return prs[0].Number, nil
}

func (f *forgeClient) parentMerged(ctx context.Context, repo, bookmark string) (bool, string, error) {
	open, err := f.findPR(ctx, repo, bookmark)
	if err != nil || open != 0 {
		return false, "", err
	}
	owner, _, _ := strings.Cut(repo, "/")
	for page := 1; ; page++ {
		var prs []struct {
			MergedAt *string `json:"merged_at"`
			Head     struct{ Ref, SHA string }
		}
		path := "/repos/" + repo + "/pulls?state=closed&head=" + url.QueryEscape(owner+":"+bookmark) + "&per_page=100&page=" + strconv.Itoa(page)
		if err := f.api(ctx, http.MethodGet, path, nil, &prs); err != nil {
			return false, "", err
		}
		for _, pr := range prs {
			if pr.Head.Ref != bookmark {
				return false, "", oops.Errorf("ambiguous parent PR ownership")
			}
			if pr.MergedAt != nil {
				if !fullCommitIDRE.MatchString(pr.Head.SHA) {
					return false, "", oops.Errorf("merged parent PR has no full head commit ID")
				}
				return true, pr.Head.SHA, nil
			}
		}
		if len(prs) < 100 {
			return false, "", nil
		}
	}
}

func (f *forgeClient) timeline(ctx context.Context, repo string, pr int) ([]TimelineEvent, bool, error) {
	events := []TimelineEvent{}
	for page := 1; ; page++ {
		var raw []struct {
			Event, State    string
			ID              int64
			DismissedReview struct {
				ReviewID int64 `json:"review_id"`
			} `json:"dismissed_review"`
		}
		path := "/repos/" + repo + "/issues/" + strconv.Itoa(pr) + "/timeline?per_page=100&page=" + strconv.Itoa(page)
		if err := f.api(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return events, false, err
		}
		for _, event := range raw {
			events = append(events, TimelineEvent{Index: len(events), Event: event.Event, State: event.State, ReviewID: event.ID, DismissedReviewID: event.DismissedReview.ReviewID})
		}
		if len(raw) < 100 {
			return events, true, nil
		}
	}
}
