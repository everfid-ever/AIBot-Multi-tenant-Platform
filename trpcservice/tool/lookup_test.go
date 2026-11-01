package tool

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/reliability/inflight"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	execution "github.com/liuzengh/trpc-agent-service/trpcservice/tool/execution"
)

type lookupTool struct {
	recoveringTool
	state   RecoveryState
	result  any
	err     error
	lookups int
	keys    []string
	effects map[string]bool
	bad     bool
}

func (*lookupTool) RecoveryLookupEnabled() bool { return true }
func (t *lookupTool) LookupRecovery(_ context.Context, key string) (RecoveryResult, error) {
	t.lookups++
	t.keys = append(t.keys, key)
	return RecoveryResult{State: t.state, Result: t.result}, t.err
}
func (t *lookupTool) Call(ctx context.Context, b []byte) (any, error) {
	value, err := t.recoveringTool.Call(ctx, b)
	k, _ := t.RecoveryKey(ctx)
	if t.effects == nil {
		t.effects = map[string]bool{}
	}
	t.effects[k] = true
	t.keys = append(t.keys, k)
	if t.bad {
		return "business rejected", nil
	}
	return value, err
}

type crashToolResult struct{}

func (crashToolResult) Wait(_ context.Context, p inflight.Point) error {
	if p == inflight.PointToolEffectBeforeCommit {
		return runtime.ErrLeaseLost
	}
	return nil
}

func TestLookupStatesDoNotBlindlyReplayAndKeepBusinessKey(t *testing.T) {
	for _, state := range []RecoveryState{RecoveryCompleted, RecoveryNotFound, RecoveryPending, RecoveryFailed, RecoveryUnknown, "invalid"} {
		t.Run(string(state), func(t *testing.T) {
			store := newDurableMemory()
			inner := &lookupTool{recoveringTool: recoveringTool{policy: RecoveryIdempotentKey}, state: state, result: "ok"}
			guard, ctx := durableGuard(t, &inner.recoveringTool, store)
			guard.Inner = tenantBoundCallable{tenantID: "tenant", id: "safe", inner: inner}
			if _, err := guard.Call(inflight.WithBarrier(ctx, crashToolResult{}), []byte(`{"a":1}`)); !errors.Is(err, runtime.ErrLeaseLost) {
				t.Fatal(err)
			}
			for k, r := range store.records {
				r.LeaseUntil = time.Now().Add(-time.Second)
				store.records[k] = r
			}
			value, err := guard.Call(ctx, []byte(`{"a":1}`))
			if inner.lookups != 1 || len(inner.effects) != 1 {
				t.Fatalf("lookup/effects: %d/%d", inner.lookups, len(inner.effects))
			}
			for _, key := range inner.keys {
				if key != inner.keys[0] {
					t.Fatal("key changed across takeover")
				}
			}
			var record execution.Record
			for _, r := range store.records {
				record = r
			}
			switch state {
			case RecoveryCompleted:
				if err != nil || value != "ok" || inner.calls != 1 || record.State != execution.Succeeded {
					t.Fatalf("completed recovery: %+v %v calls=%d", record, err, inner.calls)
				}
			case RecoveryNotFound:
				// Even a stale absence lookup is safe because the provider still
				// deduplicates the replay under exactly the original business key.
				if err != nil || inner.calls != 2 || record.State != execution.Succeeded {
					t.Fatal(record, err, inner.calls)
				}
			case RecoveryFailed:
				if !errors.Is(err, ErrRecoveryFailed) || inner.calls != 1 || record.State != execution.Failed {
					t.Fatal(record, err)
				}
			default:
				if err == nil || inner.calls != 1 || record.State != execution.EffectUnknown || len(store.outputs) != 0 {
					t.Fatal("unknown status replayed or persisted success", record, err)
				}
			}
		})
	}
}

func TestReceiptContractIsCheckedBeforeCompleteAndOnReuse(t *testing.T) {
	for _, reused := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "reuse"}[reused], func(t *testing.T) {
			store := newDurableMemory()
			inner := &lookupTool{recoveringTool: recoveringTool{policy: RecoveryIdempotentKey}, bad: !reused}
			guard, ctx := durableGuard(t, &inner.recoveringTool, store)
			guard.Inner = tenantBoundCallable{tenantID: "tenant", id: "safe", inner: inner}
			if reused {
				if _, err := guard.Call(ctx, []byte(`{}`)); err != nil {
					t.Fatal(err)
				}
				for k, r := range store.records {
					store.outputs[k] = []byte(`"business rejected"`)
					r.ResultRef = "tool-result://sha256/" + taskflow.Digest(store.outputs[k])
					store.records[k] = r
				}
			}
			if _, err := guard.Call(ctx, []byte(`{}`)); !errors.Is(err, taskflow.ErrEvidenceRejected) {
				t.Fatalf("false receipt accepted: %v", err)
			}
			if inner.calls != 1 {
				t.Fatal("rejected receipt caused another effect")
			}
			if !reused {
				if len(store.outputs) != 0 {
					t.Fatal("failed receipt was marked succeeded")
				}
				for _, r := range store.records {
					if r.State != execution.Failed || r.LastError != "tool_receipt_rejected" {
						t.Fatal(r)
					}
				}
			}
		})
	}
}

func TestLookupFailureIsRecordedAndCannotFallThroughToWrite(t *testing.T) {
	store := newDurableMemory()
	inner := &lookupTool{recoveringTool: recoveringTool{policy: RecoveryIdempotentKey}, err: errors.New("provider lookup unavailable")}
	guard, ctx := durableGuard(t, &inner.recoveringTool, store)
	guard.Inner = tenantBoundCallable{tenantID: "tenant", id: "safe", inner: inner}
	if _, err := guard.Call(inflight.WithBarrier(ctx, crashToolResult{}), []byte(`{}`)); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatal(err)
	}
	for k, r := range store.records {
		r.LeaseUntil = time.Now().Add(-time.Second)
		store.records[k] = r
	}
	if _, err := guard.Call(ctx, []byte(`{}`)); !errors.Is(err, inner.err) {
		t.Fatal(err)
	}
	if inner.calls != 1 {
		t.Fatal("lookup timeout caused a write")
	}
	for _, r := range store.records {
		if r.State != execution.EffectUnknown || r.LastError != "recovery_lookup_error" {
			t.Fatal(r)
		}
	}
}
