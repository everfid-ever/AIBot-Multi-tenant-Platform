package agentapp

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

func TestPublicationRequiresExactMandatoryToolAndCatalogValidation(t *testing.T) {
	r := Revision{TenantID: "t", AgentAppID: "app", Revision: 1, DraftVersion: 1, SchemaVersion: 1, AgentKind: AgentKindLLM, Instruction: "help", ModelProfileID: "m", ModelProfileVersion: 1, RuntimePolicy: map[string]any{"task_requirements": []taskflow.Requirement{{ID: "record", Description: "record", ToolID: "create", ToolVersion: 2}}}}
	for _, refs := range [][]VersionedRef{nil, {{ID: "create", Version: 1, Required: true}}, {{ID: "create", Version: 2}}} {
		r.ToolRefs = refs
		if !errors.Is(r.ValidateDraft(), ErrInvalid) {
			t.Fatalf("bad refs accepted: %+v", refs)
		}
	}
	r.ToolRefs = []VersionedRef{{ID: "create", Version: 2, Required: true}}
	if !errors.Is(ValidatePublication(context.Background(), r, nil), ErrInvalid) {
		t.Fatal("missing catalog validation was allowed")
	}
	calls := 0
	if err := ValidatePublication(context.Background(), r, func(_ context.Context, got Revision) error {
		calls++
		if got.TenantID != "t" {
			t.Fatal("lost tenant scope")
		}
		return nil
	}); err != nil || calls != 1 {
		t.Fatal(err, calls)
	}
	r.RuntimePolicy["task_requirements"] = []any{map[string]any{"id": "record", "description": "record", "unexpected": true}}
	if !errors.Is(r.ValidateDraft(), ErrInvalid) {
		t.Fatal("unknown configuration field accepted")
	}
}
