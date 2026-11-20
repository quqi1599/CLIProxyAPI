package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

const (
	kimiRemoteVideoBytes = 8 << 20
	kimiRemoteVideos     = 4
)

var kimiVideoSlots = make(chan struct{}, 4)
var kimiVideoClient = &http.Client{
	Transport: NewPublicMediaTransport(),
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !kimiVideoURLAllowed(req.URL) {
			return errors.New("invalid video redirect")
		}
		req.Header.Del("Referer")
		return nil
	},
}

func kimiVideoURLAllowed(u *url.URL) bool {
	return PublicMediaURLAllowed(u) && u.Scheme == "https" && (u.Port() == "" || u.Port() == "443")
}

func IsKimiVideoBase(base string) bool {
	u, err := url.Parse(base)
	if err != nil || !IsKimiCodeEndpoint(u) || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	p := strings.TrimRight(u.EscapedPath(), "/")
	return (p == "/coding" || p == "/coding/v1") && (u.Port() == "" || u.Port() == "443")
}

func KimiVideoModel(model string) bool {
	switch strings.TrimSuffix(strings.ToLower(thinking.ParseSuffix(model).ModelName), "[1m]") {
	case "k3", "k3.1-pro", "k3.1-pro-highspeed":
		return true
	}
	return false
}

// Walk only typed Chat content parts. URLs in text, tool output or schemas are data.
func kimiVideoParts(body []byte) iter.Seq[gjson.Result] {
	return func(yield func(gjson.Result) bool) {
		messages := gjson.GetBytes(body, "messages")
		if !messages.IsArray() {
			return
		}
		messages.ForEach(func(_, msg gjson.Result) bool {
			if msg.Get("role").Str != "user" {
				return true
			}
			content := msg.Get("content")
			more := true
			if content.IsArray() {
				content.ForEach(func(_, part gjson.Result) bool {
					if part.Get("type").Str == "video_url" {
						more = yield(part)
					}
					return more
				})
			}
			return more
		})
	}
}

func IsKimiVideoChat(req cliproxyexecutor.Request, opts cliproxyexecutor.Options) bool {
	if opts.SourceFormat == sdktranslator.FormatOpenAI && opts.Alt == "" && KimiVideoModel(req.Model) {
		for range kimiVideoParts(req.Payload) {
			return true
		}
	}
	return false
}

// PrepareKimiChatVideos returns separate upstream and diagnostic payloads. Only
// the URL tokens in messages[].content[] are replaced, preserving all other JSON.
func PrepareKimiChatVideos(ctx context.Context, body []byte) ([]byte, []byte, payload.AmplificationOverride, error) {
	return prepareKimiChatVideos(ctx, body, kimiVideoClient)
}

func prepareKimiChatVideos(ctx context.Context, body []byte, client *http.Client) ([]byte, []byte, payload.AmplificationOverride, error) {
	var expansion payload.AmplificationOverride
	var wire, diagnostic bytes.Buffer
	wirePos, logPos, remoteCount := 0, 0, 0
	remaining := int64(kimiRemoteVideoBytes)
	for part := range kimiVideoParts(body) {
		v := part.Get("video_url.url")
		if v.Type != gjson.String || v.Str == "" {
			return nil, nil, expansion, kimiVideoFailure(400, "Kimi video_url.url must contain a video URL")
		}
		end := v.Index + len(v.Raw)
		diagnostic.Write(body[logPos:v.Index])
		diagnostic.WriteString(`"<video URL omitted>"`)
		logPos = end
		if strings.HasPrefix(v.Str, "data:") || strings.HasPrefix(v.Str, "ms://") {
			continue
		}
		remoteCount++
		if remoteCount > kimiRemoteVideos || remaining == 0 {
			return nil, nil, expansion, kimiVideoFailure(413, "Kimi accepts up to four remote MP4 files totaling 8 MiB per request")
		}
		if remoteCount == 1 {
			select {
			case kimiVideoSlots <- struct{}{}:
				defer func() { <-kimiVideoSlots }()
			default:
				return nil, nil, expansion, kimiVideoFailure(503, "Video downloads are busy. Try again or send an inline MP4")
			}
		}
		data, err := fetchKimiVideo(ctx, client, v.Str, remaining)
		if err != nil {
			return nil, nil, expansion, err
		}
		remaining -= int64(len(data))
		wire.Write(body[wirePos:v.Index])
		wire.WriteString(`"data:video/mp4;base64,`)
		wire.Write(base64.StdEncoding.AppendEncode(nil, data))
		wire.WriteByte('"')
		wirePos = end
	}
	logBody := body
	if logPos > 0 {
		diagnostic.Write(body[logPos:])
		logBody = diagnostic.Bytes()
	}
	if wirePos > 0 {
		wire.Write(body[wirePos:])
		// Permit only the bytes actually added by these bounded video downloads.
		expansion = payload.AmplificationOverride{PolicyID: "openai_compat.kimi_video_inline", MaxExpansionBytes: int64(max(1, wire.Len()-len(body)))}
		body = wire.Bytes()
	}
	return body, logBody, expansion, nil
}

func fetchKimiVideo(ctx context.Context, client *http.Client, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || !kimiVideoURLAllowed(u) {
		return nil, kimiVideoFailure(400, "Kimi remote video requires a public HTTPS MP4 URL on port 443")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, kimiVideoFailure(400, "Invalid Kimi video URL")
	}
	req.Header.Set("Accept", "video/mp4")
	resp, err := client.Do(req)
	// Retry a read-only media GET once when the peer closes before any HTTP
	// response. Failures after a response and inference calls are not replayed.
	if resp == nil && ctx.Err() == nil && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
		resp, err = client.Do(req.Clone(ctx))
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, kimiVideoFailure(400, "Could not download the remote Kimi video. Check the link or send an inline MP4")
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK || (resp.Header.Get("Content-Encoding") != "" && !strings.EqualFold(resp.Header.Get("Content-Encoding"), "identity")) {
		return nil, kimiVideoFailure(400, "Kimi remote video must return a complete uncompressed MP4 response")
	}
	if resp.ContentLength > limit {
		return nil, kimiVideoFailure(413, "Kimi remote video exceeds the 8 MiB request limit")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, kimiVideoFailure(400, "Could not read the complete remote Kimi video")
	}
	if int64(len(data)) > limit {
		return nil, kimiVideoFailure(413, "Kimi remote video exceeds the 8 MiB request limit")
	}
	if http.DetectContentType(data) != "video/mp4" {
		return nil, kimiVideoFailure(400, "Kimi remote video content must be an MP4 file")
	}
	return data, nil
}

func kimiVideoFailure(status int, message string) *failure.Failure {
	kind, code, typ := failure.InvalidRequest, "kimi_video_input_invalid", "invalid_request_error"
	if status == http.StatusRequestEntityTooLarge {
		kind, code = failure.RequestTooLarge, "kimi_video_input_too_large"
	}
	if status == http.StatusServiceUnavailable {
		kind, code, typ = failure.RateLimited, "kimi_video_download_busy", "server_error"
	}
	return &failure.Failure{
		Kind: kind, Scope: failure.ScopeRequest, HTTPStatus: status, OuterStatus: status,
		ProviderCode: code, SemanticCode: code, SemanticType: typ,
		StreamPhase: failure.StreamPhaseBeforeOutput, PublicMessage: message,
	}
}
