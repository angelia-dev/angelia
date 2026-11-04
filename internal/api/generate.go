package api

// oapiCodegenVersion is the oapi-codegen release that generates api.gen.go.
// strict-interface.tmpl is a trimmed copy of that release's
// strict/strict-interface.tmpl: upgrading the version below needs a re-diff
// of the template against the new release (TestGeneratorPinned fails until
// this constant, the go:generate line and the generated header agree).
const oapiCodegenVersion = "v2.8.2"

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.8.2 -config oapi-codegen.yaml ../../api/openapi/angelia.v1.yaml
