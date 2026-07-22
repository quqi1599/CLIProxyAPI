package pluginstore

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/cpu"
)

const (
	pluginArchiveMaxEntries = 128
	pluginLibraryMaxBytes   = 256 << 20

	zipDirectoryHeaderSignature    = 0x02014b50
	zipDirectoryEndSignature       = 0x06054b50
	zipDirectory64EndSignature     = 0x06064b50
	zipDirectory64LocatorSignature = 0x07064b50
	zipDirectorySignatureSignature = 0x05054b50
	zipDirectoryHeaderSize         = 46
	zipDirectoryEndSize            = 22
	zipDirectory64EndSize          = 56
	zipDirectory64LocatorSize      = 20
	zipMaxCommentSize              = 1<<16 - 1
)

type InstallOptions struct {
	PluginsDir string
	GOOS       string
	GOARCH     string
	// PluginLoaded reports whether the plugin's dynamic library is currently
	// loaded by the running host. Windows installs are rejected while it returns
	// true unless BeforeWrite can unload the plugin before replacement.
	PluginLoaded func() bool
	// BeforeWrite runs after the archive has been downloaded and verified, but
	// before the target plugin file is replaced.
	BeforeWrite func() error
}

var (
	// ErrLoadedPluginLocked is returned when an install would overwrite a plugin
	// library that is loaded by the running process on Windows.
	ErrLoadedPluginLocked = errors.New("loaded plugin library cannot be overwritten while the server is running")
	// ErrPluginArchiveTooManyEntries is returned before archive/zip allocates
	// file records for an archive that exceeds the plugin entry limit.
	ErrPluginArchiveTooManyEntries = errors.New("plugin archive contains too many entries")
	// ErrPluginArchiveInvalidDirectory is returned when the ZIP central
	// directory is structurally invalid.
	ErrPluginArchiveInvalidDirectory = errors.New("plugin archive has invalid central directory")
)

type InstallResult struct {
	ID          string `json:"id"`
	Version     string `json:"version"`
	Path        string `json:"path"`
	Overwritten bool   `json:"overwritten"`
	Skipped     bool   `json:"skipped"`
}

func (c Client) Install(ctx context.Context, plugin Plugin, options InstallOptions) (InstallResult, error) {
	if errValidate := ValidatePlugin(plugin); errValidate != nil {
		return InstallResult{}, errValidate
	}
	options = normalizeInstallOptions(options)
	if loadedPluginInstallBlocked(options) && options.BeforeWrite == nil {
		return InstallResult{}, ErrLoadedPluginLocked
	}
	release, errRelease := c.FetchLatestRelease(ctx, plugin)
	if errRelease != nil {
		return InstallResult{}, errRelease
	}
	latestVersion, errVersion := ReleaseVersion(release)
	if errVersion != nil {
		return InstallResult{}, errVersion
	}
	plugin.Version = latestVersion
	return c.installRelease(ctx, plugin, release, latestVersion, options)
}

// InstallVersion installs a plugin artifact from a fixed release tag/version.
func (c Client) InstallVersion(ctx context.Context, plugin Plugin, releaseTag string, version string, options InstallOptions) (InstallResult, error) {
	if errValidate := ValidatePlugin(plugin); errValidate != nil {
		return InstallResult{}, errValidate
	}
	options = normalizeInstallOptions(options)
	if loadedPluginInstallBlocked(options) && options.BeforeWrite == nil {
		return InstallResult{}, ErrLoadedPluginLocked
	}
	version = normalizeVersion(version)
	if !validPluginVersion(version) {
		return InstallResult{}, fmt.Errorf("invalid plugin version %q", version)
	}
	releaseTag = strings.TrimSpace(releaseTag)
	if releaseTag == "" {
		releaseTag = version
	}
	release, errRelease := c.FetchReleaseByTag(ctx, plugin, releaseTag)
	if errRelease != nil {
		return InstallResult{}, errRelease
	}
	releaseVersion, errVersion := ReleaseVersion(release)
	if errVersion != nil {
		return InstallResult{}, errVersion
	}
	if releaseVersion != version {
		return InstallResult{}, fmt.Errorf("release tag %q resolved version %q, want %q", releaseTag, releaseVersion, version)
	}
	plugin.Version = version
	return c.installRelease(ctx, plugin, release, version, options)
}

