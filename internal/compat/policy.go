package compat

import "context"

// EndpointKind identifies a compatibility endpoint such as chat or responses.
type EndpointKind string

// ExecutionMode identifies a request mode such as stream or non-stream.
type ExecutionMode string

// Format identifies a source or target wire format.
type Format string

// TransformResult contains the transformed payload and controlled accounting
// metadata. Downgrades must be declared by the owning policy.
type TransformResult struct {
	Payload        []byte
	SyntheticBytes int64
	Downgrades     []string
}

// TransformFunc applies one compatibility policy to an immutable input payload.
type TransformFunc func(context.Context, []byte) (TransformResult, error)

// MatchSpec describes the requests to which a policy applies. Empty fields are
// wildcards.
type MatchSpec struct {
	ProviderFamily string          `json:"provider_family,omitempty"`
	CompatKind     string          `json:"compat_kind,omitempty"`
	ModelPattern   string          `json:"model_pattern,omitempty"`
	Endpoints      []EndpointKind  `json:"endpoints,omitempty"`
	Modes          []ExecutionMode `json:"modes,omitempty"`
	SourceFormats  []Format        `json:"source_formats,omitempty"`
	TargetFormats  []Format        `json:"target_formats,omitempty"`
}

// MatchContext contains the low-cardinality request attributes used to select
// policies. Empty values act as wildcards for inventory and diagnostic queries.
type MatchContext struct {
	ProviderFamily string
	CompatKind     string
	Model          string
	Endpoint       EndpointKind
	Mode           ExecutionMode
	SourceFormat   Format
	TargetFormat   Format
}

// CostContract bounds the work and output growth permitted for a policy.
type CostContract struct {
	Complexity         string  `json:"complexity"`
	MaxExpansionBytes  int64   `json:"max_expansion_bytes"`
	MaxExpansionRatio  float64 `json:"max_expansion_ratio"`
	MayCopyLargeFields bool    `json:"may_copy_large_fields"`
}

// LifecycleMetadata records why a compatibility policy exists and when it can
// be reviewed or removed.
type LifecycleMetadata struct {
	IntroducedVersion        string `json:"introduced_version"`
	Fixture                  string `json:"fixture"`
	UpstreamEvidence         string `json:"upstream_evidence"`
	RetrySemantics           string `json:"retry_semantics"`
	ReviewDate               string `json:"review_date,omitempty"`
	UpstreamVersionCondition string `json:"upstream_version_condition,omitempty"`
}

// Policy is one independently owned compatibility rule.
type Policy struct {
	ID               string            `json:"id"`
	Owner            string            `json:"owner"`
	Match            MatchSpec         `json:"match"`
	Phase            Phase             `json:"phase"`
	Priority         int               `json:"priority"`
	Cost             CostContract      `json:"cost"`
	RemovalCondition string            `json:"removal_condition"`
	Lifecycle        LifecycleMetadata `json:"lifecycle"`
	MutatedFields    []string          `json:"mutated_fields,omitempty"`
	DowngradeIDs     []string          `json:"downgrade_ids,omitempty"`
	Apply            TransformFunc     `json:"-"`
}
