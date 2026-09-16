package auth

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/compat"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

func shouldPrefilterDeepSeekResponses(model string, opts cliproxyexecutor.Options) bool {
	return thinking.IsDeepSeekV4Model(model) && opts.SourceFormat == sdktranslator.FormatOpenAIResponse && pinnedAuthIDFromMetadata(opts.Metadata) == ""
}

func prefilterDeepSeekResponsesAuths(auths []*Auth, model string, opts cliproxyexecutor.Options) ([]*Auth, int) {
	if !shouldPrefilterDeepSeekResponses(model, opts) {
		return auths, 0
	}
	compatible := make([]*Auth, 0, len(auths))
	for _, a := range auths {
		if a == nil {
			continue
		}
		identity := routePlanProviderIdentity(a, "")
		// Claude executors may translate the request into Messages; only reject
		// Coding routes which would send the unsupported native Responses path.
		if isOpenAICompatAPIKeyAuth(a) && !strings.EqualFold(identity.ExecutorKey, "claude") && !strings.EqualFold(a.Provider, "claude") && compat.DeepSeekCodingResponsesUnsupported(a.Attributes["base_url"], "/responses") {
			continue
		}
		compatible = append(compatible, a)
	}
	if len(compatible) == 0 {
		// Let the executor return its actionable capability error, not a generic
		// no-credentials error, when no alternative route is configured.
		return auths, 0
	}
	return compatible, len(auths) - len(compatible)
}
