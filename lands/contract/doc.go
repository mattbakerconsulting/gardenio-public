// Package contract holds the Lands contract v1: openapi.yaml (the source of
// truth), its generated bindings (landsv1 for Go, ts/ for TypeScript), and the
// golden fixtures both bindings are checked against.
//
// Its one runtime value is HygieneDenylist (denylist.go), the denylist the map
// scans live answers with. Its tests are the contract's guards:
//
//   - spec_test.go: the rules openapi.yaml's description states (paths, the
//     version header, problem+json, pagination, closed objects, the field cap,
//     no identity or backing-record fields).
//   - fixtures_test.go: every object schema has golden fixtures; each one
//     validates against openapi.yaml as JSON Schema 2020-12 and round-trips
//     through the generated Go type with unknown fields rejected.
//   - hygiene_test.go: hygiene_denylist.txt over the spec, the fixtures and
//     both generated bindings.
//   - deps_test.go: the module imports nothing from kbeads or the controller,
//     has no replace directives and embeds nothing but the denylist.
//
// The module stays out of go.work on purpose; run it with
// `make test-lands-contract`.
package contract
