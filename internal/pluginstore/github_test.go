package pluginstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSelectReleaseAssets(t *testing.T) {
	t.Parallel()

	release := Release{Assets: []ReleaseAsset{
		{Name: "sample-provider_0.1.0_darwin_arm64.zip", BrowserDownloadURL: "https://example.com/sample-provider.zip"},
		{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums.txt"},
	}}
	archiveAsset, checksumAsset, errSelect := SelectReleaseAssets(release, "sample-provider", "0.1.0", "darwin", "arm64")
	if errSelect != nil {
		t.Fatalf("SelectReleaseAssets() error = %v", errSelect)
	}
	if archiveAsset.BrowserDownloadURL != "https://example.com/sample-provider.zip" {
		t.Fatalf("archive URL = %q", archiveAsset.BrowserDownloadURL)
	}
	if checksumAsset.BrowserDownloadURL != "https://example.com/checksums.txt" {
		t.Fatalf("checksum URL = %q", checksumAsset.BrowserDownloadURL)
	}
}

func TestSelectReleaseAssetsRejectsMissingAssets(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		release Release
		wantErr string
	}{
		{
			name: "missing zip",
			release: Release{Assets: []ReleaseAsset{
				{Name: "checksums.txt", BrowserDownloadURL: "https://example.com/checksums.txt"},
			}},
			wantErr: "sample-provider_0.1.0_darwin_arm64.zip",
		},
		{
			name: "missing checksum",
			release: Release{Assets: []ReleaseAsset{
				{Name: "sample-provider_0.1.0_darwin_arm64.zip", BrowserDownloadURL: "https://example.com/sample-provider.zip"},
			}},
			wantErr: "checksums.txt",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, _, errSelect := SelectReleaseAssets(tt.release, "sample-provider", "0.1.0", "darwin", "arm64")
			if errSelect == nil {
				t.Fatal("SelectReleaseAssets() error = nil")
			}
			if !strings.Contains(errSelect.Error(), tt.wantErr) {
				t.Fatalf("SelectReleaseAssets() error = %v, want substring %q", errSelect, tt.wantErr)
			}
		})
	}
}

func TestReleaseVersion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		tagName string
		want    string
		wantErr bool
	}{
		{name: "v prefix", tagName: "v1.2.3", want: "1.2.3"},
		{name: "no prefix", tagName: "0.1.0", want: "0.1.0"},
		{name: "whitespace", tagName: " v2.0.0 ", want: "2.0.0"},
		{name: "empty", tagName: "", wantErr: true},
		{name: "non numeric", tagName: "latest", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			version, errVersion := ReleaseVersion(Release{TagName: tt.tagName})
			if tt.wantErr {
				if errVersion == nil {
					t.Fatalf("ReleaseVersion(%q) error = nil", tt.tagName)
				}
				return
			}
			if errVersion != nil {
				t.Fatalf("ReleaseVersion(%q) error = %v", tt.tagName, errVersion)
			}
			if version != tt.want {
				t.Fatalf("ReleaseVersion(%q) = %q, want %q", tt.tagName, version, tt.want)
			}
		})
	}
}

func TestParseChecksumsAndVerifyChecksum(t *testing.T) {
	t.Parallel()

	data := []byte("zip-data")
	sum := sha256.Sum256(data)
	checksumText := hex.EncodeToString(sum[:]) + "  sample-provider_0.1.0_darwin_arm64.zip\n"
	checksums, errParse := ParseChecksums([]byte(checksumText))
	if errParse != nil {
		t.Fatalf("ParseChecksums() error = %v", errParse)
	}
	if errVerify := VerifyChecksum("sample-provider_0.1.0_darwin_arm64.zip", data, checksums); errVerify != nil {
		t.Fatalf("VerifyChecksum() error = %v", errVerify)
	}
}

func TestVerifyChecksumRejectsMissingAndMismatch(t *testing.T) {
	t.Parallel()

	sum := sha256.Sum256([]byte("zip-data"))
	checksums := map[string]string{"sample-provider.zip": hex.EncodeToString(sum[:])}
	if errVerify := VerifyChecksum("missing.zip", []byte("zip-data"), checksums); errVerify == nil {
		t.Fatal("VerifyChecksum() missing checksum error = nil")
	}
	if errVerify := VerifyChecksum("sample-provider.zip", []byte("other"), checksums); errVerify == nil {
		t.Fatal("VerifyChecksum() mismatch error = nil")
	}
}

