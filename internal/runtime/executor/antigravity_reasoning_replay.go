package executor

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	internalcache "github.com/router-for-me/CLIProxyAPI/v7/internal/cache"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type antigravityReasoningReplayScope struct {
	modelName  string
	sessionKey string
}

func (s antigravityReasoningReplayScope) valid() bool {
	return strings.TrimSpace(s.modelName) != "" && strings.TrimSpace(s.sessionKey) != ""
}

func antigravityReplayAmplificationOverride(scope antigravityReasoningReplayScope) internalpayload.AmplificationOverride {
	if !scope.valid() {
		return internalpayload.AmplificationOverride{}
	}
	return internalpayload.AmplificationOverride{
		PolicyID:          "antigravity.reasoning_replay",
		MaxExpansionBytes: (1 << 20) + internalpayload.DefaultMaxExpansionBytes,
		MaxExpansionRatio: internalpayload.DefaultMaxExpansionRatio,
	}
}

func antigravityReasoningReplayScopeFromPayload(modelName string, payload []byte) antigravityReasoningReplayScope {
	sessionID := antigravityReplaySessionIDFromPayload(payload)
	if sessionID == "" {
		if stable := strings.TrimSpace(generateStableSessionID(payload)); stable != "" {
			sessionID = strings.TrimPrefix(stable, "-")
			if sessionID == "" {
				sessionID = stable
			}
		}
	}
	if sessionID == "" {
		return antigravityReasoningReplayScope{}
	}
	return antigravityReasoningReplayScope{
		modelName:  strings.TrimSpace(modelName),
		sessionKey: "session:" + sessionID,
	}
}

func antigravityReasoningReplayScopeFromRequest(ctx context.Context, modelName string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte) antigravityReasoningReplayScope {
	if scope := antigravityReasoningReplayScopeFromPayload(modelName, payload); scope.valid() {
		return scope
	}
	if scope := antigravityReasoningReplayScopeFromPayload(modelName, req.Payload); scope.valid() {
		return scope
	}
	if value := metadataString(opts.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return antigravityReasoningReplayScope{modelName: modelName, sessionKey: "execution:" + value}
	}
	if value := metadataString(req.Metadata, cliproxyexecutor.ExecutionSessionMetadataKey); value != "" {
		return antigravityReasoningReplayScope{modelName: modelName, sessionKey: "execution:" + value}
	}
	_ = ctx
	return antigravityReasoningReplayScope{}
}

func antigravityReplaySessionIDFromPayload(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	for _, path := range []string{"sessionId", "session_id", "request.sessionId", "request.session_id"} {
		if id := strings.TrimSpace(gjson.GetBytes(payload, path).String()); id != "" {
			return id
		}
	}
	return ""
}

func antigravityReasoningReplayPendingModelContentIndex(payload []byte) (contentIndex int, basePartIndex int) {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return 0, 0
	}
	arr := contents.Array()
	if len(arr) == 0 {
		return 0, 0
	}
	last := arr[len(arr)-1]
	if strings.EqualFold(strings.TrimSpace(last.Get("role").String()), "model") {
		ci := len(arr) - 1
		parts := last.Get("parts")
		base := 0
		if parts.IsArray() {
			base = len(parts.Array())
		}
		return ci, base
	}
	return len(arr), 0
}

func antigravityReasoningReplayResolveContentIndex(payload []byte, cached int) int {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return cached
	}
	arr := contents.Array()
	if cached >= 0 && cached < len(arr) {
		return cached
	}
	for i := len(arr) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(arr[i].Get("role").String()), "model") {
			return i
		}
	}
	if len(arr) == 0 {
		return 0
	}
	return len(arr) - 1
}

func prepareAntigravityGeminiReasoningReplayPayload(ctx context.Context, modelName string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte) ([]byte, antigravityReasoningReplayScope, error) {
	if !antigravityUsesReasoningReplayCache(modelName) {
		return payload, antigravityReasoningReplayScope{}, nil
	}
	return applyAntigravityReasoningReplayCache(ctx, modelName, req, opts, payload)
}

