package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

const (
	contentSafetyLogDirEnv         = "CLIPROXY_CONTENT_SAFETY_LOG_DIR"
	contentSafetyLogSubdir         = "content-safety-451"
	contentSafetyLogMaxMetadataLen = 256
	contentSafetyLogMaxRecordBytes = 16 * 1024
)

type contentSafetyLogRecord struct {
	Timestamp              string `json:"timestamp"`
	RequestID              string `json:"request_id,omitempty"`
	Provider               string `json:"provider,omitempty"`
	AuthIndex              string `json:"auth_index,omitempty"`
	RouteModel             string `json:"route_model,omitempty"`
	RequestedModel         string `json:"requested_model,omitempty"`
	UpstreamModel          string `json:"upstream_model,omitempty"`
	RequestPath            string `json:"request_path,omitempty"`
	StatusCode             int    `json:"status_code,omitempty"`
	SafetyCode             string `json:"safety_code,omitempty"`
	SafetyDirection        string `json:"safety_direction,omitempty"`
	PayloadBytes           int    `json:"payload_bytes,omitempty"`
	PayloadSHA256          string `json:"payload_sha256,omitempty"`
	OriginalRequestPresent bool   `json:"original_request_present,omitempty"`
	OriginalRequestBytes   int    `json:"original_request_bytes,omitempty"`
	OriginalRequestSHA256  string `json:"original_request_sha256,omitempty"`
}

type contentSafetyPayloadSummary struct {
	Bytes  int
	SHA256 string
}

func (m *Manager) recordContentSafetyRequest(ctx context.Context, auth *Auth, provider, routeModel, upstreamModel string, opts cliproxyexecutor.Options, payload []byte, err error) {
	if m == nil || !isRequestScopedContentSafetyError(err) {
		return
	}
	path := m.contentSafetyLogFilePath(time.Now())
	if path == "" {
		return
	}
	safetyCode, safetyDirection := contentSafetyErrorDetails(err)

	record := contentSafetyLogRecord{
		Timestamp:       time.Now().UTC().Format(time.RFC3339Nano),
		RequestID:       truncateContentSafetyString(logging.GetRequestID(ctx), contentSafetyLogMaxMetadataLen),
		Provider:        truncateContentSafetyString(strings.TrimSpace(provider), contentSafetyLogMaxMetadataLen),
		RouteModel:      truncateContentSafetyString(strings.TrimSpace(routeModel), contentSafetyLogMaxMetadataLen),
		RequestedModel:  truncateContentSafetyString(requestedModelAliasFromOptions(opts, routeModel), contentSafetyLogMaxMetadataLen),
		UpstreamModel:   truncateContentSafetyString(strings.TrimSpace(upstreamModel), contentSafetyLogMaxMetadataLen),
		RequestPath:     truncateContentSafetyString(contentSafetyMetadataString(opts.Metadata, cliproxyexecutor.RequestPathMetadataKey), contentSafetyLogMaxMetadataLen),
		StatusCode:      statusCodeFromError(err),
		SafetyCode:      truncateContentSafetyString(safetyCode, contentSafetyLogMaxMetadataLen),
		SafetyDirection: truncateContentSafetyString(safetyDirection, contentSafetyLogMaxMetadataLen),
	}
	if record.StatusCode == 0 {
		record.StatusCode = http.StatusUnavailableForLegalReasons
	}
	if auth != nil {
		record.AuthIndex = authMetricIndex(auth)
	}

	payloadSummary := summarizeContentSafetyPayload(payload)
	record.PayloadBytes = payloadSummary.Bytes
	record.PayloadSHA256 = payloadSummary.SHA256

	if len(opts.OriginalRequest) > 0 && !bytes.Equal(opts.OriginalRequest, payload) {
		originalSummary := summarizeContentSafetyPayload(opts.OriginalRequest)
		record.OriginalRequestPresent = true
		record.OriginalRequestBytes = originalSummary.Bytes
		record.OriginalRequestSHA256 = originalSummary.SHA256
	}

	line, errMarshal := marshalContentSafetyLogRecord(record)
	if errMarshal != nil {
		logEntryWithRequestID(ctx).WithError(errMarshal).Warn("marshal content safety log failed")
		return
	}

	if errWrite := appendContentSafetyLogLine(path, line); errWrite != nil {
		logEntryWithRequestID(ctx).WithError(errWrite).Warn("write content safety log failed")
	}
}

