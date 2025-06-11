package app

import (
	wasmkeeper "github.com/EuclidProtocol/vsld/x/wasm/keeper"
)

// Deprecated: Use BuiltInCapabilities from github.com/EuclidProtocol/vsld/x/wasm/keeper
func AllCapabilities() []string {
	return wasmkeeper.BuiltInCapabilities()
}