func (c Client) installRelease(ctx context.Context, plugin Plugin, release Release, version string, options InstallOptions) (InstallResult, error) {
	archiveAsset, checksumAsset, errAssets := SelectReleaseAssets(release, plugin.ID, plugin.Version, options.GOOS, options.GOARCH)
	if errAssets != nil {
		return InstallResult{}, errAssets
	}
	archiveData, errArchive := c.DownloadAsset(ctx, archiveAsset)
	if errArchive != nil {
		return InstallResult{}, fmt.Errorf("download %s: %w", archiveAsset.Name, errArchive)
	}
	checksumData, errChecksum := c.DownloadAsset(ctx, checksumAsset)
	if errChecksum != nil {
		return InstallResult{}, fmt.Errorf("download checksums.txt: %w", errChecksum)
	}
	checksums, errParse := ParseChecksums(checksumData)
	if errParse != nil {
		return InstallResult{}, errParse
	}
	if errVerify := VerifyChecksum(archiveAsset.Name, archiveData, checksums); errVerify != nil {
		return InstallResult{}, errVerify
	}
	plugin.Version = version
	return InstallArchive(archiveData, plugin, options)
}

func InstallArchive(archiveData []byte, plugin Plugin, options InstallOptions) (InstallResult, error) {
	options = normalizeInstallOptions(options)
	id := strings.TrimSpace(plugin.ID)
	if !validPluginID(id) {
		return InstallResult{}, fmt.Errorf("invalid plugin id %q", plugin.ID)
	}
	if int64(len(archiveData)) > pluginArchiveMaxBytes {
		return InstallResult{}, fmt.Errorf("plugin archive size %d exceeds maximum allowed size of %d bytes", len(archiveData), pluginArchiveMaxBytes)
	}
	if errPreflight := preflightZipDirectory(archiveData); errPreflight != nil {
		return InstallResult{}, errPreflight
	}
	reader, errZip := zip.NewReader(bytes.NewReader(archiveData), int64(len(archiveData)))
	if errZip != nil {
		return InstallResult{}, fmt.Errorf("open zip: %w", errZip)
	}

	libraryData, mode, errLibrary := readTargetLibrary(reader, id, options.GOOS)
	if errLibrary != nil {
		return InstallResult{}, errLibrary
	}

	targetPath, errTarget := installTargetPath(options, id)
	if errTarget != nil {
		return InstallResult{}, errTarget
	}
	overwritten := false
	if _, errStat := os.Stat(targetPath); errStat == nil {
		overwritten = true
	} else if !errors.Is(errStat, os.ErrNotExist) {
		return InstallResult{}, fmt.Errorf("stat target plugin: %w", errStat)
	}
	if overwritten {
		existingData, errReadExisting := os.ReadFile(targetPath)
		if errReadExisting != nil {
			return InstallResult{}, fmt.Errorf("read target plugin: %w", errReadExisting)
		}
		if bytes.Equal(existingData, libraryData) {
			return InstallResult{
				ID:          id,
				Version:     strings.TrimSpace(plugin.Version),
				Path:        targetPath,
				Overwritten: true,
				Skipped:     true,
			}, nil
		}
	}
	// Re-check immediately before writing: the plugin may have been loaded
	// while the archive was being downloaded and verified.
	if options.BeforeWrite != nil {
		if errBeforeWrite := options.BeforeWrite(); errBeforeWrite != nil {
			return InstallResult{}, fmt.Errorf("prepare plugin write: %w", errBeforeWrite)
		}
	}
	if loadedPluginInstallBlocked(options) {
		return InstallResult{}, ErrLoadedPluginLocked
	}
	if errWrite := writeFileAtomic(targetPath, libraryData, mode); errWrite != nil {
		return InstallResult{}, errWrite
	}
	return InstallResult{
		ID:          id,
		Version:     strings.TrimSpace(plugin.Version),
		Path:        targetPath,
		Overwritten: overwritten,
	}, nil
}

