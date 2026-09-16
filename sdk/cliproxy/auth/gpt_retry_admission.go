package auth

import "time"

const gptRetryAdmissionWaitBudget = 180 * time.Second

// This budget only gates a new attempt. It never changes a connection deadline
// or cancels an upstream that has already started.
func (t *requestAttemptTrace) gptRetryAdmissionExhausted() bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.gptRoute && t.gptFirstEventWait >= gptRetryAdmissionWaitBudget
}

// Prefer routes that have not recently degraded, while retaining the full pool
// if all alternatives are degraded. Expired samples restore probe eligibility.
func (m *Manager) preferHealthyGPTRoutes(auths []*Auth, model string, now time.Time) ([]*Auth, int) {
	if m.gptRetryPressure == nil || len(auths) < 2 {
		return auths, 0
	}
	c := m.gptRetryPressure
	c.mu.Lock()
	state := c.models[canonicalModelKey(model)]
	degraded := c.degradedRoutesLocked(state, now)
	c.mu.Unlock()
	if len(degraded) == 0 {
		return auths, 0
	}
	healthy := make([]*Auth, 0, len(auths))
	for _, auth := range auths {
		if _, failed := degraded[routingChannelBaseKey(auth)]; !failed {
			healthy = append(healthy, auth)
		}
	}
	if len(healthy) == 0 {
		return auths, 0
	}
	return healthy, len(auths) - len(healthy)
}
