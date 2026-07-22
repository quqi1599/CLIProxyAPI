package arraybuilderpositive

import (
	"bytes"
	"strings"
)

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

func manualRawByteBuffer(items [][]byte) []byte {
	var output bytes.Buffer
	output.WriteByte('[') // want "PG004 payload growth risk"
	for index, item := range items {
		if index > 0 {
			output.WriteByte(',')
		}
		output.Write(item)
	}
	output.WriteByte(']')
	return output.Bytes()
}

func manualRawStringBuffer(items []string) []byte {
	var output bytes.Buffer
	output.WriteString("[") // want "PG004 payload growth risk"
	for index, item := range items {
		if index > 0 {
			output.WriteString(",")
		}
		output.WriteString(item)
	}
	output.WriteString("]")
	return output.Bytes()
}
