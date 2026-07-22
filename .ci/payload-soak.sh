#!/usr/bin/env bash
set -Eeuo pipefail

: "${CPA_SOAK_API_KEY:?CPA_SOAK_API_KEY is required}"
: "${CPA_SOAK_MANAGEMENT_KEY:?CPA_SOAK_MANAGEMENT_KEY is required}"
: "${CPA_SOAK_MODEL:?CPA_SOAK_MODEL is required}"
: "${CPA_SOAK_EXPECTED_COMMIT:?CPA_SOAK_EXPECTED_COMMIT is required}"

base_url=${CPA_SOAK_BASE_URL:-http://127.0.0.1:8317}
duration=${CPA_SOAK_DURATION:-12h}
concurrency=${CPA_SOAK_CONCURRENCY:-8}
request_timeout=${CPA_SOAK_REQUEST_TIMEOUT:-2m}
health_timeout=${CPA_SOAK_HEALTH_TIMEOUT:-5s}
recovery_timeout=${CPA_SOAK_RECOVERY_TIMEOUT:-2m}
recovery_interval=${CPA_SOAK_RECOVERY_INTERVAL:-5s}
scenario_timeout=${CPA_SOAK_SCENARIO_TIMEOUT:-30s}
websocket_idle=${CPA_SOAK_WEBSOCKET_IDLE:-2s}

exec go run ./cmd/payload-soak \
	-release-gate=true \
	-chaos=true \
	-responses-websocket=true \
	-base-url "$base_url" \
	-model "$CPA_SOAK_MODEL" \
	-expected-commit "$CPA_SOAK_EXPECTED_COMMIT" \
	-duration "$duration" \
	-concurrency "$concurrency" \
	-request-timeout "$request_timeout" \
	-health-timeout "$health_timeout" \
	-recovery-timeout "$recovery_timeout" \
	-recovery-interval "$recovery_interval" \
	-scenario-timeout "$scenario_timeout" \
	-websocket-idle "$websocket_idle"
