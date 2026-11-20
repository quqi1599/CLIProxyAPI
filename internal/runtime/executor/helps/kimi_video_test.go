package helps

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/failure"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
)

const mp4Header = "\x00\x00\x00\x18ftypmp42\x00\x00\x00\x00mp42isom"

func TestKimiVideoPreparePreservesWireAndRedactsLog(t *testing.T) {
	const remote = "https://videos.example/clip?signature=test"
	const inline = "data:video/mp4;base64,AAAA"
	const file = "ms://test-file"
	const source = `{"tools":[{"description":"video_url","const":9007199254740993}],"messages":[{"role":"tool","content":"https://ignored.example"},{"role":"user","content":[{"type":"text","text":"顺序 🎥"},{"type":"video_url","video_url":{"url":"` + inline + `"}},{"type":"video_url","video_url":{"url":"` + remote + `","processing":{"fps":2}}},{"type":"video_url","video_url":{"url":"` + file + `"}}]}]}`
	clip := mp4Header + strings.Repeat("a", 320<<10)
	calls := 0
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != remote || r.Method != http.MethodGet || r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" {
			t.Fatal("unexpected media download request")
		}
		return &http.Response{StatusCode: 200, ContentLength: -1, Body: io.NopCloser(strings.NewReader(clip))}, nil
	})}
	wire, logBody, expansion, err := prepareKimiChatVideos(t.Context(), []byte(source), client)
	if err != nil || calls != 1 {
		t.Fatalf("video fetch calls=%d err=%v", calls, err)
	}
	want := strings.Replace(source, remote, "data:video/mp4;base64,"+base64.StdEncoding.EncodeToString([]byte(clip)), 1)
	if string(wire) != want {
		t.Fatal("wire bytes outside the video URL changed or the clip was corrupted")
	}
	wantLog := strings.NewReplacer(inline, "<video URL omitted>", remote, "<video URL omitted>", file, "<video URL omitted>").Replace(source)
	if string(logBody) != wantLog {
		t.Fatal("video URLs not correctly redacted")
	}
	if x := payload.ObserveAmplification(int64(len(source)), int64(len(wire)), expansion); !x.OverrideApplied || x.Exceeded {
		t.Fatalf("video expansion rejected: %+v", x)
	}
}

func TestKimiVideoFetchLimits(t *testing.T) {
	const single = `{"type":"video_url","video_url":{"url":"https://video.example/a.mp4"}}`
	for _, tc := range []struct {
		name, part, clip string
		copies, status   int
		length           int64
		wantStatus       int
	}{
		{"unavailable", single, "missing", 1, 404, -1, 400},
		{"wrong-media", single, "HTML content", 1, 200, -1, 400},
		{"partial", single, mp4Header, 1, 206, -1, 400},
		{"declared-length", single, mp4Header, 1, 200, kimiRemoteVideoBytes + 1, 413},
		{"aggregate-unknown-length", single, mp4Header + strings.Repeat("a", kimiRemoteVideoBytes/2), 2, 200, -1, 413},
		{"count", single, mp4Header, kimiRemoteVideos + 1, 200, -1, 413},
		{"plaintext-url", strings.Replace(single, "https://", "http://", 1), mp4Header, 1, 200, -1, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parts := strings.TrimSuffix(strings.Repeat(tc.part+",", tc.copies), ",")
			body := []byte(`{"messages":[{"role":"user","content":[` + parts + `]}]}`)
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, ContentLength: tc.length, Body: io.NopCloser(strings.NewReader(tc.clip))}, nil
			})}
			wire, logBody, _, err := prepareKimiChatVideos(t.Context(), body, client)
			var f *failure.Failure
			if !errors.As(err, &f) || f.HTTPStatus != tc.wantStatus || f.Scope != failure.ScopeRequest || wire != nil || logBody != nil {
				t.Fatalf("expected terminal %d before inference, got %v", tc.wantStatus, err)
			}
		})
	}
}

func TestKimiVideoRedirectAndCancellation(t *testing.T) {
	check := kimiVideoClient.CheckRedirect
	req, _ := http.NewRequest(http.MethodGet, "https://cdn.example/clip", nil)
	req.Header.Set("Referer", "https://origin.example/signed")
	if err := check(req, []*http.Request{req}); err != nil || req.Header.Get("Referer") != "" {
		t.Fatal("redirect retained original URL")
	}
	req.URL.Scheme = "http"
	if err := check(req, []*http.Request{req}); err == nil {
		t.Fatal("downgrade allowed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })}
	if _, err := fetchKimiVideo(ctx, client, "https://cdn.example/clip", 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation lost: %v", err)
	}
	body := []byte(`{"tools":[{"type":"video_url","video_url":{"url":"https://ignored.example"}}],"messages":[{"role":"user","content":"video_url"}]}`)
	got, logBody, _, err := prepareKimiChatVideos(ctx, body, client)
	if err != nil || !bytes.Equal(body, got) || !bytes.Equal(body, logBody) {
		t.Fatal("non-media was fetched or changed")
	}
}

func TestKimiVideoRetriesOnlyEarlyEOF(t *testing.T) {
	for _, tc := range []struct {
		name        string
		firstError  error
		firstStatus int
		wantCalls   int
		wantSuccess bool
	}{
		{"early-eof", io.EOF, 0, 2, true},
		{"early-unexpected-eof", io.ErrUnexpectedEOF, 0, 2, true},
		{"other-error", errors.New("invalid certificate"), 0, 1, false},
		{"origin-error", nil, 503, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 && tc.firstError != nil {
					return nil, tc.firstError
				}
				status := 200
				if calls == 1 {
					status = tc.firstStatus
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(mp4Header))}, nil
			})}
			_, err := fetchKimiVideo(t.Context(), client, "https://media.example/clip", 512)
			if calls != tc.wantCalls || (err == nil) != tc.wantSuccess {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