func clearAntigravityReasoningReplayOnInvalidSignature(ctx context.Context, scope antigravityReasoningReplayScope, statusCode int, body []byte) error {
	if !scope.valid() {
		return nil
	}
	if statusCode != http.StatusBadRequest {
		return nil
	}
	bodyText := strings.ToLower(string(body))
	if !strings.Contains(bodyText, "thoughtsignature") && !strings.Contains(bodyText, "thought_signature") && !strings.Contains(bodyText, "signature") {
		return nil
	}
	return internalcache.DeleteAntigravityReasoningReplayItemRequired(ctx, scope.modelName, scope.sessionKey)
}

func applyAntigravityReasoningReplayCache(ctx context.Context, modelName string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, payload []byte) ([]byte, antigravityReasoningReplayScope, error) {
	scope := antigravityReasoningReplayScopeFromRequest(ctx, modelName, req, opts, payload)
	if !scope.valid() {
		return payload, scope, nil
	}
	items, ok, err := internalcache.GetAntigravityReasoningReplayItemsRequired(ctx, scope.modelName, scope.sessionKey)
	if err != nil || !ok || len(items) == 0 {
		return payload, scope, err
	}
	items = filterAntigravityReasoningReplayItemsForRequest(payload, items)
	if len(items) == 0 {
		return payload, scope, nil
	}
	updated, okApply := insertAntigravityReasoningReplayItems(payload, items)
	if !okApply {
		return payload, scope, nil
	}
	return updated, scope, nil
}

func filterAntigravityReasoningReplayItemsForRequest(payload []byte, items [][]byte) [][]byte {
	existing := antigravityExistingToolCallKeys(payload)
	filtered := make([][]byte, 0, len(items))
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "function_call_part":
			keys := antigravityReplayToolCallKeys(itemResult)
			if len(keys) == 0 {
				continue
			}
			if antigravityAnyKeyExists(existing, keys) {
				if !antigravityNeedsSignatureReplayForExistingFunctionCall(payload, itemResult) {
					continue
				}
			}
			if !antigravityRequestHasMatchingFunctionResponse(payload, itemResult) {
				continue
			}
		case "thought_signature":
			if antigravityRequestHasThoughtSignatureAt(payload, itemResult) {
				continue
			}
		default:
			continue
		}
		filtered = append(filtered, item)
	}
	return filtered
}

func antigravityExistingToolCallKeys(payload []byte) map[string]bool {
	existing := make(map[string]bool)
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return existing
	}
	for _, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			if fc := part.Get("functionCall"); fc.Exists() {
				for _, key := range antigravityReplayToolCallKeysFromPart(fc) {
					existing[key] = true
				}
			}
		}
	}
	return existing
}

func antigravityReplayToolCallKeys(itemResult gjson.Result) []string {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	name := strings.TrimSpace(itemResult.Get("name").String())
	if name == "" {
		return nil
	}
	args := itemResult.Get("args").Raw
	key := antigravityFunctionCallKey(name, args, callID)
	if key == "" {
		return nil
	}
	return []string{key}
}

func antigravityReplayToolCallKeysFromPart(fc gjson.Result) []string {
	return antigravityReplayToolCallKeys(gjson.Parse(fc.Raw))
}

func antigravityFunctionCallKey(name, argsRaw, callID string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	h := sha256.Sum256([]byte(strings.Join([]string{name, argsRaw, callID}, "\x00")))
	return fmt.Sprintf("fc:%x", h[:8])
}

func antigravityAnyKeyExists(existing map[string]bool, keys []string) bool {
	for _, key := range keys {
		if existing[key] {
			return true
		}
	}
	return false
}

func antigravityNeedsSignatureReplayForExistingFunctionCall(payload []byte, itemResult gjson.Result) bool {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		callID = strings.TrimSpace(itemResult.Get("id").String())
	}
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if callID == "" || sig == "" {
		return false
	}
	ci, pi, ok := antigravityFunctionCallPartLocation(payload, callID)
	if !ok {
		return false
	}
	pathSig := fmt.Sprintf("request.contents.%d.parts.%d.thoughtSignature", ci, pi)
	return strings.TrimSpace(gjson.GetBytes(payload, pathSig).String()) == ""
}

