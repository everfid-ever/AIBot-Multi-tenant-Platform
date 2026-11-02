package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/execution"
)

type readBackTool struct {
	*recoveringTool
	err   error
	reads int
}

func (t *readBackTool) VerifyBusinessResult(context.Context, []byte, []byte) error {
	t.reads++
	return t.err
}

func TestUnconfirmedBusinessReadBackCannotCommitSucceededAndReuseRechecks(t *testing.T) {
	store := newDurableMemory()
	base := &recoveringTool{policy: RecoveryIdempotentKey}
	g, ctx := durableGuard(t, base, store)
	inner := &readBackTool{recoveringTool: base, err: runtime.ErrNotFound}
	g.Inner = tenantBoundCallable{tenantID: "tenant", id: "safe", inner: inner}
	if _, err := g.Call(ctx, []byte(`{"a":1}`)); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal(err)
	}
	for _, r := range store.records {
		if r.State != execution.EffectUnknown || r.LastError != "business_verification_unconfirmed" {
			t.Fatal(r)
		}
	}
	if len(store.outputs) != 0 {
		t.Fatal("unverified result published")
	}
	inner.err = nil
	if _, err := g.Call(ctx, []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	calls := base.calls
	inner.err = runtime.ErrNotFound
	if _, err := g.Call(ctx, []byte(`{"a":1}`)); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal(err)
	}
	if base.calls != calls || inner.reads != 3 {
		t.Fatal("cached result bypassed read-back or repeated business call")
	}
}
