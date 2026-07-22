package compat

// Report is a payload-free snapshot of the active compatibility policy bundle.
type Report struct {
	Policies []ReportPolicy `json:"policies"`
}

// ReportPolicy contains only controlled policy metadata. It deliberately omits
// the transform function and all request payload data.
type ReportPolicy struct {
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
}

// Report returns an owned, payload-free policy inventory.
func (registry *Registry) Report() Report {
	policies := registry.Policies()
	report := Report{Policies: make([]ReportPolicy, len(policies))}
	for i := range policies {
		report.Policies[i] = ReportPolicy{
			ID:               policies[i].ID,
			Owner:            policies[i].Owner,
			Match:            policies[i].Match,
			Phase:            policies[i].Phase,
			Priority:         policies[i].Priority,
			Cost:             policies[i].Cost,
			RemovalCondition: policies[i].RemovalCondition,
			Lifecycle:        policies[i].Lifecycle,
			MutatedFields:    policies[i].MutatedFields,
			DowngradeIDs:     policies[i].DowngradeIDs,
		}
	}
	return report
}