func antigravityRequestHasMatchingFunctionResponse(payload []byte, itemResult gjson.Result) bool {
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	if callID == "" {
		return true
	}
	_, ok := antigravityFunctionResponseContentIndex(payload, callID)
	return ok
}

func antigravityFunctionResponseContentIndex(payload []byte, callID string) (int, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, false
	}
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return -1, false
	}
	for i, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for _, part := range parts.Array() {
			fr := part.Get("functionResponse")
			if fr.Exists() && strings.TrimSpace(fr.Get("id").String()) == callID {
				return i, true
			}
		}
	}
	return -1, false
}

func antigravityFunctionCallPartLocation(payload []byte, callID string) (contentIndex int, partIndex int, ok bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, -1, false
	}
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return -1, -1, false
	}
	for ci, content := range contents.Array() {
		parts := content.Get("parts")
		if !parts.IsArray() {
			continue
		}
		for pi, part := range parts.Array() {
			fc := part.Get("functionCall")
			if fc.Exists() && strings.TrimSpace(fc.Get("id").String()) == callID {
				return ci, pi, true
			}
		}
	}
	return -1, -1, false
}

func antigravityRequestHasThoughtSignatureAt(payload []byte, itemResult gjson.Result) bool {
	ci := int(itemResult.Get("contentIndex").Int())
	pi := int(itemResult.Get("partIndex").Int())
	partPath, ok := antigravityExistingReplayPartPath(payload, ci, pi)
	if !ok {
		return false
	}
	path := partPath + ".thoughtSignature"
	return strings.TrimSpace(gjson.GetBytes(payload, path).String()) != ""
}

func antigravityExistingReplayPartPath(payload []byte, contentIndex int, partIndex int) (string, bool) {
	if contentIndex < 0 || partIndex < 0 {
		return "", false
	}
	partsPath := fmt.Sprintf("request.contents.%d.parts", contentIndex)
	parts := gjson.GetBytes(payload, partsPath)
	if !parts.IsArray() {
		return "", false
	}
	arr := parts.Array()
	if partIndex >= len(arr) || arr[partIndex].Type == gjson.Null {
		return "", false
	}
	return fmt.Sprintf("%s.%d", partsPath, partIndex), true
}

func insertAntigravityReasoningReplayItems(payload []byte, items [][]byte) ([]byte, bool) {
	document, ok := newAntigravityReplayDocument(payload)
	if !ok {
		return payload, false
	}
	for _, item := range items {
		itemResult := gjson.ParseBytes(item)
		switch strings.TrimSpace(itemResult.Get("type").String()) {
		case "thought_signature":
			sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
			document.setThoughtSignature(int(itemResult.Get("contentIndex").Int()), int(itemResult.Get("partIndex").Int()), sig)
		case "function_call_part":
			document.mergeFunctionCallPart(itemResult)
		}
	}
	return document.bytes()
}

type antigravityReplayContent struct {
	raw          []byte
	parts        [][]byte
	partsChanged bool
}

type antigravityReplayDocument struct {
	payload  []byte
	contents []*antigravityReplayContent
	changed  bool
}

func newAntigravityReplayDocument(payload []byte) (*antigravityReplayDocument, bool) {
	contents := gjson.GetBytes(payload, "request.contents")
	if !contents.IsArray() {
		return nil, false
	}
	document := &antigravityReplayDocument{
		payload:  payload,
		contents: make([]*antigravityReplayContent, 0, len(contents.Array())),
	}
	for _, content := range contents.Array() {
		parsed := &antigravityReplayContent{raw: []byte(content.Raw)}
		if parts := content.Get("parts"); parts.IsArray() {
			parsed.parts = make([][]byte, 0, len(parts.Array()))
			for _, part := range parts.Array() {
				parsed.parts = append(parsed.parts, []byte(part.Raw))
			}
		}
		document.contents = append(document.contents, parsed)
	}
	return document, true
}

