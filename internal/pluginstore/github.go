package pluginstore

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/httpfetch"
	log "github.com/sirupsen/logrus"
)

const (
	userAgent                     = "CLIProxyAPI"
	pluginRegistryMaxBytes  int64 = 4 << 20
	releaseMetadataMaxBytes       = 1 << 20
	pluginChecksumMaxBytes        = 256 << 10
	pluginArchiveMaxBytes         = 256 << 20
)

// HTTPDoer abstracts the HTTP client used to execute requests.
type HTTPDoer = httpfetch.Doer

type Client struct {
	HTTPClient  HTTPDoer
	RegistryURL string
	UserAgent   string
}

type Release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

func (c Client) FetchRegistry(ctx context.Context) (Registry, error) {
	registryURL := strings.TrimSpace(c.RegistryURL)
	if registryURL == "" {
		registryURL = DefaultRegistryURL
	}
	data, errDownload := c.get(ctx, registryURL, "application/json", pluginRegistryMaxBytes)
	if errDownload != nil {
		return Registry{}, errDownload
	}
	registry, errParse := ParseRegistry(data)
	if errParse != nil {
		return Registry{}, errParse
	}
	return registry, nil
}

// FetchLatestRelease returns the latest published release of the plugin's
// GitHub repository, mirroring the WebUI panel update check.
func (c Client) FetchLatestRelease(ctx context.Context, plugin Plugin) (Release, error) {
	owner, repo, errRepository := GitHubRepositoryParts(plugin.Repository)
	if errRepository != nil {
		return Release{}, errRepository
	}
	releaseURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/releases/latest",
		url.PathEscape(owner),
		url.PathEscape(repo),
	)
	data, errDownload := c.get(ctx, releaseURL, "application/vnd.github+json", releaseMetadataMaxBytes)
	if errDownload != nil {
		return Release{}, errDownload
	}
	var release Release
	if errDecode := json.Unmarshal(data, &release); errDecode != nil {
		return Release{}, fmt.Errorf("decode release: %w", errDecode)
	}
	return release, nil
}

// FetchReleaseByTag returns a published release by its exact GitHub tag.
func (c Client) FetchReleaseByTag(ctx context.Context, plugin Plugin, tag string) (Release, error) {
	owner, repo, errRepository := GitHubRepositoryParts(plugin.Repository)
	if errRepository != nil {
		return Release{}, errRepository
	}
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return Release{}, fmt.Errorf("release tag is required")
	}
	releaseURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/%s/releases/tags/%s",
		url.PathEscape(owner),
		url.PathEscape(repo),
		url.PathEscape(tag),
	)
	data, errDownload := c.get(ctx, releaseURL, "application/vnd.github+json", releaseMetadataMaxBytes)
	if errDownload != nil {
		return Release{}, errDownload
	}
	var release Release
	if errDecode := json.Unmarshal(data, &release); errDecode != nil {
		return Release{}, fmt.Errorf("decode release: %w", errDecode)
	}
	return release, nil
}

// ReleaseVersion derives the plugin version from the release tag, stripping a
// leading "v"/"V" and validating the result.
func ReleaseVersion(release Release) (string, error) {
	version := normalizeVersion(release.TagName)
	if !validPluginVersion(version) {
		return "", fmt.Errorf("invalid release tag %q", release.TagName)
	}
	return version, nil
}

func (c Client) DownloadAsset(ctx context.Context, asset ReleaseAsset) ([]byte, error) {
	if strings.TrimSpace(asset.BrowserDownloadURL) == "" {
		return nil, fmt.Errorf("asset %q missing browser_download_url", asset.Name)
	}
	maxBytes := int64(pluginArchiveMaxBytes)
	if strings.EqualFold(strings.TrimSpace(asset.Name), "checksums.txt") {
		maxBytes = pluginChecksumMaxBytes
	}
	if asset.Size < 0 {
		return nil, fmt.Errorf("asset %q has invalid size %d", asset.Name, asset.Size)
	}
	if asset.Size > maxBytes {
		return nil, fmt.Errorf("asset %q size %d exceeds maximum allowed size of %d bytes", asset.Name, asset.Size, maxBytes)
	}
	return c.get(ctx, asset.BrowserDownloadURL, "application/octet-stream", maxBytes)
}

