package main

import (
	"fmt"
	"regexp"
)

type MarkerKind string

const (
	markerRound    MarkerKind = "round"
	markerAllClear MarkerKind = "all-clear"
)

type Marker struct {
	Kind           MarkerKind
	Reviewed, Base string
	Author, URL    string
}

var markerPattern = regexp.MustCompile(`<!-- rjc:(round|all-clear) reviewed=([0-9a-f]{40}) base=([^\s<>]+) -->`)

func FormatMarker(m Marker) string {
	return fmt.Sprintf("<!-- rjc:%s reviewed=%s base=%s -->", m.Kind, m.Reviewed, m.Base)
}

func ParseMarkers(body, author, url string) []Marker {
	matches := markerPattern.FindAllStringSubmatch(body, -1)
	markers := make([]Marker, 0, len(matches))
	for _, m := range matches {
		markers = append(markers, Marker{Kind: MarkerKind(m[1]), Reviewed: m[2], Base: m[3], Author: author, URL: url})
	}
	return markers
}
