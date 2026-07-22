// Package util provides utility functions for the CLI Proxy API server.
package util

import (
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

var gjsonPathKeyReplacer = strings.NewReplacer(".", "\\.", "*", "\\*", "?", "\\?")

const placeholderReasonDescription = "Brief explanation of why you are calling this tool"

var unsupportedConstraints = []string{
	"minLength", "maxLength", "exclusiveMinimum", "exclusiveMaximum",
	"pattern", "minItems", "maxItems", "uniqueItems", "format",
	"default", "examples",
}

var unsupportedSchemaKeywords = keywordSet(append(append([]string(nil), unsupportedConstraints...),
	"$schema", "$defs", "definitions", "const", "$ref", "$id", "additionalProperties",
	"propertyNames", "patternProperties", "$comment", "enumDescriptions", "enumTitles",
	"prefill", "deprecated",
))

// CleanJSONSchemaForAntigravity transforms a JSON schema to be compatible with Antigravity API.
func CleanJSONSchemaForAntigravity(jsonStr string) string {
	return cleanJSONSchema(jsonStr, true)
}

// CleanJSONSchemaForGemini transforms a JSON schema to be compatible with Gemini tool calling.
func CleanJSONSchemaForGemini(jsonStr string) string {
	return cleanJSONSchema(jsonStr, false)
}

// cleanJSONSchema parses and encodes once so schema size does not multiply transformation cost.
func cleanJSONSchema(jsonStr string, addPlaceholder bool) string {
	root, ok := decodeSchema(jsonStr)
	if !ok {
		return jsonStr
	}

	root = addCompatibilityHints(root)
	root = mergeAllOf(root)
	root = flattenComposition(root, "anyOf")
	root = flattenComposition(root, "oneOf")
	root, _ = flattenTypeArrays(root)
	removeFields(root, unsupportedSchemaKeywords, true)
	if !addPlaceholder {
		removeFields(root, keywordSet([]string{"nullable", "title"}), false)
		removePlaceholderProperties(root)
	}
	cleanupRequiredFields(root)
	if addPlaceholder {
		addEmptySchemaPlaceholders(root, true)
	}

	result, err := json.Marshal(root)
	if err != nil {
		return jsonStr
	}
	return string(result)
}

func decodeSchema(jsonStr string) (any, bool) {
	decoder := json.NewDecoder(strings.NewReader(jsonStr))
	decoder.UseNumber()
	var root any
	if err := decoder.Decode(&root); err != nil {
		return nil, false
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, false
	}
	return root, true
}

func addCompatibilityHints(value any) any {
	switch current := value.(type) {
	case []any:
		for i := range current {
			current[i] = addCompatibilityHints(current[i])
		}
		return current
	case map[string]any:
		walkObjectChildren(current, addCompatibilityHints)

		if ref, exists := current["$ref"]; exists {
			refValue := valueString(ref)
			name := refValue
			if index := strings.LastIndex(refValue, "/"); index >= 0 {
				name = refValue[index+1:]
			}
			description := "See: " + name
			if existing := valueString(current["description"]); existing != "" {
				description = existing + " (" + description + ")"
			}
			return map[string]any{"type": "object", "description": description}
		}

		if constant, exists := current["const"]; exists {
			if _, hasEnum := current["enum"]; !hasEnum {
				current["enum"] = []any{constant}
			}
		}
		if values, ok := current["enum"].([]any); ok {
			stringsOnly := make([]any, len(values))
			for i, item := range values {
				stringsOnly[i] = valueString(item)
			}
			current["enum"] = stringsOnly
			current["type"] = "string"
			if len(stringsOnly) > 1 && len(stringsOnly) <= 10 {
				hints := make([]string, len(stringsOnly))
				for i := range stringsOnly {
					hints[i] = stringsOnly[i].(string)
				}
				appendDescription(current, "Allowed: "+strings.Join(hints, ", "))
			}
		}
		if additional, exists := current["additionalProperties"]; exists && additional == false {
			appendDescription(current, "No extra properties allowed")
		}
		for _, key := range unsupportedConstraints {
			constraint, exists := current[key]
			if !exists || isContainer(constraint) {
				continue
			}
			appendDescription(current, key+": "+valueString(constraint))
		}
		return current
	default:
		return value
	}
}

func mergeAllOf(value any) any {
	switch current := value.(type) {
	case []any:
		for i := range current {
			current[i] = mergeAllOf(current[i])
		}
		return current
	case map[string]any:
		walkObjectChildren(current, mergeAllOf)
		items, ok := current["allOf"].([]any)
		if !ok {
			return current
		}

		properties, _ := current["properties"].(map[string]any)
		required := stringValues(current["required"])
		seenRequired := make(map[string]struct{}, len(required))
		for _, name := range required {
			seenRequired[name] = struct{}{}
		}
		for _, item := range items {
			part, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if partProperties, ok := part["properties"].(map[string]any); ok {
				if properties == nil {
					properties = make(map[string]any, len(partProperties))
				}
				for name, schema := range partProperties {
					properties[name] = schema
				}
			}
			for _, name := range stringValues(part["required"]) {
				if _, exists := seenRequired[name]; exists {
					continue
				}
				seenRequired[name] = struct{}{}
				required = append(required, name)
			}
		}
		if properties != nil {
			current["properties"] = properties
		}
		if len(required) > 0 {
			current["required"] = stringsToValues(required)
		}
		delete(current, "allOf")
		return current
	default:
		return value
	}
}

func flattenComposition(value any, keyword string) any {
	switch current := value.(type) {
	case []any:
		for i := range current {
			current[i] = flattenComposition(current[i], keyword)
		}
		return current
	case map[string]any:
		walkObjectChildren(current, func(child any) any { return flattenComposition(child, keyword) })
		items, ok := current[keyword].([]any)
		if !ok || len(items) == 0 {
			return current
		}

		selected, types := selectBestSchema(items)
		selectedObject, ok := selected.(map[string]any)
		if !ok {
			return selected
		}
		if parentDescription := valueString(current["description"]); parentDescription != "" {
			childDescription := valueString(selectedObject["description"])
			switch {
			case childDescription == "":
				selectedObject["description"] = parentDescription
			case childDescription != parentDescription:
				selectedObject["description"] = parentDescription + " (" + childDescription + ")"
			}
		}
		if len(types) > 1 {
			appendDescription(selectedObject, "Accepts: "+strings.Join(types, " | "))
		}
		return selectedObject
	default:
		return value
	}
}

func selectBestSchema(items []any) (any, []string) {
	bestIndex, bestScore := 0, -1
	types := make([]string, 0, len(items))
	for i, item := range items {
		schema, _ := item.(map[string]any)
		typeName := valueString(schema["type"])
		score := 0
		_, hasProperties := schema["properties"]
		_, hasItems := schema["items"]
		switch {
		case typeName == "object" || hasProperties:
			score = 3
			if typeName == "" {
				typeName = "object"
			}
		case typeName == "array" || hasItems:
			score = 2
			if typeName == "" {
				typeName = "array"
			}
		case typeName != "" && typeName != "null":
			score = 1
		default:
			typeName = "null"
		}
		types = append(types, typeName)
		if score > bestScore {
			bestIndex, bestScore = i, score
		}
	}
	return items[bestIndex], types
}

func flattenTypeArrays(value any) (any, bool) {
	switch current := value.(type) {
	case []any:
		for i := range current {
			current[i], _ = flattenTypeArrays(current[i])
		}
		return current, false
	case map[string]any:
		for key, child := range current {
			if key == "properties" {
				continue
			}
			current[key], _ = flattenTypeArrays(child)
		}
		if properties, ok := current["properties"].(map[string]any); ok {
			nullable := make(map[string]struct{})
			for name, child := range properties {
				var isNullable bool
				properties[name], isNullable = flattenTypeArrays(child)
				if !isNullable {
					continue
				}
				nullable[name] = struct{}{}
				if schema, ok := properties[name].(map[string]any); ok {
					appendDescription(schema, "(nullable)")
				}
			}
			if len(nullable) > 0 {
				filterRequired(current, func(name string) bool {
					_, remove := nullable[name]
					return !remove
				})
			}
		}

		types, ok := current["type"].([]any)
		if !ok || len(types) == 0 {
			return current, false
		}
		hasNull := false
		nonNull := make([]string, 0, len(types))
		for _, item := range types {
			typeName := valueString(item)
			if typeName == "null" {
				hasNull = true
			} else if typeName != "" {
				nonNull = append(nonNull, typeName)
			}
		}
		firstType := "string"
		if len(nonNull) > 0 {
			firstType = nonNull[0]
		}
		current["type"] = firstType
		if len(nonNull) > 1 {
			appendDescription(current, "Accepts: "+strings.Join(nonNull, " | "))
		}
		return current, hasNull
	default:
		return value, false
	}
}

func removeFields(value any, keywords map[string]struct{}, removeExtensions bool) {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			removeFields(item, keywords, removeExtensions)
		}
	case map[string]any:
		for key, child := range current {
			if key == "properties" {
				if properties, ok := child.(map[string]any); ok {
					for _, schema := range properties {
						removeFields(schema, keywords, removeExtensions)
					}
				}
				continue
			}
			_, unsupported := keywords[key]
			if unsupported || removeExtensions && strings.HasPrefix(key, "x-") {
				delete(current, key)
				continue
			}
			removeFields(child, keywords, removeExtensions)
		}
	}
}

