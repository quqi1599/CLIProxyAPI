package contentaudit

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/api/handlers"
	log "github.com/sirupsen/logrus"
)

const (
	identitySecretEnv = "CPA_AUDIT_IDENTITY_SECRET"
	evidenceKeyEnv    = "CPA_AUDIT_EVIDENCE_KEY"

	defaultMaxBodyBytes = 256 << 20
)

type runtimeState struct {
	cfg                    config.ContentAuditConfig
	matcher                *Matcher
	store                  *Store
	identitySecret         string
	policyPath             string
	storePath              string
	evidenceKeyFingerprint [32]byte
	initErr                error
	modelReview            *modelReviewController
}

// Status reports whether enforcement is configured and ready without exposing secrets.
type Status struct {
	Enabled                 bool    `json:"enabled"`
	AuditOnly               bool    `json:"audit_only"`
	Ready                   bool    `json:"ready"`
	Error                   string  `json:"error,omitempty"`
	PolicyVersion           string  `json:"policy_version,omitempty"`
	KeywordCount            int     `json:"keyword_count"`
	DatabaseAvailable       bool    `json:"database_available"`
	RequireSignedIdentity   bool    `json:"require_signed_identity"`
	AllowUnauditedWebsocket bool    `json:"allow_unaudited_websocket"`
	BlockRuleCount          int     `json:"block_rule_count"`
	ObserveRuleCount        int     `json:"observe_rule_count"`
	DisabledRuleCount       int     `json:"disabled_rule_count"`
	MaxBodyBytes            int64   `json:"max_body_bytes"`
	EvidenceDedupeSeconds   int     `json:"evidence_dedupe_seconds"`
	RawRetentionDays        int     `json:"raw_retention_days"`
	MetadataRetentionDays   int     `json:"metadata_retention_days"`
	ModelReviewMode         string  `json:"model_review_mode"`
	ModelReviewModel        string  `json:"model_review_model,omitempty"`
	ModelReviewReady        bool    `json:"model_review_ready"`
	ModelReviewTimeoutMS    int     `json:"model_review_timeout_ms"`
	ModelReviewAllowMin     float64 `json:"model_review_allow_min_confidence"`
	ModelReviewBlockMin     float64 `json:"model_review_block_min_confidence"`
}

// Service owns the immutable request-time matcher snapshot and encrypted store.
type Service struct {
	state     atomic.Pointer[runtimeState]
	pruneOnce sync.Once
	policyMu  sync.Mutex
	reviewer  Reviewer
}

// NewService initializes a service. Invalid enabled configuration is fail-closed at request time.
func NewService(cfg config.ContentAuditConfig, configFilePath string) *Service {
	return NewServiceWithReviewer(cfg, configFilePath, nil)
}

// NewServiceWithReviewer initializes a service with an optional direct semantic reviewer.
func NewServiceWithReviewer(cfg config.ContentAuditConfig, configFilePath string, reviewer Reviewer) *Service {
	service := &Service{reviewer: reviewer}
	service.Update(cfg, configFilePath)
	return service
}

