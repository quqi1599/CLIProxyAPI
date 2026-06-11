package logging

import (
	"context"
	"testing"
)

func TestEndpointMetadataParsesLegacyEndpoint(t *testing.T) {
	ctx := WithEndpoint(context.Background(), "POST /v1/messages")

	if got := GetEndpoint(ctx); got != "POST /v1/messages" {
		t.Fatalf("GetEndpoint() = %q, want legacy endpoint", got)
	}
	if got := GetEndpointMethod(ctx); got != "POST" {
		t.Fatalf("GetEndpointMethod() = %q, want POST", got)
	}
	if got := GetEndpointPath(ctx); got != "/v1/messages" {
		t.Fatalf("GetEndpointPath() = %q, want /v1/messages", got)
	}
}

func TestEndpointMetadataJoinsEndpointParts(t *testing.T) {
	ctx := WithEndpointParts(context.Background(), "get", "/v1/responses")

	if got := GetEndpoint(ctx); got != "GET /v1/responses" {
		t.Fatalf("GetEndpoint() = %q, want joined endpoint", got)
	}
	if got := GetEndpointMethod(ctx); got != "GET" {
		t.Fatalf("GetEndpointMethod() = %q, want GET", got)
	}
	if got := GetEndpointPath(ctx); got != "/v1/responses" {
		t.Fatalf("GetEndpointPath() = %q, want /v1/responses", got)
	}
}
