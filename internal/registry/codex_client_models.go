package registry

import (
	"bytes"
	_ "embed"
)

//go:embed models/codex_client_models.json
var codexClientModelsJSON []byte

// GetCodexClientModelsJSON returns the embedded Codex client model catalog.
func GetCodexClientModelsJSON() []byte {
	return bytes.Clone(codexClientModelsJSON) //nolint:payload-clone reason=embedded_static_catalog
}