// Update atomically replaces matcher and configuration state.
func (s *Service) Update(cfg config.ContentAuditConfig, configFilePath string) {
	if s == nil {
		return
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()
	baseDir := filepath.Dir(configFilePath)
	if strings.TrimSpace(configFilePath) == "" || baseDir == "." {
		if cwd, err := os.Getwd(); err == nil {
			baseDir = cwd
		}
	}
	normalizeAuditConfig(&cfg)
	state := &runtimeState{cfg: cfg}
	state.modelReview = newModelReviewController(cfg.ModelReview, s.reviewer)
	state.identitySecret = firstNonEmptySecret(os.Getenv(identitySecretEnv), cfg.IdentitySecret)
	evidenceKey := firstNonEmptySecret(os.Getenv(evidenceKeyEnv), cfg.EvidenceKey)
	state.evidenceKeyFingerprint = sha256.Sum256([]byte(evidenceKey))

	policyPath := resolveAuditPath(baseDir, cfg.PolicyFile)
	state.policyPath = policyPath
	state.storePath = resolveAuditPath(baseDir, cfg.DatabasePath)
	previous := s.state.Load()
	if evidenceKey != "" {
		if previous != nil && previous.store != nil && previous.storePath == state.storePath && previous.evidenceKeyFingerprint == state.evidenceKeyFingerprint && previous.cfg.EvidenceKeyID == cfg.EvidenceKeyID {
			state.store = previous.store
		} else {
			store, err := NewStore(state.storePath, evidenceKey, cfg.EvidenceKeyID)
			if err != nil {
				state.initErr = err
			} else {
				state.store = store
			}
		}
	}

	if cfg.Enabled {
		switch {
		case state.initErr != nil:
		case strings.TrimSpace(cfg.PolicyFile) == "":
			state.initErr = fmt.Errorf("content audit policy file is required")
		case evidenceKey == "":
			state.initErr = fmt.Errorf("%s is required", evidenceKeyEnv)
		case cfg.RequireSignedIdentity && state.identitySecret == "":
			state.initErr = fmt.Errorf("%s is required", identitySecretEnv)
		default:
			matcher, err := LoadPolicy(policyPath)
			if err != nil {
				state.initErr = err
			} else {
				state.matcher = matcher
			}
		}
	}
	s.state.Store(state)
	if state.store != nil && state.matcher != nil {
		if err := ensurePolicyBaseline(state); err != nil {
			log.WithError(err).Warn("content audit policy baseline persistence failed")
		}
	}
	if state.store != nil {
		s.pruneOnce.Do(func() { go s.pruneLoop() })
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := state.store.Prune(ctx, cfg.RawRetentionDays, cfg.MetadataRetentionDays); err != nil {
			log.WithError(err).Warn("content audit retention cleanup failed")
		}
		cancel()
	}
	if cfg.Enabled && state.initErr != nil {
		log.WithError(state.initErr).Error("content audit is enabled but unavailable; auditable requests will fail closed")
	} else if cfg.Enabled {
		log.WithFields(log.Fields{
			"policy_version": state.matcher.Version(),
			"keyword_count":  state.matcher.KeywordCount(),
			"database_path":  state.storePath,
			"audit_only":     state.cfg.AuditOnly,
		}).Info("content audit enabled")
	}
}

func normalizeAuditConfig(cfg *config.ContentAuditConfig) {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = defaultMaxBodyBytes
	}
	if cfg.RawRetentionDays <= 0 {
		cfg.RawRetentionDays = 30
	}
	if cfg.EvidenceDedupeSeconds == 0 {
		cfg.EvidenceDedupeSeconds = 600
	} else if cfg.EvidenceDedupeSeconds < 0 {
		cfg.EvidenceDedupeSeconds = 0
	} else if cfg.EvidenceDedupeSeconds > 86400 {
		cfg.EvidenceDedupeSeconds = 86400
	}
	if cfg.MetadataRetentionDays <= 0 {
		cfg.MetadataRetentionDays = 180
	}
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join("content-audit", "audit.db")
	}
	if cfg.EvidenceKeyID == "" {
		cfg.EvidenceKeyID = "primary-v1"
	}
	normalizeModelReviewConfig(&cfg.ModelReview)
}