func (document *antigravityReplayDocument) resolveContentIndex(cached int) int {
	if cached >= 0 && cached < len(document.contents) {
		return cached
	}
	for i := len(document.contents) - 1; i >= 0; i-- {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(document.contents[i].raw, "role").String()), "model") {
			return i
		}
	}
	if len(document.contents) == 0 {
		return 0
	}
	return len(document.contents) - 1
}

func (document *antigravityReplayDocument) existingPart(contentIndex, partIndex int) ([]byte, bool) {
	if contentIndex < 0 || contentIndex >= len(document.contents) || partIndex < 0 {
		return nil, false
	}
	parts := document.contents[contentIndex].parts
	if partIndex >= len(parts) || gjson.ParseBytes(parts[partIndex]).Type == gjson.Null {
		return nil, false
	}
	return parts[partIndex], true
}

func (document *antigravityReplayDocument) updatePart(contentIndex, partIndex int, part []byte) bool {
	if _, ok := document.existingPart(contentIndex, partIndex); !ok {
		return false
	}
	document.contents[contentIndex].parts[partIndex] = part
	document.contents[contentIndex].partsChanged = true
	document.changed = true
	return true
}

func (document *antigravityReplayDocument) appendPart(contentIndex int, part []byte) {
	if contentIndex < 0 {
		contentIndex = 0
	}
	for len(document.contents) <= contentIndex {
		document.contents = append(document.contents, &antigravityReplayContent{raw: []byte(`{}`)})
	}
	content := document.contents[contentIndex]
	if !gjson.ParseBytes(content.raw).IsObject() {
		content.raw = []byte(`{}`)
	}
	content.parts = append(content.parts, part)
	content.partsChanged = true
	document.changed = true
}

func (document *antigravityReplayDocument) setThoughtSignature(contentIndex, partIndex int, signature string) {
	if signature == "" {
		return
	}
	contentIndex = document.resolveContentIndex(contentIndex)
	if part, ok := document.existingPart(contentIndex, partIndex); ok {
		if strings.TrimSpace(gjson.GetBytes(part, "thoughtSignature").String()) != "" {
			return
		}
		updated, errSet := sjson.SetBytes(part, "thoughtSignature", signature)
		if errSet == nil {
			document.updatePart(contentIndex, partIndex, updated)
		}
		return
	}
	part, errMarshal := json.Marshal(map[string]string{"thoughtSignature": signature})
	if errMarshal == nil {
		document.appendPart(contentIndex, part)
	}
}

func (document *antigravityReplayDocument) functionCallPartLocation(callID string) (int, int, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, -1, false
	}
	for contentIndex, content := range document.contents {
		for partIndex, part := range content.parts {
			functionCall := gjson.GetBytes(part, "functionCall")
			if functionCall.Exists() && strings.TrimSpace(functionCall.Get("id").String()) == callID {
				return contentIndex, partIndex, true
			}
		}
	}
	return -1, -1, false
}

func (document *antigravityReplayDocument) functionResponseContentIndex(callID string) (int, bool) {
	callID = strings.TrimSpace(callID)
	if callID == "" {
		return -1, false
	}
	for contentIndex, content := range document.contents {
		for _, part := range content.parts {
			functionResponse := gjson.GetBytes(part, "functionResponse")
			if functionResponse.Exists() && strings.TrimSpace(functionResponse.Get("id").String()) == callID {
				return contentIndex, true
			}
		}
	}
	return -1, false
}

func (document *antigravityReplayDocument) insertContent(index int, raw []byte) bool {
	if index < 0 || index > len(document.contents) {
		return false
	}
	document.contents = append(document.contents, nil)
	copy(document.contents[index+1:], document.contents[index:])
	document.contents[index] = &antigravityReplayContent{raw: raw}
	document.changed = true
	return true
}

