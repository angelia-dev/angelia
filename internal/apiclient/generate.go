// Package apiclient is the typed Go client of the Angelia REST API, generated
// from the vendored OpenAPI document (api/openapi), plus the small hand-written
// layer the CLI and other callers share: bearer authentication, problem
// details errors and the event stream reader.
package apiclient

// oapiCodegenVersion is the oapi-codegen release that generates client.gen.go;
// it is the release internal/api uses, so both ends read the spec the same way.
const oapiCodegenVersion = "v2.8.2"

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2 -config oapi-codegen.yaml ../../api/openapi/angelia.v1.yaml
