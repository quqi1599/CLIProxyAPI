package management

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentaudit"
)

type contentAuditReviewRequest struct {
	Label  string `json:"label"`
	Note   string `json:"note"`
	Reason string `json:"reason"`
}

type contentAuditAccessRequest struct {
	Action string `json:"action"`
	Reason string `json:"reason"`
}

type contentAuditPolicyRequest struct {
	Policy contentaudit.Policy `json:"policy"`
	Reason string              `json:"reason"`
}

type contentAuditPolicyRollbackRequest struct {
	Reason string `json:"reason"`
}

func (h *Handler) contentAuditService() *contentaudit.Service {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.contentAudit
}

// GetContentAuditStatus returns readiness and policy statistics without secrets.
func (h *Handler) GetContentAuditStatus(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	c.JSON(http.StatusOK, service.Status())
}

// GetContentAuditPolicy returns the active managed policy and rollback history.
func (h *Handler) GetContentAuditPolicy(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	document, err := service.CurrentPolicy(c.Request.Context())
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, document)
}

// UpdateContentAuditPolicy validates, persists, and hot-reloads a managed policy.
func (h *Handler) UpdateContentAuditPolicy(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	var request contentAuditPolicyRequest
	if err := decodeManagementJSONBody(c, maxManagementJSONBodyBytes, &request); err != nil {
		writeManagementRequestBodyError(c, err)
		return
	}
	document, err := service.ApplyPolicy(c.Request.Context(), request.Policy, request.Reason, c.ClientIP())
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, document)
}

// RollbackContentAuditPolicy activates a prior policy snapshot as a new revision.
func (h *Handler) RollbackContentAuditPolicy(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	versionID, err := strconv.ParseInt(strings.TrimSpace(c.Param("id")), 10, 64)
	if err != nil || versionID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid content audit policy version"})
		return
	}
	var request contentAuditPolicyRollbackRequest
	if err = decodeManagementJSONBody(c, maxManagementJSONBodyBytes, &request); err != nil {
		writeManagementRequestBodyError(c, err)
		return
	}
	document, err := service.RollbackPolicy(c.Request.Context(), versionID, request.Reason, c.ClientIP())
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, document)
}

// ListContentAuditEvents returns metadata only; raw prompts are never included.
func (h *Handler) ListContentAuditEvents(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	filter := contentaudit.ListFilter{
		Search:         c.Query("search"),
		Category:       c.Query("category"),
		Severity:       c.Query("severity"),
		ReviewLabel:    c.Query("review_label"),
		UserID:         parseAuditInt64(c.Query("user_id")),
		TokenID:        parseAuditInt64(c.Query("token_id")),
		MatchedRole:    c.Query("matched_role"),
		Fingerprint:    c.Query("content_fingerprint"),
		DuplicatesOnly: strings.EqualFold(strings.TrimSpace(c.Query("duplicates_only")), "true") || c.Query("duplicates_only") == "1",
		Page:           int(parseAuditInt64(c.Query("page"))),
		PageSize:       int(parseAuditInt64(c.Query("page_size"))),
	}
	result, err := service.List(c.Request.Context(), filter)
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// GetContentAuditEvent returns metadata and evidence access history.
func (h *Handler) GetContentAuditEvent(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	result, err := service.Get(c.Request.Context(), strings.TrimSpace(c.Param("id")))
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// RevealContentAuditEvidence decrypts evidence for an authenticated Management API request.
func (h *Handler) RevealContentAuditEvidence(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	evidence, err := service.Reveal(
		c.Request.Context(),
		strings.TrimSpace(c.Param("id")),
		c.ClientIP(),
	)
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"event_id": c.Param("id"),
		"evidence": json.RawMessage(evidence),
	})
}

// ReviewContentAuditEvent stores a tuning label and never bans or unbans any account.
func (h *Handler) ReviewContentAuditEvent(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	var request contentAuditReviewRequest
	if err := decodeManagementJSONBody(c, maxManagementJSONBodyBytes, &request); err != nil {
		writeManagementRequestBodyError(c, err)
		return
	}
	err := service.UpdateReview(c.Request.Context(), strings.TrimSpace(c.Param("id")), request.Label, request.Note, request.Reason, c.ClientIP())
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

// RecordContentAuditAccess records a copy or download performed after reveal.
func (h *Handler) RecordContentAuditAccess(c *gin.Context) {
	service := h.contentAuditService()
	if service == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "content audit service is unavailable"})
		return
	}
	var request contentAuditAccessRequest
	if err := decodeManagementJSONBody(c, maxManagementJSONBodyBytes, &request); err != nil {
		writeManagementRequestBodyError(c, err)
		return
	}
	err := service.RecordAccess(c.Request.Context(), strings.TrimSpace(c.Param("id")), request.Action, request.Reason, c.ClientIP())
	if err != nil {
		writeContentAuditManagementError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func parseAuditInt64(value string) int64 {
	parsed, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return parsed
}

func writeContentAuditManagementError(c *gin.Context, err error) {
	status := http.StatusBadRequest
	message := err.Error()
	switch {
	case contentaudit.IsNotFound(err):
		status = http.StatusNotFound
	case strings.Contains(strings.ToLower(message), "unavailable"):
		status = http.StatusServiceUnavailable
	}
	c.JSON(status, gin.H{"error": message})
}