func normalizeModelReviewConfig(cfg *config.ContentAuditModelReviewConfig) {
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch cfg.Mode {
	case ModelReviewModeShadow, ModelReviewModeEnforce:
	default:
		cfg.Mode = ModelReviewModeOff
	}
	if strings.TrimSpace(cfg.Model) == "" {
		cfg.Model = "codex-auto-review"
	}
	if cfg.Rules != nil {
		selectedRules := make([]string, 0, len(cfg.Rules))
		seenRules := make(map[string]struct{}, len(cfg.Rules))
		for _, rule := range cfg.Rules {
			rule = strings.TrimSpace(rule)
			if rule == "" {
				continue
			}
			if _, exists := seenRules[rule]; exists {
				continue
			}
			seenRules[rule] = struct{}{}
			selectedRules = append(selectedRules, rule)
		}
		cfg.Rules = selectedRules
	}
	if strings.TrimSpace(cfg.PromptVersion) == "" {
		cfg.PromptVersion = "cpa-audit-review-v1"
	}
	if cfg.TimeoutMilliseconds <= 0 || cfg.TimeoutMilliseconds > 4000 {
		cfg.TimeoutMilliseconds = 3500
	}
	if cfg.QueueTimeoutMilliseconds <= 0 || cfg.QueueTimeoutMilliseconds > 500 {
		cfg.QueueTimeoutMilliseconds = 100
	}
	if cfg.MaxConcurrent <= 0 || cfg.MaxConcurrent > 128 {
		cfg.MaxConcurrent = 16
	}
	if cfg.CacheSeconds < 0 {
		cfg.CacheSeconds = 0
	} else if cfg.CacheSeconds == 0 {
		cfg.CacheSeconds = 600
	} else if cfg.CacheSeconds > 3600 {
		cfg.CacheSeconds = 3600
	}
	if cfg.MaxInputBytes <= 0 || cfg.MaxInputBytes > 256<<10 {
		cfg.MaxInputBytes = 32 << 10
	}
	if cfg.MinConfidence <= 0 || cfg.MinConfidence > 1 {
		cfg.MinConfidence = 0.90
	}
	if cfg.AllowMinConfidence <= 0 || cfg.AllowMinConfidence > 1 {
		cfg.AllowMinConfidence = cfg.MinConfidence
	}
	if cfg.BlockMinConfidence <= 0 || cfg.BlockMinConfidence > 1 {
		cfg.BlockMinConfidence = cfg.MinConfidence
	}
	if cfg.CircuitFailureThreshold <= 0 || cfg.CircuitFailureThreshold > 100 {
		cfg.CircuitFailureThreshold = 5
	}
	if cfg.CircuitOpenSeconds <= 0 || cfg.CircuitOpenSeconds > 300 {
		cfg.CircuitOpenSeconds = 30
	}
}

