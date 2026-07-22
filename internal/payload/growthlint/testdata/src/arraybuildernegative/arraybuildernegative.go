package arraybuildernegative

import "strings"

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
