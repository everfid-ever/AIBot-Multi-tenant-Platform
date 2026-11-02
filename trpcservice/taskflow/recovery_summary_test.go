package taskflow_test

import (
	"testing"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

func TestRecoverySummarySeparatesDependencyReadinessAndUnknownEffects(t *testing.T) {
	now := time.Now()
	p := taskflow.Plan{Phase: "open", Version: 9, Contract: taskflow.DefaultContract(), StartedAt: now, Attempts: 1, Acceptance: &taskflow.Acceptance{Requirements: []taskflow.Requirement{{ID: "known"}, {ID: "missing"}}}, Steps: []taskflow.Step{
		{ID: "done", Kind: "tool", State: "succeeded", ResultRef: "result", EvidenceDigest: "digest", RequirementIDs: []string{"known"}},
		{ID: "next", State: "pending", DependsOn: []string{"done"}},
		{ID: "later", State: "pending", DependsOn: []string{"next"}},
	}}
	s := taskflow.SummarizeRecovery(p, now)
	if len(s.Completed) != 1 || len(s.Ready) != 1 || s.Ready[0] != "next" || len(s.Blocked) != 1 || s.Blocked[0].Reason != "dependencies_incomplete" || len(s.UnplannedRequirements) != 1 || s.UnplannedRequirements[0] != "missing" {
		t.Fatalf("wrong summary: %+v", s)
	}
	p.Steps = append(p.Steps, taskflow.Step{ID: "uncertain", Kind: "tool", State: "effect_unknown"})
	s = taskflow.SummarizeRecovery(p, now)
	if len(s.Ready) != 0 || len(s.UnknownEffects) != 1 || s.NextAction != "reconcile_unknown_effect" {
		t.Fatalf("unknown effect authorized new work: %+v", s)
	}
	p.Phase = "failed"
	s = taskflow.SummarizeRecovery(p, now)
	if s.NextAction != "stop_failed" {
		t.Fatal(s)
	}
}

func TestRecoverySummaryKeepsLastActiveAttemptUsableAndHonorsDeadline(t *testing.T) {
	now := time.Now()
	p := taskflow.Plan{Phase: "open", Contract: taskflow.DefaultContract(), StartedAt: now, Attempts: 8, Steps: []taskflow.Step{{ID: "work", State: "pending"}}}
	s := taskflow.SummarizeRecovery(p, now)
	if s.RemainingAttempts != 0 || len(s.Ready) != 1 {
		t.Fatalf("last allowed attempt was blocked: %+v", s)
	}
	s = taskflow.SummarizeRecovery(p, now.Add(time.Hour))
	if len(s.Ready) != 0 || s.NextAction != "stop_deadline" {
		t.Fatal(s)
	}
	p.Phase = "sealed"
	s = taskflow.SummarizeRecovery(p, now)
	if s.NextAction != "resume_frozen_result" || len(s.Ready) != 0 {
		t.Fatal(s)
	}
}