type zipDirectoryEnd struct {
	offset          int
	directoryOffset uint64
	directorySize   uint64
	entries         uint64
}

func preflightZipDirectory(archiveData []byte) error {
	end, errEnd := readZipDirectoryEnd(archiveData)
	if errEnd != nil {
		return errEnd
	}
	if end.entries > pluginArchiveMaxEntries {
		return tooManyPluginArchiveEntries(end.entries)
	}
	if end.directorySize > uint64(end.offset) {
		return invalidPluginArchiveDirectory("directory size exceeds archive bounds")
	}

	start := end.offset - int(end.directorySize)
	if end.directoryOffset > uint64(start) {
		return invalidPluginArchiveDirectory("directory offset exceeds directory start")
	}
	// Match archive/zip's compatibility fallback for archives whose recorded
	// offset already includes a prepended executable or other prefix.
	if end.directoryOffset < uint64(start) && validZipDirectoryHeaderAt(archiveData, int(end.directoryOffset), end.offset) {
		start = int(end.directoryOffset)
	}

	entries, errScan := scanZipDirectory(archiveData, start, end.offset)
	if errScan != nil {
		return errScan
	}
	if entries != end.entries {
		return invalidPluginArchiveDirectory(fmt.Sprintf("entry count is %d, declared %d", entries, end.entries))
	}
	return nil
}

func readZipDirectoryEnd(archiveData []byte) (zipDirectoryEnd, error) {
	offset := findZipDirectoryEnd(archiveData)
	if offset < 0 {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("end record not found")
	}
	endRecord := archiveData[offset : offset+zipDirectoryEndSize]
	diskNumber := binary.LittleEndian.Uint16(endRecord[4:6])
	directoryDisk := binary.LittleEndian.Uint16(endRecord[6:8])
	entriesOnDisk := uint64(binary.LittleEndian.Uint16(endRecord[8:10]))
	entries := uint64(binary.LittleEndian.Uint16(endRecord[10:12]))
	directorySize := uint64(binary.LittleEndian.Uint32(endRecord[12:16]))
	directoryOffset := uint64(binary.LittleEndian.Uint32(endRecord[16:20]))

	zip64 := entriesOnDisk == 1<<16-1 || entries == 1<<16-1 || directorySize == 1<<32-1 || directoryOffset == 1<<32-1
	if zip64 {
		zip64End, errZip64 := readZip64DirectoryEnd(archiveData, offset)
		if errZip64 != nil {
			return zipDirectoryEnd{}, errZip64
		}
		return zip64End, nil
	}
	if diskNumber != 0 || directoryDisk != 0 || entriesOnDisk != entries {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("multi-disk archives are not supported")
	}
	return zipDirectoryEnd{
		offset:          offset,
		directoryOffset: directoryOffset,
		directorySize:   directorySize,
		entries:         entries,
	}, nil
}

func findZipDirectoryEnd(archiveData []byte) int {
	start := len(archiveData) - (zipDirectoryEndSize + zipMaxCommentSize)
	if start < 0 {
		start = 0
	}
	for offset := len(archiveData) - zipDirectoryEndSize; offset >= start; offset-- {
		if binary.LittleEndian.Uint32(archiveData[offset:]) != zipDirectoryEndSignature {
			continue
		}
		commentSize := int(binary.LittleEndian.Uint16(archiveData[offset+20:]))
		if offset+zipDirectoryEndSize+commentSize <= len(archiveData) {
			return offset
		}
	}
	return -1
}