func (m *Manager) contentSafetyLogFilePath(now time.Time) string {
	dir := strings.TrimSpace(os.Getenv(contentSafetyLogDirEnv))
	if dir == "" {
		cfg := m.configSnapshot()
		dir = filepath.Join(logging.ResolveLogDirectory(cfg), contentSafetyLogSubdir)
	}
	if dir == "" {
		return ""
	}
	return filepath.Join(filepath.Clean(dir), now.Format("2006-01-02")+".jsonl")
}

func (m *Manager) configSnapshot() *internalconfig.Config {
	if m == nil {
		return nil
	}
	cfg, _ := m.runtimeConfig.Load().(*internalconfig.Config)
	return cfg
}

func appendContentSafetyLogLine(path string, line []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create content safety log directory: %w", err)
	}
	f, errOpen := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if errOpen != nil {
		return fmt.Errorf("open content safety log file: %w", errOpen)
	}
	defer func() {
		if errClose := f.Close(); errClose != nil {
			log.WithError(errClose).Warn("close content safety log file failed")
		}
	}()
	if _, errWrite := f.Write(line); errWrite != nil {
		return fmt.Errorf("write content safety log file: %w", errWrite)
	}
	return nil
}

func contentSafetyMetadataString(metadata map[string]any, key string) string {
	if len(metadata) == 0 {
		return ""
	}
	raw, ok := metadata[key]
	if !ok || raw == nil {
		return ""
	}
	switch typed := raw.(type) {
	case string:
		return strings.TrimSpace(typed)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func contentSafetyErrorDetails(err error) (string, string) {
	code := normalizeContentSafetyCode(errorCodeFromError(err))
	message := errorString(err)
	switch {
	case isMiniMaxInputNewSensitiveSignal(code, message):
		return fallbackContentSafetyCode(code, "1026"), "input"
	case isMiniMaxOutputNewSensitiveSignal(code, message):
		return fallbackContentSafetyCode(code, "1027"), "output"
	case isContentSafety1301Signal(code, message):
		return fallbackContentSafetyCode(code, "1301"), "input"
	case isGenericContentSafetySignal(code, message):
		return code, "input"
	default:
		return code, ""
	}
}

func normalizeContentSafetyCode(code string) string {
	return strings.Trim(strings.ToLower(strings.TrimSpace(code)), `"'(),:;[]{}<>`)
}

func fallbackContentSafetyCode(code string, fallback string) string {
	if strings.TrimSpace(code) != "" {
		return code
	}
	return fallback
}

func summarizeContentSafetyPayload(payload []byte) contentSafetyPayloadSummary {
	if len(payload) == 0 {
		return contentSafetyPayloadSummary{}
	}
	return contentSafetyPayloadSummary{
		Bytes:  len(payload),
		SHA256: contentSafetySHA256Hex(payload),
	}
}

func contentSafetySHA256Hex(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func marshalContentSafetyLogRecord(record contentSafetyLogRecord) ([]byte, error) {
	line, errMarshal := json.Marshal(record)
	if errMarshal != nil {
		return nil, errMarshal
	}
	if len(line) > contentSafetyLogMaxRecordBytes {
		return nil, fmt.Errorf("content safety log line exceeds %d bytes", contentSafetyLogMaxRecordBytes)
	}
	return append(line, '\n'), nil
}

func truncateContentSafetyString(value string, maxLen int) string {
	if maxLen <= 0 || len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + fmt.Sprintf("...[truncated %d bytes]", len(value)-maxLen)
}
