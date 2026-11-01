package localnote

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	governancememory "github.com/liuzengh/trpc-agent-service/trpcservice/governance/inmemory"
	"github.com/liuzengh/trpc-agent-service/trpcservice/profile"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	messagingmemory "github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging/inmemory"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	taskmemory "github.com/liuzengh/trpc-agent-service/trpcservice/taskflow/inmemory"
	servicetool "github.com/liuzengh/trpc-agent-service/trpcservice/tool"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/execution"
)

type businessNotes struct {
	rows             map[string]Note
	creates, lookups int
}

func (w *businessNotes) Create(_ context.Context, n Note) (Result, error) {
	w.creates++
	k := n.TenantID + ":" + n.BusinessKey
	if old, ok := w.rows[k]; ok {
		if old.RequestID != n.RequestID || old.ArgsDigest != n.ArgsDigest {
			return Result{}, runtime.ErrIdempotencyCollision
		}
		return old.Result, nil
	}
	w.rows[k] = n
	return n.Result, nil
}
func (w *businessNotes) Lookup(_ context.Context, k LookupKey) (Result, error) {
	w.lookups++
	n, ok := w.rows[k.TenantID+":"+k.BusinessKey]
	if !ok {
		return Result{}, runtime.ErrNotFound
	}
	if n.RequestID != k.RequestID || n.ArgsDigest != k.ArgsDigest {
		return Result{}, runtime.ErrIdempotencyCollision
	}
	return n.Result, nil
}

type noteExecutions struct {
	execution.Store
	mu     sync.Mutex
	record execution.Record
	output []byte
	fail   bool
}

func (s *noteExecutions) Get(_ context.Context, k execution.Key) (execution.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.record.Key != k || s.record.Attempt == 0 {
		return execution.Record{}, runtime.ErrNotFound
	}
	return s.record, nil
}
func (s *noteExecutions) Claim(_ context.Context, r execution.Record, c execution.Claim) (execution.Record, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.record
	if old.State == execution.Succeeded || old.State == execution.Running && old.LeaseUntil.After(time.Now()) {
		return old, false, nil
	}
	r.Attempt = old.Attempt + 1
	r.Fence = old.Fence + 1
	r.State = execution.Running
	r.LeaseOwner = c.Owner
	r.LeaseUntil = time.Now().Add(c.TTL)
	s.record = r
	return r, true, nil
}
func (s *noteExecutions) Renew(_ context.Context, r execution.Record, ttl time.Duration) (execution.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Fence != s.record.Fence || r.LeaseOwner != s.record.LeaseOwner {
		return r, runtime.ErrLeaseLost
	}
	s.record.LeaseUntil = time.Now().Add(ttl)
	return s.record, nil
}
func (s *noteExecutions) Finish(_ context.Context, r execution.Record, state execution.State, ref, reason string) (execution.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.Fence != s.record.Fence || r.LeaseOwner != s.record.LeaseOwner {
		return r, runtime.ErrLeaseLost
	}
	r.State = state
	r.ResultRef = ref
	r.LastError = reason
	s.record = r
	return r, nil
}
func (s *noteExecutions) Complete(_ context.Context, r execution.Record, b []byte, _ int64) (execution.Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		s.fail = false
		return r, runtime.ErrLeaseLost
	}
	if r.Fence != s.record.Fence || r.LeaseOwner != s.record.LeaseOwner || !s.record.LeaseUntil.After(time.Now()) {
		return r, runtime.ErrLeaseLost
	}
	r.State = execution.Succeeded
	r.ResultRef = "tool-result://sha256/" + taskflow.Digest(b)
	s.record = r
	s.output = append([]byte(nil), b...)
	return r, nil
}
func (s *noteExecutions) LoadResult(_ context.Context, k execution.Key) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if k != s.record.Key || s.record.State != execution.Succeeded {
		return nil, runtime.ErrNotFound
	}
	return append([]byte(nil), s.output...), nil
}

type noteGrants struct {
	governance.ConfirmationCoordinator
	attempt                   governance.ToolAttempt
	consumed, consumeAttempts int
}

func (g *noteGrants) ConsumeGrant(_ context.Context, c governance.GrantClaim) (governance.Grant, error) {
	g.consumeAttempts++
	if g.consumed > 0 {
		return governance.Grant{}, runtime.ErrVersionConflict
	}
	g.consumed++
	g.attempt = governance.ToolAttempt{TenantID: c.TenantID, GrantID: c.GrantID, RequestID: c.RequestID, ToolCallID: c.ToolCallID, State: governance.ToolAttemptEffectUnknown}
	return governance.Grant{}, nil
}
func (g *noteGrants) GetToolAttempt(context.Context, string, string) (governance.ToolAttempt, error) {
	return g.attempt, nil
}
func (g *noteGrants) FinishToolAttempt(_ context.Context, r governance.FinishToolAttemptRequest) (governance.ToolAttempt, error) {
	g.attempt.State = r.State
	g.attempt.ResultRef = r.ResultRef
	return g.attempt, nil
}

