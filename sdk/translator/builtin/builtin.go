// Package builtin exposes the built-in translator registrations for SDK users.
package builtin

import (
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"

	_ "github.com/router-for-me/CLIProxyAPI/v7/internal/translator"
)

// Registry exposes the default registry populated with all built-in translators.
func Registry() *sdktranslator.Registry {
	return sdktranslator.Default()
}

// NewRegistry returns an isolated registry populated with built-in translators.
func NewRegistry() *sdktranslator.Registry {
	return sdktranslator.NewBuiltinRegistry()
}

// Pipeline returns a pipeline that already contains the built-in translators.
func Pipeline() *sdktranslator.Pipeline {
	return sdktranslator.NewPipeline(sdktranslator.Default())
}
