package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	log "github.com/sirupsen/logrus"
)

type oauthCallbackDescriptor struct {
	path       string
	storageKey string
}

var oauthCallbackDescriptors = []oauthCallbackDescriptor{
	{path: "/anthropic/callback", storageKey: "anthropic"},
	{path: "/codex/callback", storageKey: "codex"},
	{path: "/antigravity/callback", storageKey: "antigravity"},
	{path: "/xai/callback", storageKey: "xai"},
}

func (s *Server) registerOAuthCallbackRoutes() {
	for _, descriptor := range oauthCallbackDescriptors {
		s.engine.GET(descriptor.path, s.oauthCallbackHandler(descriptor.storageKey))
	}
}

func (s *Server) oauthCallbackHandler(storageKey string) gin.HandlerFunc {
	return func(c *gin.Context) {
		code := c.Query("code")
		state := c.Query("state")
		errMessage := c.Query("error")
		if errMessage == "" {
			errMessage = c.Query("error_description")
		}

		if state != "" {
			if authDir := s.authDirSnapshot(); authDir != "" {
				if _, errWrite := managementHandlers.WriteOAuthCallbackFileForPendingSession(authDir, storageKey, state, code, errMessage); errWrite != nil {
					log.WithError(errWrite).WithField("provider", storageKey).Debug("oauth callback was not persisted")
				}
			}
		}

		c.Header("Content-Type", "text/html; charset=utf-8")
		c.String(http.StatusOK, oauthCallbackSuccessHTML)
	}
}
