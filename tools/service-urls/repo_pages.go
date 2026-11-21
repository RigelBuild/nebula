// SPDX-License-Identifier: MIT OR Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

func readOrgRepos(ctx context.Context, client *http.Client, token string) ([]orgRepo, error) {
	var org []orgRepo
	page := repoInventoryURL
	for page != "" {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, page, nil)
		if err != nil {
			return nil, fmt.Errorf("request failed: %w", err)
		}
		request.Header.Set("Accept", "application/vnd.github+json")
		request.Header.Set("Authorization", "Bearer "+token)
		response, err := client.Do(request)
		if err != nil {
			return nil, fmt.Errorf("request failed: network error: %w", err)
		}
		body, readErr := io.ReadAll(response.Body)
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return nil, fmt.Errorf("response failed: HTTP %d, could not read response", response.StatusCode)
		}
		if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
			return nil, fmt.Errorf("request failed: HTTP %d", response.StatusCode)
		}
		pageRepos, err := parseOrgRepos(body)
		if err != nil {
			return nil, fmt.Errorf("response failed: HTTP %d: %w", response.StatusCode, err)
		}
		org = append(org, pageRepos...)
		page, err = nextRepoPage(response.Header.Get("Link"))
		if err != nil {
			return nil, fmt.Errorf("invalid pagination URL: %w", err)
		}
	}
	return org, nil
}

func withoutRedirects(client *http.Client) *http.Client {
	duplicate := *client
	duplicate.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &duplicate
}

func nextRepoPage(header string) (string, error) {
	for part := range strings.SplitSeq(header, ",") {
		left, right, found := strings.Cut(part, ";")
		if !found {
			continue
		}
		link := strings.TrimSpace(left)
		if !strings.HasPrefix(link, "<") || !strings.HasSuffix(link, ">") {
			continue
		}
		rel := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(right), "rel="))
		rel = strings.Trim(rel, "\"'")
		if rel != "next" {
			continue
		}
		reference, err := url.Parse(link[1 : len(link)-1])
		if err != nil {
			return "", errors.New("pagination URL must use the GitHub API origin")
		}
		base, err := url.Parse(repoInventoryURL)
		if err != nil {
			return "", fmt.Errorf("parse API URL: %w", err)
		}
		next := base.ResolveReference(reference)
		if next.Scheme != "https" || !strings.EqualFold(next.Hostname(), "api.github.com") || next.Port() != "" || next.User != nil {
			return "", errors.New("pagination URL must use the GitHub API origin")
		}
		return next.String(), nil
	}
	return "", nil
}