func firstNonEmptySecret(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func resolveAuditPath(baseDir, path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(baseDir, path)
}

func (s *Service) pruneLoop() {
	ticker := time.NewTicker(24 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		state := s.state.Load()
		if state == nil || state.store == nil {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := state.store.Prune(ctx, state.cfg.RawRetentionDays, state.cfg.MetadataRetentionDays)
		cancel()
		if err != nil {
			log.WithError(err).Warn("content audit retention cleanup failed")
		}
	}
}

// Status returns the current immutable configuration snapshot.
func (s *Service) Status() Status {
	state := s.state.Load()
	if state == nil {
		return Status{}
	}
	status := Status{
		Enabled:                 state.cfg.Enabled,
		AuditOnly:               state.cfg.AuditOnly,
		Ready:                   state.initErr == nil,
		DatabaseAvailable:       state.store != nil,
		RequireSignedIdentity:   state.cfg.RequireSignedIdentity,
		AllowUnauditedWebsocket: state.cfg.AllowUnauditedWebsocket,
		MaxBodyBytes:            state.cfg.MaxBodyBytes,
		EvidenceDedupeSeconds:   state.cfg.EvidenceDedupeSeconds,
		RawRetentionDays:        state.cfg.RawRetentionDays,
		MetadataRetentionDays:   state.cfg.MetadataRetentionDays,
		ModelReviewMode:         state.cfg.ModelReview.Mode,
		ModelReviewModel:        state.cfg.ModelReview.Model,
		ModelReviewReady:        state.modelReview != nil,
		ModelReviewTimeoutMS:    state.cfg.ModelReview.TimeoutMilliseconds,
		ModelReviewAllowMin:     state.cfg.ModelReview.AllowMinConfidence,
		ModelReviewBlockMin:     state.cfg.ModelReview.BlockMinConfidence,
	}
	if state.matcher != nil {
		status.PolicyVersion = state.matcher.Version()
		status.KeywordCount = state.matcher.KeywordCount()
		status.BlockRuleCount, status.ObserveRuleCount, status.DisabledRuleCount = state.matcher.RuleActionCounts()
	}
	if state.initErr != nil {
		status.Error = state.initErr.Error()
	}
	return status
}

// Middleware verifies internal identity, scans prompt text, and stops matches before routing.
func (s *Service) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		state := s.state.Load()
		if c == nil || c.Request == nil {
			return
		}
		if state == nil || !state.cfg.Enabled || !isAuditableRequest(c.Request) {
			StripIdentityHeaders(c.Request.Header)
			c.Next()
			return
		}
		if isAuditWebSocketRequest(c.Request) && (state.cfg.AuditOnly || state.cfg.AllowUnauditedWebsocket) {
			StripIdentityHeaders(c.Request.Header)
			c.Next()
			return
		}
		if state.cfg.AuditOnly && (state.initErr != nil || state.matcher == nil || state.store == nil) {
			StripIdentityHeaders(c.Request.Header)
			c.Next()
			return
		}

		identity, identityErr := VerifyIdentity(c.Request, state.identitySecret, state.cfg.RequireSignedIdentity, time.Now(), 5*time.Minute)
		StripIdentityHeaders(c.Request.Header)
		if identityErr != nil && !state.cfg.AuditOnly {
			writeAuditError(c, http.StatusUnauthorized, "cpa_audit_identity_invalid", "authentication_error", "CPA 审计身份校验失败，请联系管理员检查 NewAPI 渠道签名配置。", "", "")
			return
		}
		if identityErr != nil {
			identity = Identity{}
		}
		if state.initErr != nil || state.matcher == nil || state.store == nil {
			writeAuditError(c, http.StatusServiceUnavailable, "cpa_content_audit_unavailable", "server_error", "CPA 内容审计当前不可用，请稍后重试或联系管理员。", "", "")
			return
		}
		if isAuditWebSocketRequest(c.Request) {
			writeAuditError(c, http.StatusBadRequest, "cpa_content_audit_blocked", "content_safety_blocked", "CPA 内容审计开启时暂不允许未审计的 WebSocket 请求。请改用对应的 HTTP POST 接口。", "", "websocket_not_audited")
			return
		}

		extracted, requestBytes, err := readAuditRequest(c, state.cfg.MaxBodyBytes)
		if err != nil {
			handlers.WriteRequestBodyError(c, err)
			c.Abort()
			return
		}
		decision := state.matcher.MatchScoped(extracted.EnforcementText, extracted.Text, extracted.Continuation)
		if !decision.Matched {
			c.Next()
			return
		}

		auditID := newAuditID()
		requestID := identity.RequestID
		if requestID == "" {
			requestID = sanitizeIdentityText(firstNonEmptySecret(c.GetHeader("X-Request-ID"), c.GetHeader("X-Request-Id")), 128)
		}
		model := strings.TrimSpace(extracted.Model)
		if model == "" {
			model = identity.Model
		}
		modelReview := modelReviewOutcome{}
		if decision.ModelReview && state.modelReview != nil {
			modelReview = state.modelReview.review(c.Request.Context(), ModelReviewRequest{
				Text:        extracted.EnforcementText,
				MatchedTerm: decision.MatchedTerm,
				RuleID:      decision.RuleID,
				Category:    decision.Category,
				Severity:    decision.Severity,
			})
		}
		keywordShouldBlock := decision.Action == RuleActionBlock
		shouldBlock := keywordShouldBlock
		if state.cfg.ModelReview.Mode == ModelReviewModeEnforce && modelReview.Reviewed {
			switch modelReview.Decision {
			case ModelReviewBlock:
				if modelReview.Confidence >= state.cfg.ModelReview.BlockMinConfidence {
					shouldBlock = true
				}
			case ModelReviewAllow:
				if modelReview.Confidence >= state.cfg.ModelReview.AllowMinConfidence {
					shouldBlock = false
				}
			}
		}
		shouldBlock = !state.cfg.AuditOnly && shouldBlock
		finalAction := ModelReviewAllow
		if shouldBlock {
			finalAction = ModelReviewBlock
		}
		event := Event{
			ID:                    auditID,
			CreatedAt:             time.Now().Unix(),
			RequestID:             requestID,
			UserID:                identity.UserID,
			TokenID:               identity.TokenID,
			TokenName:             identity.TokenName,
			Method:                c.Request.Method,
			Path:                  c.Request.URL.Path,
			Protocol:              protocolForPath(c.Request.URL.Path),
			Model:                 model,
			Stream:                extracted.Stream,
			Category:              decision.Category,
			Severity:              decision.Severity,
			RuleID:                decision.RuleID,
			Action:                decision.Action,
			FinalAction:           finalAction,
			MatchedTerm:           decision.MatchedTerm,
			MatchedRoles:          extracted.EnforcementMatchedRoles(decision.MatchedTerm),
			PolicyVersion:         decision.PolicyVersion,
			RequestBytes:          requestBytes,
			IdentityVerified:      identity.Verified,
			UpstreamSent:          !shouldBlock,
			EvidenceStatus:        "encrypted",
			EvidenceKeyID:         state.cfg.EvidenceKeyID,
			ReviewLabel:           defaultReviewLabel,
			ModelReviewMode:       state.cfg.ModelReview.Mode,
			ModelReviewModel:      modelReview.Model,
			ModelReviewDecision:   modelReview.Decision,
			ModelReviewCategory:   modelReview.Category,
			ModelReviewConfidence: modelReview.Confidence,
			ModelReviewLatencyMS:  modelReview.Latency.Milliseconds(),
			ModelReviewCacheHit:   modelReview.CacheHit,
			ModelReviewFallback:   modelReview.Fallback,
			fingerprintMaterial:   extracted.FingerprintMaterial(),
			dedupeWindow:          time.Duration(state.cfg.EvidenceDedupeSeconds) * time.Second,
		}
		storeCtx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 3*time.Second)
		err = state.store.Record(storeCtx, event, extracted.Evidence)
		cancel()
		if err != nil {
			log.WithFields(log.Fields{
				"audit_id":   auditID,
				"request_id": requestID,
				"category":   decision.Category,
				"rule_id":    decision.RuleID,
			}).WithError(err).Error("failed to persist matched content audit event")
			c.Header("X-CPA-Audit-Evidence-Status", "storage_failed")
		}
		if !shouldBlock {
			c.Next()
			return
		}
		writeAuditError(c, http.StatusBadRequest, "cpa_content_audit_blocked", "content_safety_blocked", "请求内容触发了本地安全审计，已在发送至上游模型前停止。请移除违法、色情、暴力、电诈、毒品或未授权网络攻击相关的请求后重试。", auditID, decision.Category)
	}
}