// removeExtensionFields removes x-* schema metadata while preserving properties with those names.
func removeExtensionFields(jsonStr string) string {
	root, ok := decodeSchema(jsonStr)
	if !ok {
		return jsonStr
	}
	removeFields(root, nil, true)
	result, err := json.Marshal(root)
	if err != nil {
		return jsonStr
	}
	return string(result)
}

func removePlaceholderProperties(value any) {
	visitSchemaObjects(value, func(schema map[string]any) {
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return
		}
		if _, exists := properties["_"]; exists {
			delete(properties, "_")
			filterRequired(schema, func(name string) bool { return name != "_" })
		}
	})
	visitSchemaObjects(value, func(schema map[string]any) {
		properties, ok := schema["properties"].(map[string]any)
		if !ok || len(properties) != 1 {
			return
		}
		reason, ok := properties["reason"].(map[string]any)
		if !ok || valueString(reason["description"]) != placeholderReasonDescription {
			return
		}
		delete(properties, "reason")
		filterRequired(schema, func(name string) bool { return name != "reason" })
	})
}

func cleanupRequiredFields(value any) {
	visitSchemaObjects(value, func(schema map[string]any) {
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			return
		}
		filterRequired(schema, func(name string) bool {
			_, exists := properties[name]
			return exists
		})
	})
}

