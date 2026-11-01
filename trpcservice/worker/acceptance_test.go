package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	taskmemory "github.com/liuzengh/trpc-agent-service/trpcservice/taskflow/inmemory"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func TestTaskAcceptanceFrozenAcrossTakeoverAndConfigurationChange(t *testing.T) {
	ctx := context.Background()
	store := taskmemory.New()
	scope := taskflow.Scope{Key: taskflow.Key{TenantID: "t", RequestID: "r"}, Fence: 1}
	digest := taskflow.Digest([]byte("original"))
	if _, err := store.Open(ctx, scope, digest, 1); err != nil {
		t.Fatal(err)
	}
	task := &taskflow.Session{Store: store, Scope: scope}
	calls := 0
	w := RunnerExecutor{TaskRequirements: func(context.Context, runtime.ExecutionEnvelope, model.Message) ([]taskflow.Requirement, error) {
		calls++
		return []taskflow.Requirement{{ID: "dynamic", Description: "original dynamic requirement"}}, nil
	}}
	policy := map[string]any{"task_requirements": []taskflow.Requirement{{ID: "required", Description: "original configured requirement", OutputContains: []string{"proof"}}}}
	msg := model.NewUserMessage("original goal")
	if err := w.bindTaskAcceptance(ctx, task, runtime.ExecutionEnvelope{}, msg, policy); err != nil {
		t.Fatal(err)
	}
	scope.Fence = 2
	if _, err := store.Open(ctx, scope, digest, 1); err != nil {
		t.Fatal(err)
	}
	task.Scope = scope
	if err := w.bindTaskAcceptance(ctx, task, runtime.ExecutionEnvelope{}, model.NewUserMessage("changed decoder"), map[string]any{"task_requirements": nil}); err != nil {
		t.Fatal(err)
	}
	p, _ := store.Get(ctx, scope.Key)
	if calls != 1 || p.Acceptance.Objective != "original goal" || len(p.Acceptance.Requirements) != 2 || p.Acceptance.Requirements[0].OutputContains[0] != "proof" {
		t.Fatalf("frozen contract replaced: %+v calls=%d", p.Acceptance, calls)
	}
}

func TestTaskAcceptanceRejectsMalformedPublishedConditions(t *testing.T) {
	for _, configured := range []any{nil, "not an array", []any{map[string]any{"id": "x", "description": "x", "unexpected": true}}, []taskflow.Requirement{{ID: "x", Description: "x", EvidencePath: []string{"id"}}}} {
		store := taskmemory.New()
		scope := taskflow.Scope{Key: taskflow.Key{TenantID: "t", RequestID: "r"}, Fence: 1}
		_, _ = store.Open(context.Background(), scope, taskflow.Digest([]byte("original")), 1)
		w := RunnerExecutor{}
		err := w.bindTaskAcceptance(context.Background(), &taskflow.Session{Store: store, Scope: scope}, runtime.ExecutionEnvelope{}, model.NewUserMessage("goal"), map[string]any{"task_requirements": configured})
		if !errors.Is(err, runtime.ErrInvalidEnvelope) {
			t.Fatalf("invalid conditions %v accepted: %v", configured, err)
		}
	}
}