func readZip64DirectoryEnd(archiveData []byte, directoryEndOffset int) (zipDirectoryEnd, error) {
	locatorOffset := directoryEndOffset - zipDirectory64LocatorSize
	if locatorOffset < 0 || binary.LittleEndian.Uint32(archiveData[locatorOffset:]) != zipDirectory64LocatorSignature {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("ZIP64 locator not found")
	}
	locator := archiveData[locatorOffset:directoryEndOffset]
	if binary.LittleEndian.Uint32(locator[4:8]) != 0 || binary.LittleEndian.Uint32(locator[16:20]) != 1 {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("multi-disk ZIP64 archives are not supported")
	}
	recordOffset64 := binary.LittleEndian.Uint64(locator[8:16])
	if recordOffset64 > uint64(locatorOffset) {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("ZIP64 end record offset exceeds archive bounds")
	}
	recordOffset := int(recordOffset64)
	if locatorOffset-recordOffset < zipDirectory64EndSize {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("truncated ZIP64 end record")
	}
	record := archiveData[recordOffset:locatorOffset]
	if binary.LittleEndian.Uint32(record) != zipDirectory64EndSignature {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("ZIP64 end record not found")
	}
	recordSize := binary.LittleEndian.Uint64(record[4:12])
	wantRecordSize := locatorOffset - recordOffset - 12
	if recordSize < zipDirectory64EndSize-12 || recordSize != uint64(wantRecordSize) {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("invalid ZIP64 end record size")
	}
	if binary.LittleEndian.Uint32(record[16:20]) != 0 || binary.LittleEndian.Uint32(record[20:24]) != 0 {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("multi-disk ZIP64 archives are not supported")
	}
	entriesOnDisk := binary.LittleEndian.Uint64(record[24:32])
	entries := binary.LittleEndian.Uint64(record[32:40])
	if entriesOnDisk != entries {
		return zipDirectoryEnd{}, invalidPluginArchiveDirectory("ZIP64 entry counts do not match")
	}
	return zipDirectoryEnd{
		offset:          recordOffset,
		directoryOffset: binary.LittleEndian.Uint64(record[48:56]),
		directorySize:   binary.LittleEndian.Uint64(record[40:48]),
		entries:         entries,
	}, nil
}

func scanZipDirectory(archiveData []byte, start int, end int) (uint64, error) {
	var entries uint64
	for offset := start; offset < end; {
		if end-offset < 4 {
			return 0, invalidPluginArchiveDirectory("truncated directory record")
		}
		switch binary.LittleEndian.Uint32(archiveData[offset:]) {
		case zipDirectoryHeaderSignature:
			recordSize, ok := zipDirectoryHeaderSizeAt(archiveData, offset, end)
			if !ok {
				return 0, invalidPluginArchiveDirectory("truncated directory entry")
			}
			entries++
			if entries > pluginArchiveMaxEntries {
				return 0, tooManyPluginArchiveEntries(entries)
			}
			offset += recordSize
		case zipDirectorySignatureSignature:
			if end-offset < 6 {
				return 0, invalidPluginArchiveDirectory("truncated directory signature")
			}
			recordSize := 6 + int(binary.LittleEndian.Uint16(archiveData[offset+4:]))
			if offset+recordSize != end {
				return 0, invalidPluginArchiveDirectory("invalid directory signature size")
			}
			offset = end
		default:
			return 0, invalidPluginArchiveDirectory("unexpected record in central directory")
		}
	}
	return entries, nil
}

func validZipDirectoryHeaderAt(archiveData []byte, offset int, end int) bool {
	if offset < 0 || offset >= end || end > len(archiveData) {
		return false
	}
	_, ok := zipDirectoryHeaderSizeAt(archiveData, offset, end)
	return ok
}

