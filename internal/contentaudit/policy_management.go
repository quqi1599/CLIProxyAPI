package contentaudit

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PolicyDocument contains the active policy and its recent rollback points.
type PolicyDocument struct {
	Policy  Policy          `json:"policy"`
	History []PolicyVersion `json:"history"`
}

// CurrentPolicy returns the active policy and recent persisted versions.
func (s *Service) CurrentPolicy(ctx context.Context) (PolicyDocument, error) {
	state := s.state.Load()
	if state == nil || state.matcher == nil || state.store == nil {
		return PolicyDocument{}, fmt.Errorf("content audit policy is unavailable")
	}
	history, err := state.store.ListPolicyVersions(ctx, state.matcher.Version(), 30)
	if err != nil {
		return PolicyDocument{}, err
	}
	return PolicyDocument{Policy: state.matcher.Policy(), History: history}, nil
}

// ApplyPolicy validates, persists, versions, and atomically activates a managed policy.
func (s *Service) ApplyPolicy(ctx context.Context, policy Policy, reason, actor string) (PolicyDocument, error) {
	if s == nil {
		return PolicyDocument{}, fmt.Errorf("content audit service is unavailable")
	}
	reason = strings.TrimSpace(reason)
	if len([]rune(reason)) < 4 {
		return PolicyDocument{}, fmt.Errorf("a policy change reason of at least four characters is required")
	}
	s.policyMu.Lock()
	defer s.policyMu.Unlock()

	state := s.state.Load()
	if state == nil || state.store == nil || strings.TrimSpace(state.policyPath) == "" {
		return PolicyDocument{}, fmt.Errorf("content audit policy is unavailable")
	}
	policy.Version = "managed-" + time.Now().UTC().Format("20060102T150405.000000000Z")
	matcher, err := CompilePolicy(policy)
	if err != nil {
		return PolicyDocument{}, err
	}
	normalizedPolicy := matcher.Policy()
	policyYAML, err := yaml.Marshal(normalizedPolicy)
	if err != nil {
		return PolicyDocument{}, fmt.Errorf("serialize content audit policy: %w", err)
	}
	previousYAML, err := os.ReadFile(state.policyPath)
	if err != nil {
		return PolicyDocument{}, fmt.Errorf("read current content audit policy: %w", err)
	}
	if err = writePolicyFile(state.policyPath, policyYAML); err != nil {
		return PolicyDocument{}, err
	}
	if err = state.store.RecordPolicyVersion(ctx, normalizedPolicy.Version, policyYAML, reason, actor); err != nil {
		if restoreErr := writePolicyFile(state.policyPath, previousYAML); restoreErr != nil {
			return PolicyDocument{}, fmt.Errorf("%v; restore policy: %w", err, restoreErr)
		}
		return PolicyDocument{}, err
	}

	next := *state
	next.matcher = matcher
	next.initErr = nil
	s.state.Store(&next)
	return s.currentPolicyLocked(ctx, &next)
}

// RollbackPolicy activates a prior snapshot as a new managed version.
func (s *Service) RollbackPolicy(ctx context.Context, id int64, reason, actor string) (PolicyDocument, error) {
	state := s.state.Load()
	if state == nil || state.store == nil {
		return PolicyDocument{}, fmt.Errorf("content audit policy is unavailable")
	}
	version, body, err := state.store.GetPolicyVersion(ctx, id)
	if err != nil {
		return PolicyDocument{}, err
	}
	var policy Policy
	if err = yaml.Unmarshal(body, &policy); err != nil {
		return PolicyDocument{}, fmt.Errorf("parse stored content audit policy: %w", err)
	}
	return s.ApplyPolicy(ctx, policy, strings.TrimSpace(reason)+"; rollback from "+version.Version, actor)
}

func (s *Service) currentPolicyLocked(ctx context.Context, state *runtimeState) (PolicyDocument, error) {
	history, err := state.store.ListPolicyVersions(ctx, state.matcher.Version(), 30)
	if err != nil {
		return PolicyDocument{}, err
	}
	return PolicyDocument{Policy: state.matcher.Policy(), History: history}, nil
}

func writePolicyFile(path string, body []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create content audit policy directory: %w", err)
	}
	temporary, err := os.CreateTemp(directory, ".content-audit-policy-*.tmp")
	if err != nil {
		return fmt.Errorf("create content audit policy temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err = temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("secure content audit policy temporary file: %w", err)
	}
	if _, err = temporary.Write(body); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write content audit policy temporary file: %w", err)
	}
	if err = temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync content audit policy temporary file: %w", err)
	}
	if err = temporary.Close(); err != nil {
		return fmt.Errorf("close content audit policy temporary file: %w", err)
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("activate content audit policy: %w", err)
	}
	return nil
}

func ensurePolicyBaseline(state *runtimeState) error {
	policyYAML, err := yaml.Marshal(state.matcher.Policy())
	if err != nil {
		return fmt.Errorf("serialize content audit policy baseline: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return state.store.EnsurePolicyVersion(ctx, state.matcher.Version(), policyYAML)
}
