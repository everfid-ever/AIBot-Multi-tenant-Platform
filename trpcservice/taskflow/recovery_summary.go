package taskflow

import "time"

type StepProgress struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	ToolID         string `json:"tool_id,omitempty"`
	ToolVersion    int64  `json:"tool_version,omitempty"`
	ResultRef      string `json:"result_ref,omitempty"`
	EvidenceDigest string `json:"evidence_digest,omitempty"`
}
type BlockedStep struct {
	ID           string   `json:"id"`
	Reason       string   `json:"reason"`
	Dependencies []string `json:"dependencies,omitempty"`
}
type RecoverySummary struct {
	Failed                 []StepProgress `json:"failed"`
	NeedsFinalRepair       bool           `json:"needs_final_repair"`
	PlanVersion            int64          `json:"plan_version"`
	Fence                  uint64         `json:"fence"`
	Phase                  string         `json:"phase"`
	InputDigest            string         `json:"input_digest"`
	Completed              []StepProgress `json:"completed"`
	Active                 []StepProgress `json:"active"`
	Ready                  []string       `json:"ready"`
	Blocked                []BlockedStep  `json:"blocked"`
	UnplannedRequirements  []string       `json:"unplanned_requirements"`
	UnfinishedRequirements []string       `json:"unfinished_requirements"`
	UnknownEffects         []string       `json:"unknown_effects"`
	NextAction             string         `json:"next_action"`
	FailureReason          string         `json:"failure_reason,omitempty"`
	Attempts               int            `json:"attempts"`
	NoProgress             int            `json:"no_progress"`
	RemainingAttempts      int            `json:"remaining_attempts"`
	Deadline               time.Time      `json:"deadline,omitempty"`
}

// SummarizeRecovery is a read-only projection, not another task authority or
// permission to execute. Runtime callers must Recover/verify evidence first.
// Operations callers expose it as observed checkpoint metadata, never as a
// fresh assertion that a provider effect has completed.
func SummarizeRecovery(p Plan, now time.Time) RecoverySummary {
	s := RecoverySummary{PlanVersion: p.Version, Fence: p.Fence, Phase: p.Phase, InputDigest: p.InputDigest, Completed: []StepProgress{}, Failed: []StepProgress{}, NeedsFinalRepair: p.CompletionIssue != "", Active: []StepProgress{}, Ready: []string{}, Blocked: []BlockedStep{}, UnplannedRequirements: []string{}, UnfinishedRequirements: []string{}, UnknownEffects: []string{}, FailureReason: p.FailureReason, Attempts: p.Attempts, NoProgress: p.NoProgress, RemainingAttempts: p.Contract.MaxAttempts - p.Attempts}
	if s.RemainingAttempts < 0 {
		s.RemainingAttempts = 0
	}
	if !p.StartedAt.IsZero() && p.Contract.MaxAgeSeconds > 0 {
		s.Deadline = p.StartedAt.Add(time.Duration(p.Contract.MaxAgeSeconds) * time.Second)
	}
	states := map[string]string{}
	mapped := map[string]bool{}
	unfinished := map[string]bool{}
	for _, st := range p.Steps {
		states[st.ID] = st.State
		for _, id := range st.RequirementIDs {
			mapped[id] = true
			if st.State != "succeeded" {
				unfinished[id] = true
			}
		}
		progress := StepProgress{ID: st.ID, Kind: st.Kind, State: st.State, ToolID: st.ToolID, ToolVersion: st.ToolVersion, ResultRef: st.ResultRef, EvidenceDigest: st.EvidenceDigest}
		if st.State == "succeeded" {
			s.Completed = append(s.Completed, progress)
		}
		if st.State == "failed" {
			s.Failed = append(s.Failed, progress)
		}
		if st.State == "running" || st.State == "effect_unknown" {
			s.Active = append(s.Active, progress)
		}
		if st.State == "effect_unknown" {
			s.UnknownEffects = append(s.UnknownEffects, st.ID)
		}
	}
	if p.Acceptance != nil {
		for _, r := range p.Acceptance.Requirements {
			if !mapped[r.ID] {
				s.UnplannedRequirements = append(s.UnplannedRequirements, r.ID)
			}
			if !mapped[r.ID] || unfinished[r.ID] {
				s.UnfinishedRequirements = append(s.UnfinishedRequirements, r.ID)
			}
		}
	}
	expired := !s.Deadline.IsZero() && !now.Before(s.Deadline)
	for _, st := range p.Steps {
		if st.State != "pending" {
			continue
		}
		blocked := BlockedStep{ID: st.ID, Dependencies: []string{}}
		for _, dep := range st.DependsOn {
			if states[dep] != "succeeded" {
				blocked.Dependencies = append(blocked.Dependencies, dep)
			}
		}
		switch {
		case p.Phase != "open":
			blocked.Reason = "task_not_open"
		case expired:
			blocked.Reason = "deadline_exceeded"
		case len(s.Failed) > 0:
			blocked.Reason = "failed_step_must_resolve"
		case len(blocked.Dependencies) > 0:
			blocked.Reason = "dependencies_incomplete"
		case len(s.Active) > 0:
			blocked.Reason = "active_step_must_finish"
		default:
			s.Ready = append(s.Ready, st.ID)
			continue
		}
		s.Blocked = append(s.Blocked, blocked)
	}
	switch {
	case p.Phase == "failed":
		s.NextAction = "stop_failed"
	case p.Phase == "sealed":
		s.NextAction = "resume_frozen_result"
	case expired:
		s.NextAction = "stop_deadline"
	case len(s.UnknownEffects) > 0:
		s.NextAction = "reconcile_unknown_effect"
	case len(s.Failed) > 0:
		s.NextAction = "inspect_failed_step"
	case len(s.Active) > 0:
		s.NextAction = "resume_active_step"
	case len(s.UnplannedRequirements) > 0 || len(p.Steps) == 0:
		s.NextAction = "plan_missing_obligations"
	case len(s.Ready) > 0:
		s.NextAction = "start_ready_step"
	case len(s.Blocked) > 0:
		s.NextAction = "inspect_blockers"
	case s.NeedsFinalRepair:
		s.NextAction = "repair_final_deliverable"
	default:
		s.NextAction = "validate_final_deliverable"
	}
	return s
}