func TestConfirmedNoteTakeoverQueriesBusinessFactAndRepairsOriginalCheckpoint(t *testing.T) {
	ctx := context.Background()
	writer := &businessNotes{rows: map[string]Note{}}
	catalog, err := servicetool.NewCatalog(Registration("tenant", writer))
	if err != nil {
		t.Fatal(err)
	}
	tools, err := (servicetool.Resolver{Catalog: catalog}).ResolveTools(ctx, "tenant", []profile.VersionedRef{{ID: ID, Version: Version}})
	if err != nil {
		t.Fatal(err)
	}
	policies := governancememory.New(0, 0)
	policy := governance.PolicyV1{SchemaVersion: 1, DefaultAction: governance.ActionAllow, InputDLP: governance.DLPDisabled, OutputDLP: governance.DLPDisabled, Tools: []governance.ToolRule{{ToolID: ID, Version: Version, Dangerous: true, ConfirmationSupported: true}}}
	digest, _, _ := governance.PolicyDigest(policy)
	if err = policies.PublishPolicy(governance.PolicySnapshot{TenantID: "tenant", Version: 1, SchemaVersion: 1, Policy: policy, ContentDigest: digest, PublishedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	store := &noteExecutions{fail: true}
	grants := &noteGrants{}
	mirrors := messagingmemory.New()
	guarded, err := servicetool.GuardCallablesWithExecution(policies, grants, mirrors, store, []governance.VersionedRef{{ID: ID, Version: Version}}, tools)
	if err != nil {
		t.Fatal(err)
	}
	args := []byte(`{"title":"note","content":"one business effect"}`)
	_, argsDigest, _ := governance.CanonicalArguments(args)
	exec := runtime.ExecutionContext{TenantID: "tenant", RequestID: "request", SubjectID: "user", ToolCallID: "confirmed-call", ArgsDigest: argsDigest, PolicyVersion: 1, PayloadKeyVersion: 1, GrantID: "grant", GrantVersion: 1}
	ctx = runtime.WithExecutionContext(ctx, exec)
	plans := taskmemory.New()
	scope := taskflow.Scope{Key: taskflow.Key{TenantID: "tenant", RequestID: "request"}, Fence: 1}
	if _, err = plans.Open(ctx, scope, taskflow.Digest([]byte("original input")), 1); err != nil {
		t.Fatal(err)
	}
	task := &taskflow.Session{Scope: scope, Store: plans, Tools: store, Results: store}
	p, err := task.Define(ctx, 1, []taskflow.Step{{ID: "note", Title: "create note", Criterion: "created receipt", Kind: "tool", ToolID: ID, ToolVersion: Version, Check: &taskflow.ResultCheck{Path: []string{"status"}, EqualsJSON: `"created"`}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = task.Start(ctx, p.Version, "note"); err != nil {
		t.Fatal(err)
	}
	ctx = taskflow.WithSession(ctx, task)
	call := guarded[0].(interface {
		Call(context.Context, []byte) (any, error)
	})
	if _, err = call.Call(ctx, args); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatalf("did not hit effect-before-result gap: %v", err)
	}
	if writer.creates != 1 || len(writer.rows) != 1 || len(store.output) != 0 {
		t.Fatal("wrong failure window")
	}
	store.mu.Lock()
	store.record.LeaseUntil = time.Now().Add(-time.Second)
	store.mu.Unlock()
	scope.Fence = 2
	if _, err = plans.Open(ctx, scope, taskflow.Digest([]byte("original input")), 1); err != nil {
		t.Fatal(err)
	}
	task = &taskflow.Session{Scope: scope, Store: plans, Tools: store, Results: store}
	ctx = taskflow.WithSession(ctx, task)
	if _, err = task.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = call.Call(ctx, args); err != nil {
		t.Fatal(err)
	}
	if writer.creates != 1 || writer.lookups != 1 || len(writer.rows) != 1 {
		t.Fatalf("writes/lookups/effects=%d/%d/%d", writer.creates, writer.lookups, len(writer.rows))
	}
	if store.record.Attempt != 2 || grants.consumed != 1 || grants.consumeAttempts != 2 || grants.attempt.State != governance.ToolAttemptSucceeded {
		t.Fatal("attempt/grant counters confused")
	}
	p, err = task.ValidateCompletion(ctx)
	if err != nil || p.Steps[0].ToolCallID != "confirmed-call" || p.Steps[0].ResultRef != store.record.ResultRef {
		t.Fatal("original checkpoint not repaired", p, err)
	}
	mirrored, err := mirrors.GetToolResult(ctx, "tenant", "grant")
	if err != nil || mirrored.ResultRef != store.record.ResultRef {
		t.Fatal("confirmation mirror not restored", err)
	}
	if _, err = (Tool{Writer: writer}).LookupRecovery(ctx, "other-key"); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatal("unrelated operation queried", err)
	}
}

var _ messaging.ToolResultStore = (*messagingmemory.Store)(nil)
