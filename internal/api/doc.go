// Package api will implement the looperd HTTP surface and adapters.
package api

// The frozen /api/v1 response and error compat artifacts under
// testdata/contracts are captured from these handlers, so a deliberate
// response-shape change regenerates them in the same commit. CI runs the same
// command and fails on a dirty tree; see contract_artifact_regen_test.go.
//
//go:generate go test -run TestRegenerateContractArtifacts -contracts.regenerate -count=1 .
