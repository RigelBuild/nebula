module github.com/RigelBuild/nebula/tools/length-gate

go 1.27.0

require (
	github.com/RigelBuild/nebula/tools/go-lib v0.0.0
	github.com/bmatcuk/doublestar/v4 v4.10.4
	github.com/samber/mo v1.12.0
	github.com/yuin/goldmark v1.8.7
)

require github.com/odvcencio/gotreesitter v0.56.0 // indirect

replace github.com/RigelBuild/nebula/tools/go-lib => ../go-lib
