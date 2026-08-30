package api

import (
	"context"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/contentaudit"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

type contentAuditReviewExecutorFunc func(context.Context, coreexecutor.Request, coreexecutor.Options) (coreexecutor.Response, error)

func (f contentAuditReviewExecutorFunc) ExecuteContentAuditReview(ctx context.Context, request coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
	return f(ctx, request, options)
}

func TestCodexContentAuditReviewerUsesDirectCodexExecution(t *testing.T) {
	reviewer := &codexContentAuditReviewer{executor: contentAuditReviewExecutorFunc(func(_ context.Context, request coreexecutor.Request, options coreexecutor.Options) (coreexecutor.Response, error) {
		if request.Model != "codex-auto-review" || options.Stream {
			t.Fatalf("request=%#v options=%#v", request, options)
		}
		body := string(request.Payload)
		if !strings.Contains(body, "CURRENT_USER_TEXT") || !strings.Contains(body, "synthetic user text") || !strings.Contains(body, "matched_term") ||
			!strings.Contains(body, "pornographic") || !strings.Contains(body, "gambling operation") {
			t.Fatalf("payload=%s", body)
		}
		return coreexecutor.Response{Payload: []byte(`{"output":[{"type":"message","content":[{"type":"output_text","text":"{\"decision\":\"allow\",\"category\":\"jailbreak\",\"confidence\":0.98,\"reason_codes\":[\"SAFETY_CONTEXT\"]}"}]}]}`)}, nil
	})}
	result, err := reviewer.Review(t.Context(), contentaudit.ModelReviewRequest{
		Model: "codex-auto-review", Text: "synthetic user text", MatchedTerm: "review fixture", RuleID: "rule", Category: "jailbreak", Severity: "critical",
	})
	if err != nil || result.Decision != contentaudit.ModelReviewAllow || result.Confidence != 0.98 {
		t.Fatalf("Review()=%#v err=%v", result, err)
	}
}

func TestExtractResponseOutputTextRejectsEmptyEnvelope(t *testing.T) {
	if _, err := extractResponseOutputText([]byte(`{"output":[]}`)); err == nil {
		t.Fatal("extractResponseOutputText() error=nil")
	}
}
