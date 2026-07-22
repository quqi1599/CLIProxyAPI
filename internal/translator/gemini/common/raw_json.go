package common

import internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"

// RawJSONArray joins valid JSON values into an array without reparsing or
// repeatedly rewriting the growing result.
func RawJSONArray(items [][]byte) []byte {
	return internalpayload.BuildRaw(items)
}
