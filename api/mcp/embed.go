// Package mcp embeds the vendored MCP tool contract (see api/README.md).
// internal/mcp serves its input and output schemas and compares its
// tools/list with it in a golden test.
package mcp

import _ "embed"

// Tools is mcp/tools.v1.json of the spec repository, byte for byte: the
// result of tools/list.
//
//go:embed tools.v1.json
var Tools []byte
