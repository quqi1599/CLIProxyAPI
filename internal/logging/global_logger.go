package logging

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	log "github.com/sirupsen/logrus"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	setupOnce      sync.Once
	writerMu       sync.Mutex
	logWriter      *lumberjack.Logger
	ginInfoWriter  *io.PipeWriter
	ginErrorWriter *io.PipeWriter
)

// LogFormatter defines a custom log format for logrus.
// This formatter adds timestamp, level, request ID, and source location to each log entry.
// Format: [2025-12-23 20:14:04] [debug] [manager.go:524] | a1b2c3d4 | Use API key sk-9...0RHO for model gpt-5.2
type LogFormatter struct{}

// logFieldOrder defines the display order for common log fields.
var logFieldOrder = []string{
	"event", "client_request_id", "provider", "model", "requested_model", "upstream_model",
	"auth_index", "routing_strategy", "routing_scope", "routing_group", "prefix", "base_url", "token_hash",
	"selection_reason", "candidate_count", "candidate_ready_count", "candidate_health_downweighted",
	"candidate_skipped_disabled", "candidate_skipped_cooldown", "candidate_skipped_breaker", "candidate_skipped_unavailable",
	"status", "status_code", "success", "error_code", "retryable", "retry_after_ms", "reset_ms", "providers",
	"normalized_status", "outer_status", "failure_kind", "failure_scope", "scope", "semantic_type", "semantic_code",
	"stream_phase", "output_committed",
	"mode", "budget", "level", "original_mode", "original_value", "min", "max", "clamped_to", "version", "error",
	"request_path", "attempt_no", "round_no", "retry_reason", "tool_type", "tool_source", "policy", "reason",
	"candidate_route_count", "eligible_route_count", "blocked_route_count", "breaker_open_count",
	"blocked_reasons", "breaker_statuses", "breaker_reasons", "earliest_recovery_ms",
	"ordinary_candidate_route_count", "ordinary_eligible_route_count", "ordinary_blocked_route_count", "ordinary_breaker_open_count",
	"ordinary_blocked_reasons", "ordinary_breaker_statuses", "ordinary_breaker_reasons", "ordinary_earliest_recovery_ms",
	"compaction_candidate_route_count", "compaction_eligible_route_count", "compaction_blocked_route_count", "compaction_breaker_open_count",
	"compaction_blocked_reasons", "compaction_breaker_statuses", "compaction_breaker_reasons", "compaction_earliest_recovery_ms",
	"compaction_intent", "compaction_compatibility_group", "failed_attempt", "consecutive_failures", "open_until",
	"cause_error_code", "cause_reset_ms",
	"retry_pressure_state", "retry_pressure_reason", "degraded_route_count",
	"retry_permit_limit", "retry_in_flight", "retry_waiters", "retry_permit_wait_ms",
	"retry_permit_acquired", "retry_permit_rejected",
	"outcome", "eligible", "delay_ms", "enforced_timeout_ms", "policy_state", "decision_source", "decision_reason",
	"previous_state", "window_seconds", "eligible_first_attempts",
	"deliverable_within_25", "deliverable_within_30", "deliverable_within_40", "deliverable_within_50",
	"first_event_success_rate_25", "first_event_success_rate_30", "first_event_success_rate_40", "first_event_success_rate_50",
	"failure_rate", "hard_failure_rate", "timeout_count", "upstream_5xx_count", "network_failure_count", "used_global_fallback",
	"max_channels", "max_rounds", "wait_budget_ms",
	"executor", "channel", "compat_name", "compat_kind", "compat_kind_source", "compat_mapping", "upstream_request_id",
	"payload_fields", "message_roles", "message_role_sequence", "message_content_kinds", "content_part_types",
	"input_item_types", "tool_definition_count", "tool_call_count", "tool_choice_type", "thinking_type",
	"response_format_type", "parallel_tool_calls", "assistant_tool_call_messages", "tool_result_messages",
	"reasoning_messages", "max_content_parts",
	"repairs", "merged_tool_result_messages", "deduped_tool_results",
	"reordered_tool_results", "removed_tool_uses", "removed_tool_results", "repair_type", "repairs_count",
	"removed_fields", "modified_fields", "added_fields",
	"payload_bytes_before", "payload_bytes_after", "repair_duration_ms",
	"wire_input_bytes", "decoded_input_bytes", "transform_output_bytes", "transform_added_bytes", "transform_removed_bytes",
	"transform_synthetic_bytes", "transform_duration_ms", "transform_stage_count", "transform_stages",
	"amplification_ratio", "amplification_exceeded", "instrumented", "finalized",
	"failure_class", "endpoint_method", "endpoint_path", "endpoint", "client_profile", "payload_bytes", "message_count", "tool_count",
	"declared_tool_count", "tool_interaction_count", "mcp_tool_count", "builtin_tool_count", "tool_types", "tool_name_hashes",
	"parallel_tool_calls_forced", "tool_stream_repair_kind", "orphan_tool_delta_dropped_count", "invalid_tool_announcement_dropped_count",
	"tool_done_fallback_emitted_count",
	"reasoning_effort", "input_tokens",
	"attempt_count", "gpt_round_count", "fallback_count", "max_attempts", "max_fallbacks", "translator_run_count",
	"first_event_wait_ms", "first_event_timeout_count", "first_event_policy_state", "first_event_policy_reason",
	"first_event_policy_source", "first_event_timeout_ms", "first_event_max_channels", "first_event_max_rounds",
	"first_event_wait_budget_ms", "first_event_wait_budget_exhausted",
	"retry_pressure_wait_ms", "retry_pressure_permit_limit", "retry_pressure_eligible_routes",
	"retry_pressure_degraded_routes", "retry_pressure_throttled",
	"final_success", "final_status", "final_error_type", "final_error_code", "final_provider", "final_model", "final_executor",
	"duration_ms", "time_to_first_chunk_ms", "upstream_chunk_wait_ms", "upstream_chunk_wait_count",
	"stream_duration_ms", "total_duration_ms", "downstream_write_ms", "downstream_write_calls",
	"downstream_flush_ms", "downstream_flush_calls", "chunks_count", "bytes_out",
	"stream_output_tokens", "stream_output_tokens_observed", "output_tokens", "tokens_per_second",
	"client_gone", "cancel_origin", "finish_reason",
	"error_type", "upstream_status", "upstream_error_code", "route_plan",
}

