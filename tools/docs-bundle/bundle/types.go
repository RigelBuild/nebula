// SPDX-License-Identifier: MIT OR Apache-2.0

// Package bundle implements docs-bundle's internal page and link checks.
package bundle

type ConfigMode string

const (
	ConfigRequired ConfigMode = "required"
	ConfigDefault  ConfigMode = "default"
)

type BundleSource struct {
	Repo   string     `json:"repo"`
	Mount  string     `json:"mount"`
	Config ConfigMode `json:"config"`
}

type FetchedSource struct {
	BundleSource
	Root string `json:"root"`
	SHA  string `json:"sha"`
}

type SectionRule struct {
	ID        string
	Label     string
	Route     string
	Prefix    *string
	Files     []string
	Pagefind  *bool
	Collapsed *bool
}

type PackageRules struct {
	Route       string
	Allow       []string
	ReadmeDepth int
	Nested      []string
}

type PublishConfig struct {
	Exclude  []string
	Sections []SectionRule
	Packages *PackageRules
}

type BundlePage struct {
	Repo       string
	SourcePath string
	Route      string
	Section    *string
	Pagefind   bool
}

type BrokenLink struct {
	Repo       string
	SourcePath string
	Target     string
}