func TestDownloadAssetRejectsDeclaredSizeBeforeRequest(t *testing.T) {
	t.Parallel()

	doer := &recordingHTTPDoer{}
	_, errDownload := (Client{HTTPClient: doer}).DownloadAsset(context.Background(), ReleaseAsset{
		Name:               "sample-provider.zip",
		BrowserDownloadURL: "https://downloads.example/sample-provider.zip",
		Size:               pluginArchiveMaxBytes + 1,
	})
	if errDownload == nil || !strings.Contains(errDownload.Error(), "asset \"sample-provider.zip\" size") {
		t.Fatalf("DownloadAsset() error = %v, want asset size error", errDownload)
	}
	if doer.calls != 0 {
		t.Fatalf("HTTP calls = %d, want 0", doer.calls)
	}
}

func TestDownloadAssetRejectsContentLengthBeforeRead(t *testing.T) {
	t.Parallel()

	body := &trackingReadCloser{Reader: strings.NewReader("unused")}
	doer := &recordingHTTPDoer{response: &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: pluginChecksumMaxBytes + 1,
		Body:          body,
		Header:        make(http.Header),
	}}
	_, errDownload := (Client{HTTPClient: doer}).DownloadAsset(context.Background(), ReleaseAsset{
		Name:               "checksums.txt",
		BrowserDownloadURL: "https://downloads.example/checksums.txt",
	})
	if errDownload == nil || !strings.Contains(errDownload.Error(), "response content length") {
		t.Fatalf("DownloadAsset() error = %v, want content length error", errDownload)
	}
	if body.read != 0 {
		t.Fatalf("response bytes read = %d, want 0", body.read)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestDownloadAssetErrorDoesNotExposeBody(t *testing.T) {
	t.Parallel()
	const secret = "plugin-store-secret-marker"
	body := &trackingReadCloser{Reader: strings.NewReader(`{"error":"` + secret + `"}`)}
	doer := &recordingHTTPDoer{response: &http.Response{
		StatusCode: http.StatusUnauthorized,
		Body:       body,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
	}}
	_, err := (Client{HTTPClient: doer}).DownloadAsset(context.Background(), ReleaseAsset{
		Name:               "checksums.txt",
		BrowserDownloadURL: "https://downloads.example/checksums.txt",
	})
	if err == nil {
		t.Fatal("DownloadAsset() error = nil")
	}
	if strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), `"sha256":"`) {
		t.Fatalf("unsafe plugin store error: %v", err)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
}

func TestDownloadAssetStopsAtLimitPlusOne(t *testing.T) {
	t.Parallel()

	body := &trackingReadCloser{Reader: bytes.NewReader(bytes.Repeat([]byte{'x'}, pluginChecksumMaxBytes+2))}
	doer := &recordingHTTPDoer{response: &http.Response{
		StatusCode:    http.StatusOK,
		ContentLength: -1,
		Body:          body,
		Header:        make(http.Header),
	}}
	_, errDownload := (Client{HTTPClient: doer}).DownloadAsset(context.Background(), ReleaseAsset{
		Name:               "checksums.txt",
		BrowserDownloadURL: "https://downloads.example/checksums.txt",
	})
	if errDownload == nil || !strings.Contains(errDownload.Error(), "maximum allowed size") {
		t.Fatalf("DownloadAsset() error = %v, want size limit error", errDownload)
	}
	if body.read != pluginChecksumMaxBytes+1 {
		t.Fatalf("response bytes read = %d, want limit+1 (%d)", body.read, pluginChecksumMaxBytes+1)
	}
}

type recordingHTTPDoer struct {
	response *http.Response
	calls    int
}

func (d *recordingHTTPDoer) Do(*http.Request) (*http.Response, error) {
	d.calls++
	if d.response == nil {
		return nil, errors.New("unexpected request")
	}
	return d.response, nil
}

type trackingReadCloser struct {
	io.Reader
	read   int64
	closed bool
}

func (r *trackingReadCloser) Read(buffer []byte) (int, error) {
	n, errRead := r.Reader.Read(buffer)
	r.read += int64(n)
	return n, errRead
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}
