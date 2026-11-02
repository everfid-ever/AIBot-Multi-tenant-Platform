package taskflow

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FinalEvidenceDescriptor pins adapter-owned output references to the operation
// before it executes. It does not create obligations for unrelated chat tasks.
type FinalEvidenceDescriptor interface{ TaskFinalEvidenceChecks() []*ResultCheck }

// VerifyFinalEvidenceReceipt rejects missing/non-scalar business references
// before a durable tool result can be published as succeeded.
func VerifyFinalEvidenceReceipt(checks []*ResultCheck, content []byte) error {
	if !validFinalEvidence(checks) {
		return ErrEvidenceRejected
	}
	for _, check := range checks {
		if _, err := scalarAt(content, check.Path); err != nil {
			return err
		}
	}
	return nil
}

func validFinalEvidence(checks []*ResultCheck) bool {
	if len(checks) > 8 {
		return false
	}
	for _, c := range checks {
		if c == nil || !c.valid() || !c.NonEmpty {
			return false
		}
	}
	return true
}

func scalarAt(content []byte, path []string) (string, error) {
	v, err := decodeJSON(content)
	if err != nil {
		return "", ErrEvidenceRejected
	}
	for _, key := range path {
		object, ok := v.(map[string]any)
		if !ok {
			return "", ErrEvidenceRejected
		}
		v = object[key]
	}
	var text string
	switch x := v.(type) {
	case string:
		text = x
	case json.Number:
		text = x.String()
	}
	if strings.TrimSpace(text) == "" {
		return "", ErrEvidenceRejected
	}
	return text, nil
}

func (s *Session) validateToolFinalEvidence(ctx context.Context, p Plan, content []byte) error {
	for _, st := range p.Steps {
		if len(st.TrustedOutputs) == 0 {
			continue
		}
		if st.Kind != "tool" || !validFinalEvidence(st.TrustedOutputs) {
			return ErrEvidenceRejected
		}
		_, b, err := s.verifyTool(ctx, st, p.Contract.RequireToolChecks)
		if err != nil {
			return err
		}
		for _, check := range st.TrustedOutputs {
			text, err := scalarAt(b, check.Path)
			if err != nil {
				return err
			}
			if !containsBusinessReference(string(content), text) {
				return fmt.Errorf("final answer missing business reference for step %s: %w", st.ID, ErrIncomplete)
			}
		}
	}
	return nil
}

// Business identifiers require ASCII token boundaries, so note_abc_extra does
// not count as a reference to note_abc. Chinese prose can abut an ASCII ID.
func containsBusinessReference(answer, reference string) bool {
	id := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-'
	}
	for offset := 0; offset <= len(answer)-len(reference); {
		i := strings.Index(answer[offset:], reference)
		if i < 0 {
			return false
		}
		i += offset
		end := i + len(reference)
		left := i == 0 || !id(reference[0]) || !id(answer[i-1])
		right := end == len(answer) || !id(reference[len(reference)-1]) || !id(answer[end])
		if left && right {
			return true
		}
		offset = i + 1
	}
	return false
}