func zipDirectoryHeaderSizeAt(archiveData []byte, offset int, end int) (int, bool) {
	if end-offset < zipDirectoryHeaderSize || binary.LittleEndian.Uint32(archiveData[offset:]) != zipDirectoryHeaderSignature {
		return 0, false
	}
	nameSize := int(binary.LittleEndian.Uint16(archiveData[offset+28:]))
	extraSize := int(binary.LittleEndian.Uint16(archiveData[offset+30:]))
	commentSize := int(binary.LittleEndian.Uint16(archiveData[offset+32:]))
	recordSize := zipDirectoryHeaderSize + nameSize + extraSize + commentSize
	if recordSize > end-offset {
		return 0, false
	}
	extraStart := offset + zipDirectoryHeaderSize + nameSize
	if !validZip64DirectoryExtra(archiveData[offset:offset+zipDirectoryHeaderSize], archiveData[extraStart:extraStart+extraSize]) {
		return 0, false
	}
	return recordSize, true
}

func validZip64DirectoryExtra(header []byte, extra []byte) bool {
	needUncompressedSize := binary.LittleEndian.Uint32(header[24:28]) == 1<<32-1
	needCompressedSize := binary.LittleEndian.Uint32(header[20:24]) == 1<<32-1
	needHeaderOffset := binary.LittleEndian.Uint32(header[42:46]) == 1<<32-1
	for len(extra) >= 4 {
		fieldTag := binary.LittleEndian.Uint16(extra[0:2])
		fieldSize := int(binary.LittleEndian.Uint16(extra[2:4]))
		extra = extra[4:]
		if fieldSize > len(extra) {
			break
		}
		field := extra[:fieldSize]
		extra = extra[fieldSize:]
		if fieldTag != 0x0001 {
			continue
		}
		for _, needed := range []bool{needUncompressedSize, needCompressedSize, needHeaderOffset} {
			if !needed {
				continue
			}
			if len(field) < 8 {
				return false
			}
			field = field[8:]
		}
		return true
	}
	return !needCompressedSize && !needHeaderOffset
}

func tooManyPluginArchiveEntries(entries uint64) error {
	return fmt.Errorf("%w: zip contains %d entries, maximum allowed is %d", ErrPluginArchiveTooManyEntries, entries, pluginArchiveMaxEntries)
}

func invalidPluginArchiveDirectory(reason string) error {
	return fmt.Errorf("%w: %s", ErrPluginArchiveInvalidDirectory, reason)
}

func installTargetPath(options InstallOptions, id string) (string, error) {
	defaultPath := filepath.Join(options.PluginsDir, options.GOOS, options.GOARCH, id+pluginExtension(options.GOOS))
	if options.GOOS != runtime.GOOS || options.GOARCH != runtime.GOARCH {
		return defaultPath, nil
	}
	files, errDiscover := discoverCurrentPluginFiles(options.PluginsDir)
	if errDiscover != nil {
		return "", fmt.Errorf("discover current plugin files: %w", errDiscover)
	}
	for _, file := range files {
		if file.ID == id && strings.TrimSpace(file.Path) != "" {
			return file.Path, nil
		}
	}
	return defaultPath, nil
}