func isAuditableRequest(request *http.Request) bool {
	if request == nil {
		return false
	}
	path := request.URL.Path
	pathInScope := strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/backend-api/codex/") || strings.HasPrefix(path, "/openai/v1/")
	return pathInScope && (request.Method == http.MethodPost || isAuditWebSocketRequest(request))
}

func isAuditWebSocketRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket")
}

func readAuditRequest(c *gin.Context, maxBodyBytes int64) (ExtractedRequest, int64, error) {
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		form, err := handlers.ParseMultipartFormWithPolicy(c, maxBodyBytes, 1<<20, maxBodyBytes)
		if err != nil {
			return ExtractedRequest{}, 0, err
		}
		requestBytes := c.Request.ContentLength
		if requestBytes < 0 {
			requestBytes = 0
		}
		return ExtractMultipartRequest(form), requestBytes, nil
	}
	body, err := handlers.ReadRequestBodyWithLimits(c, maxBodyBytes, maxBodyBytes)
	if err != nil {
		return ExtractedRequest{}, 0, err
	}
	return ExtractJSONRequest(body), int64(len(body)), nil
}

func protocolForPath(path string) string {
	switch {
	case strings.HasPrefix(path, "/v1beta/"):
		return "gemini"
	case path == "/v1/messages" || path == "/v1/messages/count_tokens":
		return "claude"
	case strings.Contains(path, "/responses"):
		return "openai_responses"
	case strings.Contains(path, "/images"):
		return "openai_images"
	case strings.Contains(path, "/videos"):
		return "openai_videos"
	default:
		return "openai"
	}
}

