// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"slices"
	"sort"
	"strings"
)

func collectMarkdownFiles(root string) ([]string, error) {
	files := []string{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		if entry.IsDir() {
			if isSkippedDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() && IsGatherable(relative) {
			files = append(files, relative)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(files)
	return files, nil
}

func CollectPages(source FetchedSource, config PublishConfig) ([]BundlePage, error) {
	files, err := collectMarkdownFiles(source.Root)
	if err != nil {
		return nil, err
	}
	eligible := make([]string, 0, len(files))
	for _, path := range files {
		if !MatchesAny(path, config.Exclude) {
			eligible = append(eligible, path)
		}
	}
	if config.Packages != nil {
		for _, path := range eligible {
			if firstSection(path, config.Sections) != nil {
				continue
			}
			if !hasPackageReadmeDepth(path, *config.Packages) && !packageAllowed(path, *config.Packages) {
				return nil, fmt.Errorf("stray package page: %s", path)
			}
		}
	}
	pages := make([]BundlePage, 0, len(eligible))
	for _, path := range eligible {
		section := firstSection(path, config.Sections)
		if len(config.Sections) > 0 && section == nil && config.Packages == nil {
			if source.Config == ConfigRequired {
				return nil, fmt.Errorf("unassigned page: %s", path)
			}
			continue
		}
		contentPath := path
		if source.Mount != "" {
			contentPath = pathJoin(source.Mount, path)
		}
		var route string
		switch {
		case section != nil:
			relativePath := ""
			if slices.Contains(section.Files, path) {
				relativePath = sectionFilePath(path)
			} else if section.Prefix != nil {
				relativePath = sectionPath(path, *section.Prefix)
			}
			route = cleanRoute(section.Route) + RouteSlug(relativePath)
		case config.Packages != nil:
			id, rest := packageIdentifier(path, config.Packages.Nested)
			packagePath := cleanPackageRoute(config.Packages.Route) + "/" + id
			if rest != "" {
				packagePath += "/" + rest
			}
			route = RouteSlug(packagePath)
		default:
			route = "/" + source.Repo + RouteSlug(contentPath)
		}
		var sectionID *string
		if section != nil {
			sectionID = new(section.ID)
		}
		pages = append(pages, BundlePage{
			Repo:       source.Repo,
			SourcePath: path,
			Route:      route,
			Section:    sectionID,
			Pagefind:   pagefindValue(section),
		})
	}
	return pages, nil
}

func firstSection(path string, sections []SectionRule) *SectionRule {
	for index := range sections {
		if sectionMatches(path, sections[index]) {
			return &sections[index]
		}
	}
	return nil
}

func sectionPath(path, prefix string) string {
	path = strings.TrimPrefix(path, prefix)
	return strings.TrimPrefix(path, "/")
}

func sectionFilePath(path string) string {
	return strings.ReplaceAll(strings.TrimPrefix(path, "."), "/", "-")
}

func cleanRoute(route string) string {
	return strings.TrimSuffix(route, "/")
}

func cleanPackageRoute(route string) string {
	return strings.Trim(route, "/")
}

func pathJoin(left, right string) string {
	if left == "" {
		return right
	}
	return strings.TrimSuffix(left, "/") + "/" + strings.TrimPrefix(right, "/")
}

func AssertNoRouteCollisions(pages []BundlePage) error {
	byRoute := make(map[string][]BundlePage, len(pages))
	order := make([]string, 0, len(pages))
	for _, page := range pages {
		if _, exists := byRoute[page.Route]; !exists {
			order = append(order, page.Route)
		}
		byRoute[page.Route] = append(byRoute[page.Route], page)
	}
	collisions := make([]string, 0)
	pageCount := 0
	for _, route := range order {
		if len(byRoute[route]) > 1 {
			collisions = append(collisions, route)
			pageCount += len(byRoute[route])
		}
	}
	if len(collisions) == 0 {
		return nil
	}
	return fmt.Errorf("%d page(s) collide on route(s): %s", pageCount, strings.Join(collisions, ", "))
}
