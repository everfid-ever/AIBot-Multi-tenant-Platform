package taskflow_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/execution"
)

func TestAcceptanceRejectsOmittedRequirementAndWrongFinalWithoutLosingProgress(t *testing.T) {
	ctx := context.Background()
	s, store, _ := session(t)
	a := taskflow.Acceptance{Version: 1, InputDigest: taskflow.Digest([]byte("original input")), Objective: "Compare and summarize", Requirements: []taskflow.Requirement{{ID: "compare", Description: "comparison", OutputContains: []string{"comparison"}}, {ID: "summary", Description: "summary", OutputContains: []string{"summary"}}}}
	p, err := s.BindAcceptance(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Define(ctx, p.Version, []taskflow.Step{{ID: "work", Title: "work", Criterion: "document comparison", Kind: "analysis", RequirementIDs: []string{"compare"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Start(ctx, p.Version, "work")
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.CompleteAnalysis(ctx, p.Version, "work", "comparison evidence")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateFinal(ctx, []byte("comparison summary")); !errors.Is(err, taskflow.ErrIncomplete) {
		t.Fatalf("omitted requirement accepted: %v", err)
	}
	p, _ = store.Get(ctx, s.Scope.Key)
	if !strings.Contains(p.CompletionIssue, "summary") {
		t.Fatal("missing durable repair reason")
	}
	p, err = s.Define(ctx, p.Version, []taskflow.Step{{ID: "sum", Title: "sum", Criterion: "saved summary", Kind: "analysis", RequirementIDs: []string{"summary"}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Start(ctx, p.Version, "sum")
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.CompleteAnalysis(ctx, p.Version, "sum", "summary evidence")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ValidateFinal(ctx, []byte("comparison only")); !errors.Is(err, taskflow.ErrIncomplete) {
		t.Fatalf("missing final deliverable accepted: %v", err)
	}
	// Adopt on another node. Weaker conditions cannot replace the original ones.
	s.Scope.Fence++
	if _, err = store.Open(ctx, s.Scope, a.InputDigest, 1); err != nil {
		t.Fatal(err)
	}
	weaker := a
	weaker.Requirements = nil
	if _, err = s.BindAcceptance(ctx, weaker); !errors.Is(err, runtime.ErrIdempotencyCollision) {
		t.Fatalf("contract weakened: %v", err)
	}
	if err = s.Seal(ctx, "result"); !errors.Is(err, taskflow.ErrEvidenceRequired) {
		t.Fatalf("unverified seal: %v", err)
	}
	if err = s.SealFinal(ctx, "result", []byte("comparison and summary")); err != nil {
		t.Fatal(err)
	}
	if err = s.SealFinal(ctx, "result", []byte("comparison and summary")); err != nil {
		t.Fatal(err)
	}
	if err = s.SealFinal(ctx, "result", []byte("different comparison and summary")); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatalf("sealed output replaced: %v", err)
	}
}

func TestFinalAnswerMustReferenceVerifiedReceiptValue(t *testing.T) {
	ctx := context.Background()
	s, _, r := session(t)
	p, err := s.BindAcceptance(ctx, taskflow.Acceptance{Version: 1, InputDigest: taskflow.Digest([]byte("original input")), Objective: "Query and report record ID", Requirements: []taskflow.Requirement{{ID: "record", Description: "report real record", ToolID: "query", ToolVersion: 1, EvidencePath: []string{"id"}}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Define(ctx, p.Version, []taskflow.Step{{ID: "query", Title: "query", Criterion: "has id", Kind: "tool", ToolID: "query", ToolVersion: 1, Check: &taskflow.ResultCheck{Path: []string{"id"}, NonEmpty: true}, RequirementIDs: []string{"record"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, p.Version, "query"); err != nil {
		t.Fatal(err)
	}
	args := []byte(`{"query":"record"}`)
	r.r = execution.Record{Key: execution.Key{TenantID: "t", RequestID: "r", ToolCallID: "op"}, ToolID: "query", ToolVersion: 1, ArgsDigest: taskflow.Digest(args), State: execution.Running, Policy: "replay_safe"}
	if err = s.BeforeTool(ctx, r.r, args, &taskflow.ResultCheck{Path: []string{"id"}, NonEmpty: true}); err != nil {
		t.Fatal(err)
	}
	r.b = []byte(`{"id":"R-actual","status":"found"}`)
	r.r.State = execution.Succeeded
	r.r.ResultRef = "tool-result://sha256/" + taskflow.Digest(r.b)
	if _, err = s.ValidateFinal(ctx, []byte("Record R-invented was found")); !errors.Is(err, taskflow.ErrIncomplete) {
		t.Fatalf("invented receipt value accepted: %v", err)
	}
	if _, err = s.ValidateFinal(ctx, []byte("Record R-actual was found")); err != nil {
		t.Fatal(err)
	}
	// A changed result may not be legitimized merely by repeating the new text.
	r.b = []byte(`{"id":"R-tampered"}`)
	if _, err = s.ValidateFinal(ctx, []byte("Record R-tampered was found")); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatalf("tampered receipt accepted: %v", err)
	}
}

func TestToolRequirementCannotBeCoveredByAnalysisOrAnotherTool(t *testing.T) {
	ctx := context.Background()
	s, _, _ := session(t)
	p, err := s.BindAcceptance(ctx, taskflow.Acceptance{Version: 1, InputDigest: taskflow.Digest([]byte("original input")), Objective: "Create record", Requirements: []taskflow.Requirement{{ID: "created", Description: "created record", ToolID: "create", ToolVersion: 2, EvidencePath: []string{"id"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []taskflow.Step{
		{ID: "fake", Title: "fake", Criterion: "done", Kind: "analysis", RequirementIDs: []string{"created"}},
		{ID: "fake", Title: "fake", Criterion: "done", Kind: "tool", ToolID: "create", ToolVersion: 1, Check: &taskflow.ResultCheck{NonEmpty: true}, RequirementIDs: []string{"created"}},
	} {
		if _, err = s.Define(ctx, p.Version, []taskflow.Step{st}); !errors.Is(err, taskflow.ErrEvidenceRequired) {
			t.Fatalf("false requirement coverage: %v", err)
		}
	}
}
