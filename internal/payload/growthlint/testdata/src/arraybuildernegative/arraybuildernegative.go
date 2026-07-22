package arraybuildernegative

import (
	"bytes"
	"strings"
)

func commaSeparatedText(items []string) string {
	return strings.Join(items, ",")
}

func labeledCommaSeparatedText(items []string) string {
	return "items: " + strings.Join(items, ",")
}

func bracketedSemicolonText(items []string) string {
	return "[" + strings.Join(items, ";") + "]"
}

func labeledBracketedText(items []string) string {
	return "items=[" + strings.Join(items, ",") + "]"
}

func suffixedBracketedText(items []string) string {
	return "[" + strings.Join(items, ",") + "] items"
}

func localJoin(items []string, separator string) string {
	return strings.Join(items, separator)
}

func unrelatedJoinFunction(items []string) string {
	return "[" + localJoin(items, ",") + "]"
}

func bracketedDisplay(items []string) string {
	var output bytes.Buffer
	output.WriteByte('[')
	for index, item := range items {
		if index > 0 {
			output.WriteByte(',')
		}
		output.WriteString(item)
	}
	output.WriteByte(']')
	return output.String()
}

func binaryEnvelope(item []byte) []byte {
	var output bytes.Buffer
	output.WriteByte('[')
	output.Write(item)
	output.WriteByte(']')
	return output.Bytes()
}