func newAuditID() string {
	random := make([]byte, 12)
	if _, err := rand.Read(random); err == nil {
		return "aud_" + hex.EncodeToString(random)
	}
	return "aud_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func writeAuditError(c *gin.Context, status int, code, errorType, message, auditID, category string) {
	c.Header("Cache-Control", "no-store")
	c.Header("X-CPA-Local-Guard", "content-audit")
	c.Header("X-CPA-Retryable", "false")
	if auditID != "" {
		c.Header("X-CPA-Audit-ID", auditID)
	}
	if category != "" {
		c.Header("X-CPA-Local-Guard-Category", category)
	}
	path := ""
	if c.Request != nil && c.Request.URL != nil {
		path = c.Request.URL.Path
	}
	switch {
	case path == "/v1/messages" || path == "/v1/messages/count_tokens":
		c.AbortWithStatusJSON(status, gin.H{
			"type":  "error",
			"error": gin.H{"type": errorType, "message": message, "code": code, "audit_id": auditID},
		})
	case strings.HasPrefix(path, "/v1beta/"):
		c.AbortWithStatusJSON(status, gin.H{
			"error": gin.H{"code": status, "message": message, "status": "INVALID_ARGUMENT", "reason": code, "audit_id": auditID},
		})
	default:
		c.AbortWithStatusJSON(status, gin.H{
			"error": gin.H{"message": message, "type": errorType, "code": code, "audit_id": auditID},
		})
	}
}

func (s *Service) currentStore() (*runtimeState, error) {
	state := s.state.Load()
	if state == nil || state.store == nil {
		return nil, fmt.Errorf("content audit database is unavailable")
	}
	return state, nil
}

// List returns metadata-only audit events.
func (s *Service) List(ctx context.Context, filter ListFilter) (ListResult, error) {
	state, err := s.currentStore()
	if err != nil {
		return ListResult{}, err
	}
	return state.store.List(ctx, filter)
}

// Get returns metadata and access history without raw evidence.
func (s *Service) Get(ctx context.Context, eventID string) (EventDetail, error) {
	state, err := s.currentStore()
	if err != nil {
		return EventDetail{}, err
	}
	return state.store.Get(ctx, eventID)
}

// Reveal decrypts one event for an authenticated Management API request.
func (s *Service) Reveal(ctx context.Context, eventID, actor string) ([]byte, error) {
	state, err := s.currentStore()
	if err != nil {
		return nil, err
	}
	evidence, err := state.store.Reveal(ctx, eventID, "management console evidence view", actor)
	return []byte(evidence), err
}

// UpdateReview records a policy-tuning label only; it never changes user, token, or session state.
func (s *Service) UpdateReview(ctx context.Context, eventID, label, note, reason, actor string) error {
	state, err := s.currentStore()
	if err != nil {
		return err
	}
	return state.store.UpdateReview(ctx, eventID, label, note, reason, actor)
}

// RecordAccess records a copy or download action after the UI reveals evidence.
func (s *Service) RecordAccess(ctx context.Context, eventID, action, reason, actor string) error {
	state, err := s.currentStore()
	if err != nil {
		return err
	}
	switch action {
	case "copy", "download":
	default:
		return fmt.Errorf("invalid content audit access action")
	}
	return state.store.RecordAccess(ctx, eventID, action, reason, actor)
}

// IsNotFound reports whether a store operation did not find the requested event.
func IsNotFound(err error) bool {
	return errors.Is(err, sql.ErrNoRows)
}
