package config

// Walk describes the filesystem roots used for one inventory pass.
type Walk struct {
	State     string      `json:"state"`
	Root      string      `json:"root"`
	Sources   []Source    `json:"sources"`
	Watch     []string    `json:"watch"`
	Exclude   []Exclusion `json:"exclude"`
	FinalOnly []string    `json:"finalOnly"`
}

// Source names a source root relative to Root.
type Source struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// Exclusion is a root-relative path or glob excluded from the walk.
type Exclusion struct {
	Path string `json:"path"`
}
