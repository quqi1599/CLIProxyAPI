package pluginstore

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestInstallBlocksLoadedWindowsPlugin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		goos        string
		loaded      bool
		wantBlocked bool
	}{
		{name: "windows loaded", goos: "windows", loaded: true, wantBlocked: true},
		{name: "windows not loaded", goos: "windows", loaded: false, wantBlocked: false},
		{name: "linux loaded", goos: "linux", loaded: true, wantBlocked: false},
		{name: "darwin loaded", goos: "darwin", loaded: true, wantBlocked: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errInstall := Client{HTTPClient: failingHTTPDoer{}}.Install(context.Background(), testPlugin(), InstallOptions{
				PluginsDir:   t.TempDir(),
				GOOS:         tt.goos,
				GOARCH:       "amd64",
				PluginLoaded: func() bool { return tt.loaded },
			})
			if errInstall == nil {
				t.Fatal("Install() error = nil")
			}
			if gotBlocked := errors.Is(errInstall, ErrLoadedPluginLocked); gotBlocked != tt.wantBlocked {
				t.Fatalf("Install() error = %v, blocked = %v, want %v", errInstall, gotBlocked, tt.wantBlocked)
			}
		})
	}
}

func TestInstallArchiveBlocksLoadedWindowsPluginBeforeWrite(t *testing.T) {
	t.Parallel()

	_, errInstall := InstallArchive(makeZip(t, map[string]string{
		"sample-provider.dll": "library-data",
	}), testPlugin(), InstallOptions{
		PluginsDir:   t.TempDir(),
		GOOS:         "windows",
		GOARCH:       "amd64",
		PluginLoaded: func() bool { return true },
	})
	if !errors.Is(errInstall, ErrLoadedPluginLocked) {
		t.Fatalf("InstallArchive() error = %v, want ErrLoadedPluginLocked", errInstall)
	}
}

