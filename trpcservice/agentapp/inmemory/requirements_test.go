package inmemory

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/agentapp"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

func TestRejectedToolContractDoesNotPublishOrAdvanceApplication(t *testing.T) {
	ctx := context.Background()
	r := New()
	a, err := r.Create(ctx, agentapp.CreateInput{App: agentapp.AgentApp{TenantID: "t", AgentAppID: "app", AgentAppKey: "key", DisplayName: "app"}, ChangeMetadata: appMeta()})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := r.CreateDraft(ctx, agentapp.CreateDraftInput{TenantID: "t", AgentAppID: "app", ExpectedAppVersion: a.Version, Revision: agentapp.Revision{AgentKind: agentapp.AgentKindLLM, Instruction: "help", ModelProfileID: "m", ModelProfileVersion: 1, ToolRefs: []agentapp.VersionedRef{{ID: "create", Version: 1, Required: true}}, RuntimePolicy: map[string]any{"task_requirements": []taskflow.Requirement{{ID: "record", Description: "record", ToolID: "create", ToolVersion: 1}}}}, ChangeMetadata: appMeta()})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := r.Get(ctx, "t", "app")
	in := agentapp.PublishInput{TenantID: "t", AgentAppID: "app", Revision: draft.Revision, ExpectedAppVersion: before.Version, ExpectedDraftVersion: draft.DraftVersion, ChangeMetadata: appMeta()}
	if _, err = r.Publish(ctx, in); !errors.Is(err, agentapp.ErrInvalid) {
		t.Fatalf("missing validator did not block publication: %v", err)
	}
	after, _ := r.Get(ctx, "t", "app")
	stillDraft, _ := r.GetRevision(ctx, "t", "app", draft.Revision)
	if after.Version != before.Version || after.CurrentRevision != before.CurrentRevision || stillDraft.State != agentapp.RevisionDraft {
		t.Fatal("rejected publication mutated authority")
	}
	r.Requirements = func(context.Context, agentapp.Revision) error { return nil }
	if _, err = r.Publish(ctx, in); err != nil {
		t.Fatal(err)
	}
}
