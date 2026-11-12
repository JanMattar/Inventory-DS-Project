// Package api embeds the OpenAPI document and a single-file Swagger UI page,
// so every server serves its own documentation:
//
//	GET /openapi.yaml  the spec
//	GET /docs          Swagger UI (view + "Try it out") for that spec
//	GET /editor        Swagger Editor (edit + live preview) for that spec
package api

import _ "embed"

//go:embed openapi.yaml
var Spec []byte

//go:embed docs.html
var DocsHTML []byte

//go:embed editor.html
var EditorHTML []byte
