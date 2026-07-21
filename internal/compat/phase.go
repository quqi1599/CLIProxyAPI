// Package compat defines deterministic compatibility policy metadata.
package compat

// Phase identifies one fixed stage in the compatibility pipeline.
type Phase string

const (
	PreTranslateInspect         Phase = "PreTranslateInspect"
	PostTranslateCanonicalize   Phase = "PostTranslateCanonicalize"
	ApplyThinking               Phase = "ApplyThinking"
	RepairHistory               Phase = "RepairHistory"
	ProviderCapabilityScrub     Phase = "ProviderCapabilityScrub"
	ProviderQuirkPatch          Phase = "ProviderQuirkPatch"
	ApplyUserPayloadConfig      Phase = "ApplyUserPayloadConfig"
	PostConfigRevalidate        Phase = "PostConfigRevalidate"
	FinalizeHeadersAndSignature Phase = "FinalizeHeadersAndSignature"
	AmplificationGuard          Phase = "AmplificationGuard"
)

// Phases returns the compatibility phases in execution order.
func Phases() []Phase {
	return []Phase{
		PreTranslateInspect,
		PostTranslateCanonicalize,
		ApplyThinking,
		RepairHistory,
		ProviderCapabilityScrub,
		ProviderQuirkPatch,
		ApplyUserPayloadConfig,
		PostConfigRevalidate,
		FinalizeHeadersAndSignature,
		AmplificationGuard,
	}
}

func phaseRank(phase Phase) int {
	switch phase {
	case PreTranslateInspect:
		return 0
	case PostTranslateCanonicalize:
		return 1
	case ApplyThinking:
		return 2
	case RepairHistory:
		return 3
	case ProviderCapabilityScrub:
		return 4
	case ProviderQuirkPatch:
		return 5
	case ApplyUserPayloadConfig:
		return 6
	case PostConfigRevalidate:
		return 7
	case FinalizeHeadersAndSignature:
		return 8
	case AmplificationGuard:
		return 9
	default:
		return -1
	}
}