func (c Client) get(ctx context.Context, requestURL string, accept string, maxBytes int64) ([]byte, error) {
	headers := map[string]string{
		"Accept":          accept,
		"Accept-Encoding": "identity",
		"User-Agent":      c.userAgent(),
	}
	if token := gitHubAPIToken(requestURL); token != "" {
		headers["Authorization"] = "Bearer " + token
	}
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodGet, requestURL, nil)
	if errRequest != nil {
		return nil, fmt.Errorf("create request: %w", errRequest)
	}
	for key, value := range headers {
		if value != "" {
			req.Header.Set(key, value)
		}
	}

	resp, errDo := c.httpClient().Do(req)
	if errDo != nil {
		return nil, fmt.Errorf("request failed: %w", errDo)
	}
	if resp == nil || resp.Body == nil {
		return nil, fmt.Errorf("request returned an empty response")
	}
	defer func() {
		if errClose := resp.Body.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close plugin store response body")
		}
	}()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("unexpected status %d: %s", resp.StatusCode, httpfetch.ErrorBodyMetadata(resp.Header.Get("Content-Type"), body))
	}
	if resp.ContentLength > maxBytes {
		return nil, fmt.Errorf("response content length %d exceeds maximum allowed size of %d bytes", resp.ContentLength, maxBytes)
	}
	data, errRead := httpfetch.ReadBytes(resp.Body, maxBytes)
	if errRead != nil {
		return nil, fmt.Errorf("read response: %w", errRead)
	}
	return data, nil
}

// gitHubAPIToken returns the optional GitHub token for GitHub API requests to
// raise the unauthenticated rate limit, mirroring the management asset updater.
func gitHubAPIToken(requestURL string) string {
	parsed, errParse := url.Parse(requestURL)
	if errParse != nil || !strings.EqualFold(parsed.Host, "api.github.com") {
		return ""
	}
	gitURL := strings.ToLower(strings.TrimSpace(os.Getenv("GITSTORE_GIT_URL")))
	if !strings.Contains(gitURL, "github.com") {
		return ""
	}
	return strings.TrimSpace(os.Getenv("GITSTORE_GIT_TOKEN"))
}

func (c Client) httpClient() HTTPDoer {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Client) userAgent() string {
	if strings.TrimSpace(c.UserAgent) != "" {
		return strings.TrimSpace(c.UserAgent)
	}
	return userAgent
}

func SelectReleaseAssets(release Release, id, version, goos, goarch string) (ReleaseAsset, ReleaseAsset, error) {
	archiveName := ArchiveName(id, version, goos, goarch)
	var archiveAsset ReleaseAsset
	var checksumAsset ReleaseAsset
	for _, asset := range release.Assets {
		switch strings.TrimSpace(asset.Name) {
		case archiveName:
			archiveAsset = asset
		case "checksums.txt":
			checksumAsset = asset
		}
	}
	if strings.TrimSpace(archiveAsset.Name) == "" {
		return ReleaseAsset{}, ReleaseAsset{}, fmt.Errorf("release asset %s not found", archiveName)
	}
	if strings.TrimSpace(checksumAsset.Name) == "" {
		return ReleaseAsset{}, ReleaseAsset{}, fmt.Errorf("release asset checksums.txt not found")
	}
	return archiveAsset, checksumAsset, nil
}

func ArchiveName(id, version, goos, goarch string) string {
	return fmt.Sprintf(
		"%s_%s_%s_%s.zip",
		strings.TrimSpace(id),
		strings.TrimSpace(version),
		strings.TrimSpace(goos),
		strings.TrimSpace(goarch),
	)
}
