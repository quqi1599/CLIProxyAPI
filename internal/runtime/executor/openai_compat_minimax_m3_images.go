package executor

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	miniMaxM3ImageInlineMaxBytes     = 5 << 20
	miniMaxM3ImageInlineMaxWireBytes = 8 << 20
	miniMaxM3ImageInlineMaxImages    = 4
	// Four 5 MiB images expand to at most about 26.7 MiB after base64 encoding.
	miniMaxM3ImageInlineMaxExpansionBytes int64 = 28 << 20
)

func miniMaxM3InlineAmplificationOverride(applied bool) internalpayload.AmplificationOverride {
	if !applied {
		return internalpayload.AmplificationOverride{}
	}
	return internalpayload.AmplificationOverride{
		PolicyID:          "openai_compat.minimax_m3_image_inline",
		MaxExpansionBytes: miniMaxM3ImageInlineMaxExpansionBytes,
	}
}

var (
	fetchMiniMaxM3ImageURL = fetchMiniMaxM3ImageURLDefault

	miniMaxM3ImageHTTPClient = &http.Client{
		Transport:     helps.NewPublicMediaTransport(),
		CheckRedirect: miniMaxM3ImageCheckRedirect,
	}
)

type miniMaxM3ImageInlineState struct {
	ctx       context.Context
	attempted int
}

func inlineMiniMaxM3RemoteImageURLs(ctx context.Context, payload []byte, profile openAICompatProfile, model string) ([]byte, bool) {
	if !shouldInlineMiniMaxM3RemoteImageURLs(profile, model) || len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload, false
	}

	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload, false
	}

	state := &miniMaxM3ImageInlineState{ctx: ctx}
	if !state.walk(root) {
		return payload, false
	}

	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload, false
	}
	return out, true
}

func shouldInlineMiniMaxM3RemoteImageURLs(profile openAICompatProfile, model string) bool {
	if config.NormalizeOpenAICompatibilityKind(profile.Kind) != "minimax" {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(model), "MiniMax-M3")
}

func (s *miniMaxM3ImageInlineState) walk(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := s.inlineImagePart(typed)
		for _, child := range typed {
			if s.walk(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if s.walk(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}

func (s *miniMaxM3ImageInlineState) inlineImagePart(part map[string]any) bool {
	partType := strings.ToLower(strings.TrimSpace(compatStringValue(part["type"])))
	switch partType {
	case "image_url":
		return s.inlineOpenAIImageURLPart(part, "image_url")
	case "input_image":
		if s.inlineOpenAIImageURLPart(part, "image_url") {
			return true
		}
		return s.inlineOpenAIImageURLPart(part, "url")
	default:
		return false
	}
}

func (s *miniMaxM3ImageInlineState) inlineOpenAIImageURLPart(part map[string]any, field string) bool {
	rawValue, ok := part[field]
	if !ok {
		return false
	}
	switch typed := rawValue.(type) {
	case string:
		if dataURL, okInline := s.inlineURL(typed); okInline {
			part[field] = dataURL
			return true
		}
	case map[string]any:
		urlValue := strings.TrimSpace(compatStringValue(typed["url"]))
		if dataURL, okInline := s.inlineURL(urlValue); okInline {
			typed["url"] = dataURL
			return true
		}
	}
	return false
}

func (s *miniMaxM3ImageInlineState) inlineURL(rawURL string) (string, bool) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" || s.attempted >= miniMaxM3ImageInlineMaxImages {
		return "", false
	}
	if strings.HasPrefix(strings.ToLower(rawURL), "data:") {
		return "", false
	}
	s.attempted++
	mediaType, data, ok := fetchMiniMaxM3ImageURL(s.ctx, rawURL)
	if !ok {
		return "", false
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), true
}

func fetchMiniMaxM3ImageURLDefault(ctx context.Context, rawURL string) (string, []byte, bool) {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || !helps.PublicMediaURLAllowed(parsed) {
		return "", nil, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", nil, false
	}
	req.Header.Set("Accept", "image/png,image/jpeg,image/webp,image/gif")
	req.Header.Set("User-Agent", "cli-proxy-minimax-m3-image-fetch")

	resp, err := miniMaxM3ImageHTTPClient.Do(req)
	if err != nil {
		return "", nil, false
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		if errClose := resp.Body.Close(); errClose != nil {
			log.Errorf("openai compat executor: close minimax m3 image fetch body error: %v", errClose)
		}
		return "", nil, false
	}

	data, err := helps.ReadBoundedUpstreamHTTPResponse(resp, helps.UpstreamBodyLimits{
		SuccessBytes:     miniMaxM3ImageInlineMaxBytes,
		SuccessWireBytes: miniMaxM3ImageInlineMaxWireBytes,
	})
	if err != nil || len(data) == 0 {
		return "", nil, false
	}
	mediaType := miniMaxM3AllowedImageMediaType(resp.Header.Get("Content-Type"), data)
	if mediaType == "" {
		return "", nil, false
	}
	return mediaType, data, true
}

func miniMaxM3ImageCheckRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 3 {
		return fmt.Errorf("too many redirects")
	}
	if req == nil || req.URL == nil || !helps.PublicMediaURLAllowed(req.URL) {
		return fmt.Errorf("blocked image redirect")
	}
	return nil
}

func miniMaxM3AllowedImageMediaType(contentType string, data []byte) string {
	if parsed, _, err := mime.ParseMediaType(strings.TrimSpace(contentType)); err == nil {
		if normalized := normalizeMiniMaxM3ImageMediaType(parsed); normalized != "" {
			return normalized
		}
	}
	return normalizeMiniMaxM3ImageMediaType(http.DetectContentType(data))
}

func normalizeMiniMaxM3ImageMediaType(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		return "image/jpeg"
	case "image/png":
		return "image/png"
	case "image/webp":
		return "image/webp"
	case "image/gif":
		return "image/gif"
	default:
		return ""
	}
}

func redactOpenAICompatImageDataURLsForLog(payload []byte) []byte {
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return payload
	}
	var root any
	if err := json.Unmarshal(payload, &root); err != nil {
		return payload
	}
	if !redactImageDataURLs(root) {
		return payload
	}
	out, err := json.Marshal(root)
	if err != nil || !gjson.ValidBytes(out) {
		return payload
	}
	return out
}

func redactImageDataURLs(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		changed := false
		for key, child := range typed {
			if str, ok := child.(string); ok {
				if _, _, okData := util.ParseDataURL(str); okData {
					typed[key] = "<image data URL omitted>"
					changed = true
					continue
				}
			}
			if redactImageDataURLs(child) {
				changed = true
			}
		}
		return changed
	case []any:
		changed := false
		for _, child := range typed {
			if redactImageDataURLs(child) {
				changed = true
			}
		}
		return changed
	default:
		return false
	}
}
