// SPDX-License-Identifier: MIT OR Apache-2.0

package bundle

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
)

type linkTarget struct {
	start int
	end   int
	raw   string
}

var (
	inlineLinkPattern = regexp.MustCompile(`(?m)\[[^\[\]]*(?:!\[[^\]]*\]\([^)]*\)[^\[\]]*)*\]\(\s*(<[^>]*>|[^\s)]+)(?:\s+[^)]*)?\)`)
	imagePattern      = regexp.MustCompile(`!\[[^\]]*\]\(\s*(<[^>]*>|[^\s)]+)(?:\s+[^)]*)?\)`)
	definitionPattern = regexp.MustCompile(`(?m)^ {0,3}\[([^\]\n]+)\]:[ \t]*(<[^>\n]*>|[^\s<>]+)(?:[ \t]+[^\n]*)?$`)
	autoURLPattern    = regexp.MustCompile(`(?i)^(?:[a-z][a-z\d+.-]*:|//)`)
)

func maskedMarkdown(source string) string {
	lines := strings.Split(source, "\n")
	fenceMarker := byte(0)
	fenceLength := 0
	for index, line := range lines {
		if fenceMarker != 0 {
			lines[index] = strings.Repeat(" ", len(line))
			if closesFence(line, fenceMarker, fenceLength) {
				fenceMarker = 0
			}
			continue
		}
		match := fenceOpenPattern.FindStringSubmatch(line)
		if len(match) > 1 {
			fenceMarker = match[1][0]
			fenceLength = len(match[1])
			lines[index] = strings.Repeat(" ", len(line))
			continue
		}
		lines[index] = maskCodeSpans(line)
	}
	return strings.Join(lines, "\n")
}

func maskCodeSpans(line string) string {
	var result strings.Builder
	for index := 0; index < len(line); {
		if line[index] != '`' {
			result.WriteByte(line[index])
			index++
			continue
		}
		start := index
		for index < len(line) && line[index] == '`' {
			index++
		}
		runLength := index - start
		search := index
		found := -1
		for search < len(line) {
			next := strings.IndexByte(line[search:], '`')
			if next < 0 {
				break
			}
			runStart := search + next
			runEnd := runStart
			for runEnd < len(line) && line[runEnd] == '`' {
				runEnd++
			}
			if runEnd-runStart == runLength {
				found = runEnd
				break
			}
			search = runEnd
		}
		if found < 0 {
			result.WriteString(line[start:index])
			continue
		}
		result.WriteString(strings.Repeat(" ", found-start))
		index = found
	}
	return result.String()
}

func linkTargets(source string) []linkTarget {
	masked := maskedMarkdown(source)
	candidates := make([]linkTarget, 0)
	for _, pattern := range []*regexp.Regexp{inlineLinkPattern, imagePattern} {
		for _, match := range pattern.FindAllStringSubmatchIndex(masked, -1) {
			if len(match) < 4 {
				continue
			}
			start, end := match[2], match[3]
			if pattern == inlineLinkPattern && match[0] > 0 && masked[match[0]-1] == '!' {
				continue
			}
			candidates = append(candidates, linkTarget{start: start, end: end, raw: source[start:end]})
		}
	}

	lines := strings.Split(masked, "\n")
	lineOffset := 0
	for _, line := range lines {
		if match := definitionPattern.FindStringSubmatchIndex(line); len(match) >= 6 && !strings.HasPrefix(line[match[2]:match[3]], "^") && !strings.HasPrefix(line[match[4]:match[5]], "\"") && !strings.HasPrefix(line[match[4]:match[5]], "'") {
			start, end := lineOffset+match[4], lineOffset+match[5]
			candidates = append(candidates, linkTarget{start: start, end: end, raw: source[start:end]})
		}
		lineOffset += len(line) + 1
	}
	sort.Slice(candidates, func(left, right int) bool {
		return candidates[left].start < candidates[right].start
	})
	links := make([]linkTarget, 0, len(candidates))
	for _, candidate := range candidates {
		if len(links) == 0 || candidate.start >= links[len(links)-1].end {
			links = append(links, candidate)
		}
	}
	return links
}

func unwrapTarget(target string) string {
	if strings.HasPrefix(target, "<") && strings.HasSuffix(target, ">") {
		return target[1 : len(target)-1]
	}
	return target
}

func targetParts(target string) (string, string) {
	index := strings.IndexAny(target, "?#")
	if index < 0 {
		return target, ""
	}
	return target[:index], target[index:]
}

func decodePath(path string) string {
	decoded, err := url.PathUnescape(path)
	if err != nil {
		return path
	}
	return decoded
}

func routeKey(repo, path string) string {
	return repo + ":" + normalizePath(path)
}

func repoTarget(root string, page BundlePage, target string) string {
	path, _ := targetParts(target)
	path = decodePath(path)
	base := filepath.Dir(filepath.Join(root, filepath.FromSlash(page.SourcePath)))
	if strings.HasPrefix(path, "/") {
		base = root
		path = strings.TrimPrefix(path, "/")
	}
	resolved := filepath.Clean(filepath.Join(base, filepath.FromSlash(path)))
	relative, err := filepath.Rel(root, resolved)
	if err != nil {
		return filepath.ToSlash(resolved)
	}
	return filepath.ToSlash(relative)
}

func FindBrokenLinks(root, repo string, pages []BundlePage) ([]BrokenLink, error) {
	routes := make(map[string]string, len(pages))
	for _, page := range pages {
		routes[routeKey(page.Repo, page.SourcePath)] = page.Route
	}
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	broken := []BrokenLink{}
	for _, page := range pages {
		if page.Repo != repo {
			continue
		}
		data, err := os.ReadFile(filepath.Join(rootPath, filepath.FromSlash(page.SourcePath)))
		if err != nil {
			return nil, err
		}
		for _, link := range linkTargets(string(data)) {
			target := unwrapTarget(link.raw)
			if autoURLPattern.MatchString(target) {
				continue
			}
			path, _ := targetParts(target)
			if path == "" {
				continue
			}
			resolved := repoTarget(rootPath, page, target)
			if _, ok := routes[routeKey(repo, resolved)]; ok {
				continue
			}
			absolute := filepath.Join(rootPath, filepath.FromSlash(resolved))
			if !pathWithinRoot(rootPath, absolute) {
				broken = append(broken, BrokenLink{Repo: repo, SourcePath: page.SourcePath, Target: resolved})
				continue
			}
			_, statErr := os.Stat(absolute)
			if statErr == nil {
				continue
			}
			if !os.IsNotExist(statErr) && !errors.Is(statErr, syscall.ENOTDIR) {
				return nil, statErr
			}
			broken = append(broken, BrokenLink{Repo: repo, SourcePath: page.SourcePath, Target: resolved})
		}
	}
	return broken, nil
}
