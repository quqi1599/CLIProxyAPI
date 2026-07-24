package auth

import "strings"

const (
	// RoutingStrategyRoundRobin rotates across ready credentials.
	RoutingStrategyRoundRobin = "round-robin"
	// RoutingStrategyFillFirst burns the first ready credential before moving on.
	RoutingStrategyFillFirst = "fill-first"
	// RoutingStrategySequentialFill sticks to the current credential until it becomes unavailable.
	RoutingStrategySequentialFill = "sequential-fill"
	// RoutingStrategySpread weights all available credentials without strict priority buckets.
	RoutingStrategySpread = "spread"
)

// NormalizeRoutingStrategy canonicalizes supported routing strategy names and aliases.
func NormalizeRoutingStrategy(strategy string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(strategy)) {
	case "", RoutingStrategyRoundRobin, "roundrobin", "rr":
		return RoutingStrategyRoundRobin, true
	case RoutingStrategyFillFirst, "fillfirst", "ff":
		return RoutingStrategyFillFirst, true
	case RoutingStrategySequentialFill, "sequentialfill", "sf":
		return RoutingStrategySequentialFill, true
	case RoutingStrategySpread, "balanced", "even", "even-round-robin", "balanced-round-robin":
		return RoutingStrategySpread, true
	default:
		return "", false
	}
}

// SelectorForRoutingStrategy returns the built-in selector for the supplied strategy.
// Unknown values fall back to round-robin so startup and reload behavior stay safe.
func SelectorForRoutingStrategy(strategy string) Selector {
	normalized, ok := NormalizeRoutingStrategy(strategy)
	if !ok {
		normalized = RoutingStrategyRoundRobin
	}
	switch normalized {
	case RoutingStrategyFillFirst:
		return &FillFirstSelector{}
	case RoutingStrategySequentialFill:
		return &SequentialFillSelector{}
	case RoutingStrategySpread:
		return &SpreadSelector{}
	default:
		return &RoundRobinSelector{}
	}
}

func normalizeRoutingGroupKey(group string) string {
	return strings.ToLower(strings.TrimSpace(group))
}

// NormalizeRoutingGroupStrategies canonicalizes routing group strategy overrides.
// Empty group names and unsupported strategies are discarded.
func NormalizeRoutingGroupStrategies(overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(overrides))
	for group, strategy := range overrides {
		normalizedGroup := normalizeRoutingGroupKey(group)
		if normalizedGroup == "" {
			continue
		}
		normalizedStrategy, ok := NormalizeRoutingStrategy(strategy)
		if !ok {
			continue
		}
		out[normalizedGroup] = normalizedStrategy
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// NormalizeRoutingProviderStrategies canonicalizes provider strategy overrides.
// Empty provider names and unsupported strategies are discarded.
func NormalizeRoutingProviderStrategies(overrides map[string]string) map[string]string {
	if len(overrides) == 0 {
		return nil
	}
	out := make(map[string]string, len(overrides))
	for provider, strategy := range overrides {
		normalizedProvider := normalizeRoutingGroupKey(provider)
		if normalizedProvider == "" {
			continue
		}
		normalizedStrategy, ok := NormalizeRoutingStrategy(strategy)
		if !ok {
			continue
		}
		out[normalizedProvider] = normalizedStrategy
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