// Format renders a single log entry with custom formatting.
func (m *LogFormatter) Format(entry *log.Entry) ([]byte, error) {
	var buffer *bytes.Buffer
	if entry.Buffer != nil {
		buffer = entry.Buffer
	} else {
		buffer = &bytes.Buffer{}
	}

	timestamp := entry.Time.Format("2006-01-02 15:04:05")
	message := strings.TrimRight(entry.Message, "\r\n")

	reqID := "--------"
	if id, ok := entry.Data["request_id"].(string); ok && id != "" {
		reqID = id
	}

	level := entry.Level.String()
	if level == "warning" {
		level = "warn"
	}
	levelStr := fmt.Sprintf("%-5s", level)

	// Build fields string (only print fields in logFieldOrder)
	var fieldsStr string
	if len(entry.Data) > 0 {
		var fields []string
		for _, k := range logFieldOrder {
			if v, ok := entry.Data[k]; ok {
				fields = append(fields, fmt.Sprintf("%s=%v", k, v))
			}
		}
		if len(fields) > 0 {
			fieldsStr = " " + strings.Join(fields, " ")
		}
	}

	var formatted string
	if entry.Caller != nil {
		formatted = fmt.Sprintf("[%s] [%s] [%s] [%s:%d] %s%s\n", timestamp, reqID, levelStr, filepath.Base(entry.Caller.File), entry.Caller.Line, message, fieldsStr)
	} else {
		formatted = fmt.Sprintf("[%s] [%s] [%s] %s%s\n", timestamp, reqID, levelStr, message, fieldsStr)
	}
	buffer.WriteString(formatted)

	return buffer.Bytes(), nil
}