func (document *antigravityReplayDocument) mergeFunctionCallPart(itemResult gjson.Result) bool {
	name := strings.TrimSpace(itemResult.Get("name").String())
	args := itemResult.Get("args")
	callID := strings.TrimSpace(itemResult.Get("call_id").String())
	sig := strings.TrimSpace(itemResult.Get("thoughtSignature").String())
	if name == "" || !args.Exists() {
		return false
	}
	if callID != "" {
		if contentIndex, partIndex, exists := document.functionCallPartLocation(callID); exists {
			if sig != "" {
				part, _ := document.existingPart(contentIndex, partIndex)
				if strings.TrimSpace(gjson.GetBytes(part, "thoughtSignature").String()) == "" {
					updated, errSet := sjson.SetBytes(part, "thoughtSignature", sig)
					return errSet == nil && document.updatePart(contentIndex, partIndex, updated)
				}
			}
			return false
		}
		if responseIndex, exists := document.functionResponseContentIndex(callID); exists {
			functionCall := map[string]any{"name": name, "id": callID, "args": args.Value()}
			part := map[string]any{"functionCall": functionCall}
			if sig != "" {
				part["thoughtSignature"] = sig
			}
			content, errMarshal := json.Marshal(map[string]any{"role": "model", "parts": []any{part}})
			return errMarshal == nil && document.insertContent(responseIndex, content)
		}
	}

	contentIndex := document.resolveContentIndex(int(itemResult.Get("contentIndex").Int()))
	partIndex := int(itemResult.Get("partIndex").Int())
	part, exists := document.existingPart(contentIndex, partIndex)
	functionCall := map[string]any{"name": name}
	if callID != "" {
		functionCall["id"] = callID
	}
	functionCall["args"] = args.Value()
	if !exists {
		newPart := map[string]any{"functionCall": functionCall}
		if sig != "" {
			newPart["thoughtSignature"] = sig
		}
		partRaw, errMarshal := json.Marshal(newPart)
		if errMarshal != nil {
			return false
		}
		document.appendPart(contentIndex, partRaw)
		return true
	}

	updatedPart := part
	changed := false
	if sig != "" && strings.TrimSpace(gjson.GetBytes(updatedPart, "thoughtSignature").String()) == "" {
		if updated, errSet := sjson.SetBytes(updatedPart, "thoughtSignature", sig); errSet == nil {
			updatedPart = updated
			changed = true
		}
	}
	if !gjson.GetBytes(updatedPart, "functionCall").Exists() {
		if updated, errSet := sjson.SetBytes(updatedPart, "functionCall", functionCall); errSet == nil {
			updatedPart = updated
			changed = true
		}
	}
	return changed && document.updatePart(contentIndex, partIndex, updatedPart)
}

func (document *antigravityReplayDocument) bytes() ([]byte, bool) {
	if document == nil {
		return nil, false
	}
	if !document.changed {
		return document.payload, false
	}
	contents := make([][]byte, len(document.contents))
	for i, content := range document.contents {
		contentRaw := content.raw
		if content.partsChanged {
			var errSet error
			contentRaw, errSet = sjson.SetRawBytes(contentRaw, "parts", internalpayload.BuildRaw(content.parts))
			if errSet != nil {
				return document.payload, false
			}
		}
		contents[i] = contentRaw
	}
	out, errSet := sjson.SetRawBytes(document.payload, "request.contents", internalpayload.BuildRaw(contents))
	if errSet != nil {
		return document.payload, false
	}
	return out, true
}

type antigravityReasoningReplayAccumulator struct {
	scope          antigravityReasoningReplayScope
	requestPayload []byte
	items          [][]byte
	seenFC         map[string]bool
	contentIndex   int
	nextPartIndex  int
}

func newAntigravityReasoningReplayAccumulator(scope antigravityReasoningReplayScope, requestPayload []byte) *antigravityReasoningReplayAccumulator {
	if !scope.valid() {
		return nil
	}
	contentIndex, basePartIndex := antigravityReasoningReplayPendingModelContentIndex(requestPayload)
	return &antigravityReasoningReplayAccumulator{
		scope:          scope,
		requestPayload: append([]byte(nil), requestPayload...),
		seenFC:         make(map[string]bool),
		contentIndex:   contentIndex,
		nextPartIndex:  basePartIndex,
	}
}

