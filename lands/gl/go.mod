// Module gardenio/lands/gl is gl, the tenant's CLI for a land. It speaks only
// the Lands contract (gardenio/lands/contract) through the land door, and
// signs in with the shared device-grant login (gardenio/clicreds/oidclogin).
// It sits outside go.work so no workspace member can slip a backing-store
// import in; deps_test.go checks that with go list -deps. The two replace
// directives below point at those two in-repo modules and nothing else
// (TestGoModReplacesOnlyInRepoModules): the dotless module paths cannot be
// fetched, the same reason kbeads/go.mod replaces them.
module gardenio/lands/gl

go 1.25.12

require (
	gardenio/clicreds v0.0.0
	gardenio/lands/contract v0.0.0
	github.com/spf13/cobra v1.10.2
)

require (
	github.com/apapsch/go-jsonmerge/v2 v2.0.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/oapi-codegen/runtime v1.7.0 // indirect
	github.com/spf13/pflag v1.0.9 // indirect
)

replace gardenio/clicreds => ../../gardenio/clicreds

replace gardenio/lands/contract => ../contract