func TestInstallArchivePreparesLoadedWindowsPluginBeforeWrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetDir := filepath.Join(root, "windows", "amd64")
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	targetPath := filepath.Join(targetDir, "sample-provider.dll")
	if errWrite := os.WriteFile(targetPath, []byte("old"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	loaded := true
	prepared := false

	result, errInstall := InstallArchive(makeZip(t, map[string]string{
		"sample-provider.dll": "new",
	}), testPlugin(), InstallOptions{
		PluginsDir:   root,
		GOOS:         "windows",
		GOARCH:       "amd64",
		PluginLoaded: func() bool { return loaded },
		BeforeWrite: func() error {
			prepared = true
			loaded = false
			return nil
		},
	})
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	if !prepared {
		t.Fatal("BeforeWrite was not called")
	}
	if !result.Overwritten {
		t.Fatal("Overwritten = false, want true")
	}
	data, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "new" {
		t.Fatalf("installed data = %q, want new", data)
	}
}

func TestInstallArchiveSkipsIdenticalLoadedWindowsPlugin(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetDir := filepath.Join(root, "windows", "amd64")
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	targetPath := filepath.Join(targetDir, "sample-provider.dll")
	if errWrite := os.WriteFile(targetPath, []byte("same"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	beforeWriteCalled := false

	result, errInstall := InstallArchive(makeZip(t, map[string]string{
		"sample-provider.dll": "same",
	}), testPlugin(), InstallOptions{
		PluginsDir:   root,
		GOOS:         "windows",
		GOARCH:       "amd64",
		PluginLoaded: func() bool { return true },
		BeforeWrite: func() error {
			beforeWriteCalled = true
			return errors.New("before write should not run")
		},
	})
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	if beforeWriteCalled {
		t.Fatal("BeforeWrite was called for identical artifact")
	}
	if !result.Overwritten {
		t.Fatal("Overwritten = false, want true")
	}
	if !result.Skipped {
		t.Fatal("Skipped = false, want true")
	}
	data, errRead := os.ReadFile(targetPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "same" {
		t.Fatalf("installed data = %q, want same", data)
	}
}

func TestInstallArchiveWritesPlatformPlugin(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	result, errInstall := InstallArchive(makeZip(t, map[string]string{
		"README.md":             "ignored",
		"sample-provider.dylib": "library-data",
	}), testPlugin(), InstallOptions{PluginsDir: root, GOOS: "darwin", GOARCH: "arm64"})
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	wantPath := filepath.Join(root, "darwin", "arm64", "sample-provider.dylib")
	if result.Path != wantPath {
		t.Fatalf("Path = %q, want %q", result.Path, wantPath)
	}
	data, errRead := os.ReadFile(wantPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "library-data" {
		t.Fatalf("installed data = %q", data)
	}
}

func TestInstallArchiveReportsOverwrite(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	targetDir := filepath.Join(root, "darwin", "arm64")
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		t.Fatalf("MkdirAll() error = %v", errMkdir)
	}
	if errWrite := os.WriteFile(filepath.Join(targetDir, "sample-provider.dylib"), []byte("old"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}
	result, errInstall := InstallArchive(makeZip(t, map[string]string{
		"sample-provider.dylib": "new",
	}), testPlugin(), InstallOptions{PluginsDir: root, GOOS: "darwin", GOARCH: "arm64"})
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	if !result.Overwritten {
		t.Fatal("Overwritten = false, want true")
	}
}

func TestInstallArchiveOverwritesRuntimeSelectedPlugin(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	existingPath := filepath.Join(root, "sample-provider"+pluginExtension(runtime.GOOS))
	if errWrite := os.WriteFile(existingPath, []byte("old"), 0o644); errWrite != nil {
		t.Fatalf("WriteFile() error = %v", errWrite)
	}

	result, errInstall := InstallArchive(makeZip(t, map[string]string{
		"sample-provider" + pluginExtension(runtime.GOOS): "new",
	}), testPlugin(), InstallOptions{PluginsDir: root, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH})
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	if result.Path != existingPath {
		t.Fatalf("Path = %q, want selected runtime plugin %q", result.Path, existingPath)
	}
	if !result.Overwritten {
		t.Fatal("Overwritten = false, want true")
	}
	data, errRead := os.ReadFile(existingPath)
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "new" {
		t.Fatalf("installed data = %q, want new", data)
	}
}

func TestInstallArchiveRejectsUnsafeArchives(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		files   map[string]string
		wantErr string
	}{
		{
			name:    "zip slip",
			files:   map[string]string{"../sample-provider.dylib": "library"},
			wantErr: "escapes archive root",
		},
		{
			name:    "absolute path",
			files:   map[string]string{"/sample-provider.dylib": "library"},
			wantErr: "is absolute",
		},
		{
			name:    "nested target",
			files:   map[string]string{"nested/sample-provider.dylib": "library"},
			wantErr: "zip root",
		},
		{
			name:    "extension mismatch",
			files:   map[string]string{"sample-provider.so": "library"},
			wantErr: "sample-provider.dylib",
		},
		{
			name:    "filename mismatch",
			files:   map[string]string{"other.dylib": "library"},
			wantErr: "sample-provider.dylib",
		},
		{
			name:    "missing target",
			files:   map[string]string{"README.md": "library"},
			wantErr: "does not contain",
		},
		{
			name: "multiple targets",
			files: map[string]string{
				"sample-provider.dylib": "library",
				"copy.dylib":            "library",
			},
			wantErr: "sample-provider.dylib",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, errInstall := InstallArchive(makeZip(t, tt.files), testPlugin(), InstallOptions{PluginsDir: t.TempDir(), GOOS: "darwin", GOARCH: "arm64"})
			if errInstall == nil {
				t.Fatal("InstallArchive() error = nil")
			}
			if !strings.Contains(errInstall.Error(), tt.wantErr) {
				t.Fatalf("InstallArchive() error = %v, want substring %q", errInstall, tt.wantErr)
			}
		})
	}
}

func TestInstallArchiveRejectsTooManyEntries(t *testing.T) {
	t.Parallel()

	files := make(map[string]string, pluginArchiveMaxEntries+1)
	files["sample-provider.dylib"] = "library"
	for index := 0; index < pluginArchiveMaxEntries; index++ {
		files[fmt.Sprintf("docs/%03d.txt", index)] = "ignored"
	}
	_, errInstall := InstallArchive(makeZip(t, files), testPlugin(), InstallOptions{
		PluginsDir: t.TempDir(),
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if !errors.Is(errInstall, ErrPluginArchiveTooManyEntries) {
		t.Fatalf("InstallArchive() error = %v, want ErrPluginArchiveTooManyEntries", errInstall)
	}
}

func TestInstallArchiveRejectsUnderreportedEntryCountBeforeZipReader(t *testing.T) {
	t.Parallel()

	files := make(map[string]string, pluginArchiveMaxEntries+1)
	files["sample-provider.dylib"] = "library"
	for index := 0; index < pluginArchiveMaxEntries; index++ {
		files[fmt.Sprintf("docs/%03d.txt", index)] = "ignored"
	}
	archiveData := makeZip(t, files)
	directoryEnd := bytes.LastIndex(archiveData, []byte{'P', 'K', 0x05, 0x06})
	if directoryEnd < 0 {
		t.Fatal("ZIP directory end not found")
	}
	binary.LittleEndian.PutUint16(archiveData[directoryEnd+8:], 1)
	binary.LittleEndian.PutUint16(archiveData[directoryEnd+10:], 1)
	lastDirectoryEntry := bytes.LastIndex(archiveData[:directoryEnd], []byte{'P', 'K', 0x01, 0x02})
	if lastDirectoryEntry < 0 {
		t.Fatal("ZIP directory entry not found")
	}
	binary.LittleEndian.PutUint32(archiveData[directoryEnd+12:], uint32(directoryEnd-lastDirectoryEntry))

	_, errInstall := InstallArchive(archiveData, testPlugin(), InstallOptions{
		PluginsDir: t.TempDir(),
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if !errors.Is(errInstall, ErrPluginArchiveTooManyEntries) {
		t.Fatalf("InstallArchive() error = %v, want ErrPluginArchiveTooManyEntries", errInstall)
	}
}

func TestInstallArchiveRejectsMalformedCentralDirectory(t *testing.T) {
	t.Parallel()

	archiveData := makeZip(t, map[string]string{"sample-provider.dylib": "library"})
	directoryEnd := bytes.LastIndex(archiveData, []byte{'P', 'K', 0x05, 0x06})
	if directoryEnd < 0 {
		t.Fatal("ZIP directory end not found")
	}
	directoryOffset := int(binary.LittleEndian.Uint32(archiveData[directoryEnd+16:]))
	archiveData[directoryOffset] = 0

	_, errInstall := InstallArchive(archiveData, testPlugin(), InstallOptions{
		PluginsDir: t.TempDir(),
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if !errors.Is(errInstall, ErrPluginArchiveInvalidDirectory) {
		t.Fatalf("InstallArchive() error = %v, want ErrPluginArchiveInvalidDirectory", errInstall)
	}
}

func TestInstallArchiveRejectsMissingCentralZip64Extra(t *testing.T) {
	t.Parallel()

	archiveData := makeZip(t, map[string]string{"sample-provider.dylib": "library"})
	directoryEnd := bytes.LastIndex(archiveData, []byte{'P', 'K', 0x05, 0x06})
	if directoryEnd < 0 {
		t.Fatal("ZIP directory end not found")
	}
	directoryOffset := int(binary.LittleEndian.Uint32(archiveData[directoryEnd+16:]))
	binary.LittleEndian.PutUint32(archiveData[directoryOffset+20:], 1<<32-1)

	_, errInstall := InstallArchive(archiveData, testPlugin(), InstallOptions{
		PluginsDir: t.TempDir(),
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if !errors.Is(errInstall, ErrPluginArchiveInvalidDirectory) {
		t.Fatalf("InstallArchive() error = %v, want ErrPluginArchiveInvalidDirectory", errInstall)
	}
}

func TestInstallArchiveAcceptsZip64Directory(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	_, errInstall := InstallArchive(makeZip64(t, map[string]string{
		"sample-provider.dylib": "library",
	}), testPlugin(), InstallOptions{
		PluginsDir: root,
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if errInstall != nil {
		t.Fatalf("InstallArchive() error = %v", errInstall)
	}
	data, errRead := os.ReadFile(filepath.Join(root, "darwin", "arm64", "sample-provider.dylib"))
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "library" {
		t.Fatalf("installed data = %q, want library", data)
	}
}

func TestInstallArchiveRejectsMalformedZip64Directory(t *testing.T) {
	t.Parallel()

	archiveData := makeZip64(t, map[string]string{"sample-provider.dylib": "library"})
	directoryEnd := bytes.LastIndex(archiveData, []byte{'P', 'K', 0x05, 0x06})
	if directoryEnd < zipDirectory64LocatorSize {
		t.Fatal("ZIP64 locator not found")
	}
	archiveData[directoryEnd-zipDirectory64LocatorSize] = 0

	_, errInstall := InstallArchive(archiveData, testPlugin(), InstallOptions{
		PluginsDir: t.TempDir(),
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if !errors.Is(errInstall, ErrPluginArchiveInvalidDirectory) {
		t.Fatalf("InstallArchive() error = %v, want ErrPluginArchiveInvalidDirectory", errInstall)
	}
}

func TestReadTargetLibraryRejectsDeclaredUncompressedSize(t *testing.T) {
	t.Parallel()

	reader := &zip.Reader{File: []*zip.File{{FileHeader: zip.FileHeader{
		Name:               "sample-provider.dylib",
		UncompressedSize64: pluginLibraryMaxBytes + 1,
	}}}}
	_, _, errRead := readTargetLibrary(reader, "sample-provider", "darwin")
	if errRead == nil || !strings.Contains(errRead.Error(), "uncompressed size") {
		t.Fatalf("readTargetLibrary() error = %v, want uncompressed size error", errRead)
	}
}

func TestReadArchiveEntryStopsAtLimitPlusOne(t *testing.T) {
	t.Parallel()

	reader := &countingArchiveReader{Reader: strings.NewReader("123456")}
	_, errRead := readArchiveEntry(reader, 4)
	if errRead == nil || !strings.Contains(errRead.Error(), "maximum allowed size") {
		t.Fatalf("readArchiveEntry() error = %v, want size limit error", errRead)
	}
	if reader.read != 5 {
		t.Fatalf("bytes read = %d, want limit+1 (5)", reader.read)
	}
}

func TestInstallUsesLatestReleaseVersion(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	archiveData := makeZip(t, map[string]string{"sample-provider.dylib": "library-data"})
	archiveName := "sample-provider_0.2.0_darwin_arm64.zip"
	checksum := sha256.Sum256(archiveData)
	client := Client{HTTPClient: mapHTTPDoer{
		"https://api.github.com/repos/author-name/cliproxy-sample-provider-plugin/releases/latest": []byte(`{
			"tag_name": "v0.2.0",
			"assets": [
				{"name": "` + archiveName + `", "browser_download_url": "https://downloads.example/` + archiveName + `"},
				{"name": "checksums.txt", "browser_download_url": "https://downloads.example/checksums.txt"}
			]
		}`),
		"https://downloads.example/" + archiveName: archiveData,
		"https://downloads.example/checksums.txt":  []byte(hex.EncodeToString(checksum[:]) + "  " + archiveName + "\n"),
	}}

	result, errInstall := client.Install(context.Background(), testPlugin(), InstallOptions{
		PluginsDir: root,
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if errInstall != nil {
		t.Fatalf("Install() error = %v", errInstall)
	}
	if result.Version != "0.2.0" {
		t.Fatalf("Version = %q, want 0.2.0 from latest release tag", result.Version)
	}
	data, errRead := os.ReadFile(filepath.Join(root, "darwin", "arm64", "sample-provider.dylib"))
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "library-data" {
		t.Fatalf("installed data = %q", data)
	}
}

func TestInstallVersionUsesPinnedReleaseTag(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	archiveData := makeZip(t, map[string]string{"sample-provider.so": "library-data"})
	archiveName := "sample-provider_0.3.0_linux_amd64.zip"
	checksum := sha256.Sum256(archiveData)
	client := Client{HTTPClient: mapHTTPDoer{
		"https://api.github.com/repos/author-name/cliproxy-sample-provider-plugin/releases/tags/v0.3.0": []byte(`{
			"tag_name": "v0.3.0",
			"assets": [
				{"name": "` + archiveName + `", "browser_download_url": "https://downloads.example/` + archiveName + `"},
				{"name": "checksums.txt", "browser_download_url": "https://downloads.example/checksums.txt"}
			]
		}`),
		"https://downloads.example/" + archiveName: archiveData,
		"https://downloads.example/checksums.txt":  []byte(hex.EncodeToString(checksum[:]) + "  " + archiveName + "\n"),
	}}

	result, errInstall := client.InstallVersion(context.Background(), testPlugin(), "v0.3.0", "0.3.0", InstallOptions{
		PluginsDir: root,
		GOOS:       "linux",
		GOARCH:     "amd64",
	})
	if errInstall != nil {
		t.Fatalf("InstallVersion() error = %v", errInstall)
	}
	if result.Version != "0.3.0" {
		t.Fatalf("Version = %q, want 0.3.0", result.Version)
	}
	data, errRead := os.ReadFile(filepath.Join(root, "linux", "amd64", "sample-provider.so"))
	if errRead != nil {
		t.Fatalf("ReadFile() error = %v", errRead)
	}
	if string(data) != "library-data" {
		t.Fatalf("installed data = %q", data)
	}
}

func TestInstallRejectsInvalidLatestReleaseTag(t *testing.T) {
	t.Parallel()

	client := Client{HTTPClient: mapHTTPDoer{
		"https://api.github.com/repos/author-name/cliproxy-sample-provider-plugin/releases/latest": []byte(`{"tag_name": "latest", "assets": []}`),
	}}
	_, errInstall := client.Install(context.Background(), testPlugin(), InstallOptions{
		PluginsDir: t.TempDir(),
		GOOS:       "darwin",
		GOARCH:     "arm64",
	})
	if errInstall == nil {
		t.Fatal("Install() error = nil")
	}
	if !strings.Contains(errInstall.Error(), "invalid release tag") {
		t.Fatalf("Install() error = %v, want invalid release tag", errInstall)
	}
}

func makeZip(t *testing.T, files map[string]string) []byte {
	t.Helper()

	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		file, errCreate := writer.Create(name)
		if errCreate != nil {
			t.Fatalf("Create(%s) error = %v", name, errCreate)
		}
		if _, errWrite := file.Write([]byte(content)); errWrite != nil {
			t.Fatalf("Write(%s) error = %v", name, errWrite)
		}
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatalf("Close() error = %v", errClose)
	}
	return buffer.Bytes()
}

func makeZip64(t *testing.T, files map[string]string) []byte {
	t.Helper()

	archiveData := makeZip(t, files)
	directoryEnd := bytes.LastIndex(archiveData, []byte{'P', 'K', 0x05, 0x06})
	if directoryEnd < 0 {
		t.Fatal("ZIP directory end not found")
	}
	endRecord := append([]byte(nil), archiveData[directoryEnd:]...)
	directorySize := binary.LittleEndian.Uint32(endRecord[12:16])
	directoryOffset := binary.LittleEndian.Uint32(endRecord[16:20])
	entries := binary.LittleEndian.Uint16(endRecord[10:12])

	zip64Records := make([]byte, zipDirectory64EndSize+zipDirectory64LocatorSize)
	binary.LittleEndian.PutUint32(zip64Records[0:4], zipDirectory64EndSignature)
	binary.LittleEndian.PutUint64(zip64Records[4:12], zipDirectory64EndSize-12)
	binary.LittleEndian.PutUint16(zip64Records[12:14], 45)
	binary.LittleEndian.PutUint16(zip64Records[14:16], 45)
	binary.LittleEndian.PutUint64(zip64Records[24:32], uint64(entries))
	binary.LittleEndian.PutUint64(zip64Records[32:40], uint64(entries))
	binary.LittleEndian.PutUint64(zip64Records[40:48], uint64(directorySize))
	binary.LittleEndian.PutUint64(zip64Records[48:56], uint64(directoryOffset))
	locator := zip64Records[zipDirectory64EndSize:]
	binary.LittleEndian.PutUint32(locator[0:4], zipDirectory64LocatorSignature)
	binary.LittleEndian.PutUint64(locator[8:16], uint64(directoryEnd))
	binary.LittleEndian.PutUint32(locator[16:20], 1)
	binary.LittleEndian.PutUint16(endRecord[8:10], 1<<16-1)
	binary.LittleEndian.PutUint16(endRecord[10:12], 1<<16-1)
	binary.LittleEndian.PutUint32(endRecord[12:16], 1<<32-1)
	binary.LittleEndian.PutUint32(endRecord[16:20], 1<<32-1)

	result := make([]byte, 0, len(archiveData)+len(zip64Records))
	result = append(result, archiveData[:directoryEnd]...)
	result = append(result, zip64Records...)
	result = append(result, endRecord...)
	return result
}

type failingHTTPDoer struct{}

func (failingHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return nil, errors.New("network unavailable")
}

type mapHTTPDoer map[string][]byte

func (c mapHTTPDoer) Do(req *http.Request) (*http.Response, error) {
	body, ok := c[req.URL.String()]
	if !ok {
		return &http.Response{
			StatusCode: http.StatusNotFound,
			Body:       io.NopCloser(strings.NewReader("not found")),
			Header:     make(http.Header),
			Request:    req,
		}, nil
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(body)),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func testPlugin() Plugin {
	return Plugin{
		ID:          "sample-provider",
		Name:        "Sample Provider",
		Description: "Adds sample provider support.",
		Author:      "author-name",
		Version:     "0.1.0",
		Repository:  "https://github.com/author-name/cliproxy-sample-provider-plugin",
	}
}

type countingArchiveReader struct {
	io.Reader
	read int
}

func (r *countingArchiveReader) Read(buffer []byte) (int, error) {
	n, errRead := r.Reader.Read(buffer)
	r.read += n
	return n, errRead
}
