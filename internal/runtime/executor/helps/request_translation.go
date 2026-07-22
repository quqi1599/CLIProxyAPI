package helps

import (
	"bytes"
	"context"

	internalpayload "github.com/router-for-me/CLIProxyAPI/v7/internal/payload"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// TranslateRequestGuarded runs the legacy translator and rejects amplified
// output before callers retain another copy, retry, or contact an upstream.
// The translator's initial allocation necessarily happens before this check.
func TranslateRequestGuarded(
	ctx context.Context,
	stage string,
	from, to sdktranslator.Format,
	model string,
	body []byte,
	stream bool,
	override internalpayload.AmplificationOverride,
) ([]byte, error) {
	translated := sdktranslator.RegistryFromContext(ctx).TranslateRequest(from, to, model, body, stream)
	if err := internalpayload.EnforceRequestTransform(ctx, stage, int64(len(body)), int64(len(translated)), override); err != nil {
		return nil, err
	}
	return translated, nil
}

// TranslateRequestPairGuarded avoids retaining two translated copies when the
// original and active request bodies are identical, and bounds both results
// when they differ.
func TranslateRequestPairGuarded(
	ctx context.Context,
	stage string,
	from, to sdktranslator.Format,
	model string,
	original, body []byte,
	stream bool,
	override internalpayload.AmplificationOverride,
) ([]byte, []byte, error) {
	if bytes.Equal(original, body) {
		translated, errTranslate := TranslateRequestGuarded(ctx, stage, from, to, model, body, stream, override)
		return translated, translated, errTranslate
	}
	originalTranslated, errOriginal := TranslateRequestGuarded(ctx, stage+".original", from, to, model, original, stream, override)
	if errOriginal != nil {
		return nil, nil, errOriginal
	}
	translated, errTranslate := TranslateRequestGuarded(ctx, stage+".active", from, to, model, body, stream, override)
	if errTranslate != nil {
		return nil, nil, errTranslate
	}
	return originalTranslated, translated, nil
}