func readTargetLibrary(reader *zip.Reader, id string, goos string) ([]byte, os.FileMode, error) {
	targetName := strings.TrimSpace(id) + pluginExtension(goos)
	var target *zip.File
	for _, file := range reader.File {
		cleanedName, errClean := cleanZipName(file.Name)
		if errClean != nil {
			return nil, 0, errClean
		}
		if file.FileInfo().IsDir() {
			continue
		}
		if !regularZipFile(file) {
			return nil, 0, fmt.Errorf("zip entry %s is not a regular file", file.Name)
		}
		if !hasDynamicLibraryExtension(cleanedName) {
			continue
		}
		if cleanedName != targetName {
			if path.Base(cleanedName) == targetName {
				return nil, 0, fmt.Errorf("target dynamic library must be at zip root")
			}
			return nil, 0, fmt.Errorf("dynamic library filename must be %s", targetName)
		}
		if target != nil {
			return nil, 0, fmt.Errorf("zip contains multiple target dynamic libraries")
		}
		target = file
	}
	if target == nil {
		return nil, 0, fmt.Errorf("zip does not contain %s", targetName)
	}
	if target.UncompressedSize64 > pluginLibraryMaxBytes {
		return nil, 0, fmt.Errorf("%s uncompressed size %d exceeds maximum allowed size of %d bytes", targetName, target.UncompressedSize64, pluginLibraryMaxBytes)
	}

	handle, errOpen := target.Open()
	if errOpen != nil {
		return nil, 0, fmt.Errorf("open %s: %w", targetName, errOpen)
	}
	defer func() {
		if errClose := handle.Close(); errClose != nil {
			log.WithError(errClose).Debug("failed to close plugin archive entry")
		}
	}()
	data, errRead := readArchiveEntry(handle, pluginLibraryMaxBytes)
	if errRead != nil {
		return nil, 0, fmt.Errorf("read %s: %w", targetName, errRead)
	}
	mode := target.FileInfo().Mode().Perm()
	if mode == 0 {
		mode = 0o755
	}
	return data, mode, nil
}

func readArchiveEntry(reader io.Reader, maxBytes int64) ([]byte, error) {
	data, errRead := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if errRead != nil {
		return nil, errRead
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("entry exceeds maximum allowed size of %d bytes", maxBytes)
	}
	return data, nil
}

