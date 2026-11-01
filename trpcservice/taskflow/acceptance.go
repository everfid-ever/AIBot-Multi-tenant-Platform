package taskflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

// Requirement is supplied by trusted application code, never checkpoint arguments.
// Literal and receipt checks are deterministic constraints, not semantic graders.
type Requirement struct {
	ID             string   `json:"id"`
	Description    string   `json:"description"`
	ToolID         string   `json:"tool_id,omitempty"`
	ToolVersion    int64    `json:"tool_version,omitempty"`
	OutputContains []string `json:"output_contains,omitempty"`
	EvidencePath   []string `json:"evidence_path,omitempty"`
}

type Acceptance struct {
	Version      int           `json:"version"`
	InputDigest  string        `json:"input_digest"`
	Objective    string        `json:"objective"`
	Requirements []Requirement `json:"requirements"`
}

func (a Acceptance) Validate() error {
	if a.Version != 1 || len(a.InputDigest) != 64 || strings.TrimSpace(a.Objective) == "" || len(a.Objective) > 64<<10 || len(a.Requirements) > 32 {
		return runtime.ErrInvalidEnvelope
	}
	seen := map[string]bool{}
	for _, r := range a.Requirements {
		if !validID(r.ID) || seen[r.ID] || strings.TrimSpace(r.Description) == "" || len(r.Description) > 1024 || len(r.ToolID) > 256 || (r.ToolID == "") != (r.ToolVersion == 0) || r.ToolVersion < 0 || len(r.OutputContains) > 16 || len(r.EvidencePath) > 8 || (len(r.EvidencePath) > 0 && r.ToolID == "") {
			return runtime.ErrInvalidEnvelope
		}
		seen[r.ID] = true
		for _, v := range r.OutputContains {
			if strings.TrimSpace(v) == "" || len(v) > 4096 {
				return runtime.ErrInvalidEnvelope
			}
		}
		for _, v := range r.EvidencePath {
			if v == "" || len(v) > 128 {
				return runtime.ErrInvalidEnvelope
			}
		}
	}
	return nil
}

// BindAcceptance freezes the original objective before planning. An adopted
// legacy plan must not acquire new obligations retroactively.
func (s *Session) BindAcceptance(ctx context.Context, a Acceptance) (Plan, error) {
	p, err := s.load(ctx)
	if err != nil {
		return p, err
	}
	if err = a.Validate(); err != nil {
		return p, err
	}
	if a.InputDigest != p.InputDigest {
		return p, runtime.ErrVersionMismatch
	}
	if p.Acceptance != nil {
		x, _ := json.Marshal(p.Acceptance)
		y, _ := json.Marshal(a)
		if string(x) != string(y) {
			return p, runtime.ErrIdempotencyCollision
		}
		return p, nil
	}
	if p.Phase != "open" || p.Attempts != 0 || len(p.Steps) != 0 {
		return p, runtime.ErrVersionConflict
	}
	// Copy caller-owned slices before the store receives the contract.
	b, _ := json.Marshal(a)
	var frozen Acceptance
	_ = json.Unmarshal(b, &frozen)
	p.Acceptance = &frozen
	return s.save(ctx, p)
}

func validateStepRequirements(a *Acceptance, st Step) error {
	seen := map[string]bool{}
	for _, id := range st.RequirementIDs {
		if a == nil || seen[id] {
			return runtime.ErrInvalidEnvelope
		}
		seen[id] = true
		found := false
		for _, r := range a.Requirements {
			if r.ID == id {
				found = true
				if r.ToolID != "" && (st.Kind != "tool" || st.ToolID != r.ToolID || st.ToolVersion != r.ToolVersion) {
					return ErrEvidenceRequired
				}
			}
		}
		if !found {
			return runtime.ErrInvalidEnvelope
		}
	}
	return nil
}

func validateCoverage(p Plan) error {
	if p.Acceptance == nil {
		return nil
	}
	if err := p.Acceptance.Validate(); err != nil {
		return err
	}
	if p.Acceptance.InputDigest != p.InputDigest {
		return runtime.ErrVersionMismatch
	}
	covered := map[string]bool{}
	for _, st := range p.Steps {
		if err := validateStepRequirements(p.Acceptance, st); err != nil {
			return err
		}
		for _, id := range st.RequirementIDs {
			covered[id] = true
		}
	}
	for _, r := range p.Acceptance.Requirements {
		if !covered[r.ID] {
			return fmt.Errorf("missing required deliverable %s: %w", r.ID, ErrIncomplete)
		}
	}
	return nil
}

// ValidateFinal rechecks authoritative receipts, coverage and the actual rendered
// output before candidate persistence and again before recovery/terminal commit.
func (s *Session) ValidateFinal(ctx context.Context, content []byte) (p Plan, err error) {
	defer func() {
		if err != nil && errors.Is(err, ErrIncomplete) && p.Phase == "open" && p.Acceptance != nil && p.CompletionIssue != err.Error() {
			p.CompletionIssue = err.Error()
			if len(p.CompletionIssue) > 1024 {
				p.CompletionIssue = p.CompletionIssue[:1024]
			}
			saved, saveErr := s.save(ctx, p)
			if saveErr != nil {
				err = saveErr
			} else {
				p = saved
			}
		}
	}()
	p, err = s.ValidateCompletion(ctx)
	if err != nil {
		return p, err
	}
	if p.Acceptance == nil {
		return p, nil
	}
	if strings.TrimSpace(string(content)) == "" {
		return p, fmt.Errorf("empty final deliverable: %w", ErrIncomplete)
	}
	if p.Phase == "sealed" && p.FinalDigest != Digest(content) {
		return p, runtime.ErrVersionMismatch
	}
	for _, r := range p.Acceptance.Requirements {
		for _, text := range r.OutputContains {
			if !strings.Contains(string(content), text) {
				return p, fmt.Errorf("final answer missing required content for %s: %w", r.ID, ErrIncomplete)
			}
		}
		if len(r.EvidencePath) == 0 {
			continue
		}
		matched := false
		for _, st := range p.Steps {
			if !containsID(st.RequirementIDs, r.ID) {
				continue
			}
			_, b, e := s.verifyTool(ctx, st, p.Contract.RequireToolChecks)
			if e != nil {
				return p, e
			}
			v, e := decodeJSON(b)
			if e != nil {
				return p, ErrEvidenceRejected
			}
			for _, path := range r.EvidencePath {
				obj, ok := v.(map[string]any)
				if !ok {
					v = nil
					break
				}
				v = obj[path]
			}
			var text string
			switch x := v.(type) {
			case string:
				text = x
			case json.Number:
				text = x.String()
			}
			if strings.TrimSpace(text) != "" && strings.Contains(string(content), text) {
				matched = true
			}
		}
		if !matched {
			return p, fmt.Errorf("final answer lacks verified receipt value for %s: %w", r.ID, ErrIncomplete)
		}
	}
	return p, nil
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
