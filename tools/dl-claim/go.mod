module github.com/RigelBuild/nebula/tools/dl-claim

go 1.27.0

require (
	github.com/RigelBuild/nebula/tools/go-lib v0.0.0
	github.com/samber/mo v1.12.0
)

require go.yaml.in/yaml/v3 v3.0.6 // indirect

replace github.com/RigelBuild/nebula/tools/go-lib => ../go-lib