func addEmptySchemaPlaceholders(value any, root bool) {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			addEmptySchemaPlaceholders(item, false)
		}
	case map[string]any:
		for _, child := range current {
			addEmptySchemaPlaceholders(child, false)
		}
		if valueString(current["type"]) != "object" {
			return
		}
		properties, hasProperties := current["properties"]
		propertyMap, propertiesAreObject := properties.(map[string]any)
		if !hasProperties || propertiesAreObject && len(propertyMap) == 0 {
			propertyMap = map[string]any{
				"reason": map[string]any{
					"type":        "string",
					"description": placeholderReasonDescription,
				},
			}
			current["properties"] = propertyMap
			current["required"] = []any{"reason"}
			return
		}
		if !root && propertiesAreObject && len(stringValues(current["required"])) == 0 {
			if _, exists := propertyMap["_"]; !exists {
				propertyMap["_"] = map[string]any{"type": "boolean"}
			}
			current["required"] = []any{"_"}
		}
	}
}

func visitSchemaObjects(value any, visit func(map[string]any)) {
	switch current := value.(type) {
	case []any:
		for _, item := range current {
			visitSchemaObjects(item, visit)
		}
	case map[string]any:
		for _, child := range current {
			visitSchemaObjects(child, visit)
		}
		visit(current)
	}
}

func walkObjectChildren(object map[string]any, transform func(any) any) {
	for key, child := range object {
		if key != "properties" {
			object[key] = transform(child)
			continue
		}
		properties, ok := child.(map[string]any)
		if !ok {
			object[key] = transform(child)
			continue
		}
		for name, schema := range properties {
			properties[name] = transform(schema)
		}
	}
}

func appendDescription(schema map[string]any, hint string) {
	if existing := valueString(schema["description"]); existing != "" {
		hint = existing + " (" + hint + ")"
	}
	schema["description"] = hint
}

func filterRequired(schema map[string]any, keep func(string) bool) {
	required, ok := schema["required"].([]any)
	if !ok {
		return
	}
	filtered := make([]any, 0, len(required))
	for _, item := range required {
		name := valueString(item)
		if keep(name) {
			filtered = append(filtered, name)
		}
	}
	if len(filtered) == len(required) {
		return
	}
	if len(filtered) == 0 {
		delete(schema, "required")
		return
	}
	schema["required"] = filtered
}

func stringValues(value any) []string {
	values, ok := value.([]any)
	if !ok {
		return nil
	}
	result := make([]string, len(values))
	for i, item := range values {
		result[i] = valueString(item)
	}
	return result
}

func stringsToValues(values []string) []any {
	result := make([]any, len(values))
	for i := range values {
		result[i] = values[i]
	}
	return result
}

func valueString(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	default:
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

func isContainer(value any) bool {
	switch value.(type) {
	case map[string]any, []any:
		return true
	default:
		return false
	}
}

func keywordSet(values []string) map[string]struct{} {
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func escapeGJSONPathKey(key string) string {
	if strings.IndexAny(key, ".*?") == -1 {
		return key
	}
	return gjsonPathKeyReplacer.Replace(key)
}