// SetupBaseLogger configures the shared logrus instance and Gin writers.
// It is safe to call multiple times; initialization happens only once.
func SetupBaseLogger() {
	setupOnce.Do(func() {
		log.SetOutput(os.Stdout)
		log.SetReportCaller(true)
		log.SetFormatter(&LogFormatter{})

		ginInfoWriter = log.StandardLogger().Writer()
		gin.DefaultWriter = ginInfoWriter
		ginErrorWriter = log.StandardLogger().WriterLevel(log.ErrorLevel)
		gin.DefaultErrorWriter = ginErrorWriter
		gin.DebugPrintFunc = func(format string, values ...interface{}) {
			format = strings.TrimRight(format, "\r\n")
			log.StandardLogger().Infof(format, values...)
		}

		log.RegisterExitHandler(closeLogOutputs)
	})
}

// isDirWritable checks if the specified directory exists and is writable by attempting to create and remove a test file.
func isDirWritable(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}

	testFile := filepath.Join(dir, ".perm_test")
	f, err := os.Create(testFile)
	if err != nil {
		return false
	}

	defer func() {
		_ = f.Close()
		_ = os.Remove(testFile)
	}()
	return true
}

// ResolveLogDirectory determines the directory used for application logs.
func ResolveLogDirectory(cfg *config.Config) string {
	logDir := "logs"
	if base := util.WritablePath(); base != "" {
		return filepath.Join(base, "logs")
	}
	if cfg == nil {
		return logDir
	}
	if !isDirWritable(logDir) {
		authDir, err := util.ResolveAuthDir(cfg.AuthDir)
		if err != nil {
			log.Warnf("Failed to resolve auth-dir %q for log directory: %v", cfg.AuthDir, err)
		}
		if authDir != "" {
			logDir = filepath.Join(authDir, "logs")
		}
	}
	return logDir
}

// ConfigureLogOutput switches the global log destination between rotating files and stdout.
// When logsMaxTotalSizeMB > 0, a background cleaner removes the oldest log files in the logs directory
// until the total size is within the limit.
func ConfigureLogOutput(cfg *config.Config) error {
	SetupBaseLogger()

	writerMu.Lock()
	defer writerMu.Unlock()

	logDir := ResolveLogDirectory(cfg)

	protectedPath := ""
	if cfg.LoggingToFile {
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return fmt.Errorf("logging: failed to create log directory: %w", err)
		}
		if logWriter != nil {
			_ = logWriter.Close()
		}
		protectedPath = filepath.Join(logDir, "main.log")
		logWriter = &lumberjack.Logger{
			Filename:   protectedPath,
			MaxSize:    10,
			MaxBackups: 0,
			MaxAge:     0,
			Compress:   false,
		}
		log.SetOutput(logWriter)
	} else {
		if logWriter != nil {
			_ = logWriter.Close()
			logWriter = nil
		}
		log.SetOutput(os.Stdout)
	}

	configureLogDirCleanerLocked(logDir, cfg.LogsMaxTotalSizeMB, protectedPath)
	return nil
}

func closeLogOutputs() {
	writerMu.Lock()
	defer writerMu.Unlock()

	stopLogDirCleanerLocked()

	if logWriter != nil {
		_ = logWriter.Close()
		logWriter = nil
	}
	if ginInfoWriter != nil {
		_ = ginInfoWriter.Close()
		ginInfoWriter = nil
	}
	if ginErrorWriter != nil {
		_ = ginErrorWriter.Close()
		ginErrorWriter = nil
	}
}
