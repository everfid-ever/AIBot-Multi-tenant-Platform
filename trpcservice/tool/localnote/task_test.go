package localnote

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	taskmemory "github.com/liuzengh/trpc-agent-service/trpcservice/taskflow/inmemory"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/execution"
)

func TestExplicitNoteCommandFreezesExactArgumentsAndRejectsChangedWrite(t *testing.T) {
	args := []byte(`{"title":"approved","content":"original"}`)
	requirements, err := TaskRequirements("/note " + string(args))
	if err != nil || len(requirements) != 1 {
		t.Fatal(requirements, err)
	}
	canonical, digest, _ := governance.CanonicalArguments(args)
	if requirements[0].ArgsDigest != digest {
		t.Fatal("argument contract missing")
	}
	ctx := context.Background()
	store := taskmemory.New()
	scope := taskflow.Scope{Key: taskflow.Key{TenantID: "t", RequestID: "r"}, Fence: 1}
	input := taskflow.Digest([]byte("original input"))
	_, _ = store.Open(ctx, scope, input, 1)
	s := taskflow.Session{Store: store, Scope: scope}
	p, err := s.BindAcceptance(ctx, taskflow.Acceptance{Version: 1, InputDigest: input, Objective: "original note", Requirements: requirements})
	if err != nil {
		t.Fatal(err)
	}
	p, err = s.Define(ctx, p.Version, []taskflow.Step{{ID: "write", Title: "write", Criterion: "created", Kind: "tool", ToolID: ID, ToolVersion: Version, Check: (Tool{}).TaskEvidenceCheck(), RequirementIDs: []string{requirements[0].ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Start(ctx, p.Version, "write"); err != nil {
		t.Fatal(err)
	}
	changed := []byte(`{"title":"approved","content":"changed"}`)
	_, wrongDigest, _ := governance.CanonicalArguments(changed)
	r := execution.Record{Key: execution.Key{TenantID: "t", RequestID: "r", ToolCallID: "op"}, ToolID: ID, ToolVersion: Version, Policy: "idempotent_key", ArgsDigest: wrongDigest}
	if err = s.BeforeTool(ctx, r, changed, (Tool{}).TaskEvidenceCheck()); !errors.Is(err, taskflow.ErrEvidenceRejected) {
		t.Fatalf("changed business input accepted: %v", err)
	}
	r.ArgsDigest = digest
	if err = s.BeforeTool(ctx, r, canonical, (Tool{}).TaskEvidenceCheck()); err != nil {
		t.Fatal(err)
	}
}

func TestNoteCommandsDoNotInferWritesFromConversationAndRejectMalformedInput(t *testing.T) {
	for _, text := range []string{"hello", "请介绍笔记功能", "/noteworthy"} {
		if r, e := TaskRequirements(text); e != nil || len(r) != 0 {
			t.Fatal(text, r, e)
		}
	}
	for _, text := range []string{"/note", "/note null", `/note {"title":"x","content":"x","extra":true}`, `/note {"title":"x","title":"other","content":"x"}`} {
		if _, err := TaskRequirements(text); !errors.Is(err, runtime.ErrInvalidEnvelope) {
			t.Fatal(text, err)
		}
	}
}