func cleanZipName(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("zip entry has empty name")
	}
	if strings.Contains(name, `\`) {
		return "", fmt.Errorf("zip entry %s uses backslash path separators", name)
	}
	if path.IsAbs(name) {
		return "", fmt.Errorf("zip entry %s is absolute", name)
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("zip entry %s escapes archive root", name)
	}
	return cleaned, nil
}

func regularZipFile(file *zip.File) bool {
	mode := file.FileInfo().Mode()
	return mode.IsRegular() || mode.Type() == 0
}

func hasDynamicLibraryExtension(name string) bool {
	lowerName := strings.ToLower(name)
	return strings.HasSuffix(lowerName, ".dylib") || strings.HasSuffix(lowerName, ".so") || strings.HasSuffix(lowerName, ".dll")
}

type pluginFileInfo struct {
	ID   string
	Path string
}

func discoverCurrentPluginFiles(root string) ([]pluginFileInfo, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		root = "plugins"
	}
	candidates := pluginCandidateDirs(root, runtime.GOOS, runtime.GOARCH, cpuVariant())
	extension := pluginExtension(runtime.GOOS)
	selected := make([]pluginFileInfo, 0)
	seen := make(map[string]struct{})
	for _, dir := range candidates {
		entries, errReadDir := os.ReadDir(dir)
		if errReadDir != nil {
			if os.IsNotExist(errReadDir) {
				continue
			}
			return nil, errReadDir
		}
		files := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry == nil || !entry.Type().IsRegular() {
				continue
			}
			if strings.HasSuffix(strings.ToLower(entry.Name()), extension) {
				files = append(files, filepath.Join(dir, entry.Name()))
			}
		}
		sort.Strings(files)
		for _, path := range files {
			id := pluginIDFromPath(path)
			if !validPluginID(id) {
				continue
			}
			if _, exists := seen[id]; exists {
				continue
			}
			seen[id] = struct{}{}
			selected = append(selected, pluginFileInfo{ID: id, Path: path})
		}
	}
	return selected, nil
}

func pluginCandidateDirs(root string, goos string, goarch string, variant string) []string {
	dirs := make([]string, 0, 3)
	if variant != "" {
		dirs = append(dirs, filepath.Join(root, goos, goarch+"-"+variant))
	}
	dirs = append(dirs, filepath.Join(root, goos, goarch))
	dirs = append(dirs, root)
	return dirs
}

func pluginIDFromPath(path string) string {
	base := filepath.Base(path)
	lowerBase := strings.ToLower(base)
	for _, extension := range []string{".so", ".dylib", ".dll"} {
		if strings.HasSuffix(lowerBase, extension) {
			return base[:len(base)-len(extension)]
		}
	}
	return base
}

func pluginExtension(goos string) string {
	switch strings.ToLower(strings.TrimSpace(goos)) {
	case "darwin", "mac", "macos", "osx":
		return ".dylib"
	case "windows":
		return ".dll"
	default:
		return ".so"
	}
}

func cpuVariant() string {
	if runtime.GOARCH != "amd64" {
		return ""
	}
	if cpu.X86.HasAVX512F && cpu.X86.HasAVX512BW && cpu.X86.HasAVX512CD && cpu.X86.HasAVX512DQ && cpu.X86.HasAVX512VL {
		return "v4"
	}
	if cpu.X86.HasAVX && cpu.X86.HasAVX2 && cpu.X86.HasBMI1 && cpu.X86.HasBMI2 && cpu.X86.HasFMA {
		return "v3"
	}
	if cpu.X86.HasSSE3 && cpu.X86.HasSSSE3 && cpu.X86.HasSSE41 && cpu.X86.HasSSE42 && cpu.X86.HasPOPCNT {
		return "v2"
	}
	return "v1"
}

func writeFileAtomic(targetPath string, data []byte, mode os.FileMode) error {
	targetDir := filepath.Dir(targetPath)
	if errMkdir := os.MkdirAll(targetDir, 0o755); errMkdir != nil {
		return fmt.Errorf("create plugin directory: %w", errMkdir)
	}

	temp, errTemp := os.CreateTemp(targetDir, "."+filepath.Base(targetPath)+".tmp-*")
	if errTemp != nil {
		return fmt.Errorf("create temp plugin file: %w", errTemp)
	}
	tempPath := temp.Name()
	removeTemp := true
	closed := false
	defer func() {
		if !closed {
			if errClose := temp.Close(); errClose != nil {
				log.WithError(errClose).Debug("failed to close temp plugin file")
			}
		}
		if removeTemp {
			if errRemove := os.Remove(tempPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				log.WithError(errRemove).Debug("failed to remove temp plugin file")
			}
		}
	}()

	if errChmod := temp.Chmod(mode); errChmod != nil {
		return fmt.Errorf("chmod temp plugin file: %w", errChmod)
	}
	if _, errWrite := temp.Write(data); errWrite != nil {
		return fmt.Errorf("write temp plugin file: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		return fmt.Errorf("sync temp plugin file: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temp plugin file: %w", errClose)
	}
	closed = true
	if errRename := os.Rename(tempPath, targetPath); errRename != nil {
		if runtime.GOOS == "windows" {
			if errRemove := os.Remove(targetPath); errRemove != nil && !errors.Is(errRemove, os.ErrNotExist) {
				return fmt.Errorf("remove old plugin file: %w", errRemove)
			}
			if errRenameRetry := os.Rename(tempPath, targetPath); errRenameRetry == nil {
				removeTemp = false
				return nil
			} else {
				return fmt.Errorf("install plugin file: %w", errRenameRetry)
			}
		}
		return fmt.Errorf("install plugin file: %w", errRename)
	}
	removeTemp = false
	return nil
}

func loadedPluginInstallBlocked(options InstallOptions) bool {
	return options.PluginLoaded != nil && strings.EqualFold(options.GOOS, "windows") && options.PluginLoaded()
}

func normalizeInstallOptions(options InstallOptions) InstallOptions {
	options.PluginsDir = strings.TrimSpace(options.PluginsDir)
	if options.PluginsDir == "" {
		options.PluginsDir = "plugins"
	}
	options.GOOS = strings.TrimSpace(options.GOOS)
	if options.GOOS == "" {
		options.GOOS = runtime.GOOS
	}
	options.GOARCH = strings.TrimSpace(options.GOARCH)
	if options.GOARCH == "" {
		options.GOARCH = runtime.GOARCH
	}
	return options
}
