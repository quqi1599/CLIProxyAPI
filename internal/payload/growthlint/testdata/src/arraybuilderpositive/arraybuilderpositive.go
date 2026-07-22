package arraybuilderpositive

import "strings"

func manualRawArray(items []string) string {
	return "[" + strings.Join(items, ",") + "]" // want "PG004 payload growth risk"
}

func parenthesizedManualRawArray(items []string) string {
	return ("[" + strings.Join(items, ",")) + "]" // want "PG004 payload growth risk"
}

func constantDelimitersManualRawArray(items []string) string {
	const open = "["
	const separator = ","
	const close = "]"
	return open + strings.Join(items, separator) + close // want "PG004 payload growth risk"
}
