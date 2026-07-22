package auth

import (
	"context"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

type translatorRegistryExecutor struct {
	from sdktranslator.Format
	to   sdktranslator.Format
}

func (executor *translatorRegistryExecutor) Identifier() string { return "translator-registry" }

func (executor *translatorRegistryExecutor) Execute(ctx context.Context, _ *Auth, req cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	body := sdktranslator.RegistryFromContext(ctx).TranslateRequest(executor.from, executor.to, req.Model, req.Payload, false)
	return cliproxyexecutor.Response{Payload: body}, nil
}

func (executor *translatorRegistryExecutor) ExecuteStream(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, nil
}

func (executor *translatorRegistryExecutor) Refresh(_ context.Context, auth *Auth) (*Auth, error) {
	return auth, nil
}

func (executor *translatorRegistryExecutor) CountTokens(context.Context, *Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (executor *translatorRegistryExecutor) HttpRequest(context.Context, *Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func TestManagerInjectsTranslatorRegistryIntoExecution(t *testing.T) {
	const model = "translator-registry-model"
	const authID = "translator-registry-auth"
	from := sdktranslator.Format("manager-isolated-from")
	to := sdktranslator.Format("manager-isolated-to")
	translationRegistry := sdktranslator.NewRegistry()
	translationRegistry.Register(from, to, func(_ string, _ []byte, _ bool) []byte {
		return []byte(`{"registry":"isolated"}`)
	}, sdktranslator.ResponseTransform{})

	executor := &translatorRegistryExecutor{from: from, to: to}
	manager := NewManager(nil, nil, nil)
	manager.SetTranslatorRegistry(translationRegistry)
	manager.RegisterExecutor(executor)

	modelRegistry := registry.GetGlobalRegistry()
	modelRegistry.RegisterClient(authID, executor.Identifier(), []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { modelRegistry.UnregisterClient(authID) })
	if _, err := manager.Register(context.Background(), &Auth{ID: authID, Provider: executor.Identifier()}); err != nil {
		t.Fatalf("register auth: %v", err)
	}

	response, err := manager.Execute(context.Background(), []string{executor.Identifier()}, cliproxyexecutor.Request{
		Model: model, Payload: []byte(`{"registry":"default"}`),
	}, cliproxyexecutor.Options{})
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if string(response.Payload) != `{"registry":"isolated"}` {
		t.Fatalf("response payload = %s", response.Payload)
	}
	if sdktranslator.Default().HasRequestTransformer(from, to) {
		t.Fatal("manager registry registration leaked into default facade")
	}
}
