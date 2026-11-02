package localnote

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

func TestBusinessReadBackComparesApprovedContentAndRealStoredRow(t *testing.T) {
	args := []byte(`{"title":"approved","content":"body"}`)
	_, digest, _ := governance.CanonicalArguments(args)
	ctx := runtime.WithExecutionContext(context.Background(), runtime.ExecutionContext{TenantID: "t", RequestID: "r", ToolCallID: "op", SubjectID: "u", ArgsDigest: digest})
	w := &businessNotes{rows: map[string]Note{}}
	tool := Tool{Writer: w}
	result, err := tool.Call(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	if err = tool.VerifyBusinessResult(ctx, args, b); err != nil {
		t.Fatal(err)
	}
	for key, n := range w.rows {
		n.Result.Content = "changed externally"
		w.rows[key] = n
	}
	if err = tool.VerifyBusinessResult(ctx, args, b); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatalf("changed business effect accepted: %v", err)
	}
	w.rows = map[string]Note{}
	if err = tool.VerifyBusinessResult(ctx, args, b); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("missing effect accepted: %v", err)
	}
}