func (a *antigravityReasoningReplayAccumulator) ObserveSSELine(line []byte) {
	if a == nil {
		return
	}
	payload := helps.JSONPayload(line)
	if payload == nil {
		return
	}
	a.observeResponsePayload(payload)
}

func (a *antigravityReasoningReplayAccumulator) observeResponsePayload(payload []byte) {
	parts := gjson.GetBytes(payload, "response.candidates.0.content.parts")
	if !parts.IsArray() {
		return
	}
	parts.ForEach(func(_, part gjson.Result) bool {
		pi := a.nextPartIndex
		a.nextPartIndex++
		sig := antigravityNativePartThoughtSignature(part)
		if fc := part.Get("functionCall"); fc.Exists() {
			keys := antigravityReplayToolCallKeysFromPart(fc)
			for _, k := range keys {
				if a.seenFC[k] {
					return true
				}
			}
			for _, k := range keys {
				a.seenFC[k] = true
			}
			item := buildAntigravityFunctionCallPartItem(a.contentIndex, pi, fc, sig)
			if len(item) > 0 {
				a.items = append(a.items, item)
			}
			return true
		}
		if sig != "" {
			item := buildAntigravityThoughtSignatureItem(a.contentIndex, pi, sig)
			a.items = append(a.items, item)
		}
		return true
	})
}

func buildAntigravityThoughtSignatureItem(contentIndex, partIndex int, signature string) []byte {
	return []byte(fmt.Sprintf(`{"type":"thought_signature","thoughtSignature":%q,"contentIndex":%d,"partIndex":%d}`,
		signature, contentIndex, partIndex))
}

func buildAntigravityFunctionCallPartItem(contentIndex, partIndex int, fc gjson.Result, signature string) []byte {
	item := map[string]any{
		"type":         "function_call_part",
		"contentIndex": contentIndex,
		"partIndex":    partIndex,
		"name":         fc.Get("name").String(),
	}
	if id := strings.TrimSpace(fc.Get("id").String()); id != "" {
		item["call_id"] = id
	}
	if args := fc.Get("args"); args.Exists() {
		if args.Type == gjson.String {
			item["args"] = args.String()
		} else {
			item["args"] = json.RawMessage(args.Raw)
		}
	}
	if signature != "" {
		item["thoughtSignature"] = signature
	}
	raw, err := json.Marshal(item)
	if err != nil {
		return nil
	}
	return raw
}

func (a *antigravityReasoningReplayAccumulator) Flush(ctx context.Context) {
	if a == nil || !a.scope.valid() || len(a.items) == 0 {
		return
	}
	if !internalcache.CacheAntigravityReasoningReplayItemsBestEffort(ctx, a.scope.modelName, a.scope.sessionKey, a.items) {
		_ = internalcache.DeleteAntigravityReasoningReplayItemRequired(ctx, a.scope.modelName, a.scope.sessionKey)
	}
}

func cacheAntigravityReasoningReplayFromResponse(ctx context.Context, scope antigravityReasoningReplayScope, requestPayload, body []byte) {
	if !scope.valid() || len(body) == 0 {
		return
	}
	acc := newAntigravityReasoningReplayAccumulator(scope, requestPayload)
	acc.observeResponsePayload(body)
	acc.Flush(ctx)
}

func applyAntigravityNativeSignatureReplayIfNeeded(modelName string, payload []byte) []byte {
	if antigravityUsesReasoningReplayCache(modelName) {
		return payload
	}
	// Native per-part signature replay is not on upstream/dev; Gemini uses HOME replay only.
	return payload
}

func antigravityUsesReasoningReplayCache(modelName string) bool {
	modelName = strings.ToLower(modelName)
	if strings.Contains(modelName, "claude") {
		return false
	}
	return strings.Contains(modelName, "gemini") || strings.Contains(modelName, "flash") || strings.Contains(modelName, "agent")
}

func antigravityNativePartThoughtSignature(part gjson.Result) string {
	for _, path := range []string{"thoughtSignature", "thought_signature", "extra_content.google.thought_signature"} {
		if signature := strings.TrimSpace(part.Get(path).String()); signature != "" {
			return signature
		}
	}
	return ""
}
