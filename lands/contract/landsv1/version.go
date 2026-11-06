// Package landsv1 is the Go binding of the Lands contract v1
// (lands/contract/openapi.yaml): the models, a client for gl, and the
// net/http server interface the land's tenant listener implements.
//
// landsv1.gen.go is generated; edit openapi.yaml and regenerate. This file is
// the only hand-written part.
package landsv1

// ContractVersion is openapi.yaml's info.version. The server sends it as
// HeaderContract on every response; a client compares it to this constant and
// warns once on skew, never refuses. spec_test.go keeps the two in step.
const ContractVersion = "1.0.0-beta.5"

// HeaderContract carries ContractVersion on every response.
const HeaderContract = "X-Lands-Contract"
