package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func (h *Handler) GetMonitorGPTFirstEventPolicy(c *gin.Context) {
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	days, err := parseBoundedInt(firstQuery(c, "days"), 7, 1, 31)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	model := strings.TrimSpace(firstQuery(c, "model"))
	if model == "" {
		model = "*"
	}
	c.JSON(http.StatusOK, gin.H{
		"model":          model,
		"current":        h.authManager.GPTFirstEventPolicySnapshot(model),
		"daily":          h.authManager.GPTFirstEventDailySnapshots(model, days),
		"server_time":    time.Now(),
		"runtime_scoped": true,
		"durable_log_events": []string{
			"gpt_first_event_observation",
			"gpt_first_event_policy_transition",
			"request_execution_summary",
		},
	})
}
