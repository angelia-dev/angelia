// Package openapi embeds the vendored REST API specification (see
// api/README.md). internal/api reads it to route, authorize and validate
// requests from the specification itself.
package openapi

import _ "embed"

// Spec is the OpenAPI 3.1 document angelia.v1.yaml, byte for byte.
//
//go:embed angelia.v1.yaml
var Spec []byte
