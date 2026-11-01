package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/agentapp"
	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/profile"
	profilememory "github.com/liuzengh/trpc-agent-service/trpcservice/profile/inmemory"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	messagingmemory "github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging/inmemory"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
	sessionmemory "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session/inmemory"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	taskmemory "github.com/liuzengh/trpc-agent-service/trpcservice/taskflow/inmemory"
	sdkmemory "trpc.group/trpc-go/trpc-agent-go/session/inmemory"
)

func recoveryFixture(t *testing.T, planned bool) (RunnerExecutor, runtime.ExecutionEnvelope, messaging.PayloadRecord, *messagingmemory.Store, *sessionmemory.Store, *taskflow.Session) {
	t.Helper()
	ctx := context.Background()
	e := runtime.ExecutionEnvelope{SchemaVersion: 1, TenantID: "tenant-a", TenantVersion: 1, AgentAppID: "app", AgentAppVersion: 1, AgentAppRevision: 1, AgentContentDigest: "digest", ConfigVersion: 1, PolicyVersion: 1, RequestID: "request", SessionID: "session", UserID: "user", Channel: "fake", InputSeq: 1, PayloadRef: "payload://request", CreatedAt: time.Now().UTC()}
	k := profile.ExecutionProfileKey{TenantID: e.TenantID, TenantVersion: 1, AgentAppID: "app", AgentAppVersion: 1, AgentAppRevision: 1, ContentDigest: "digest", ConfigVersion: 1, PolicyVersion: 1}
	profiles := profilememory.NewResolver(profile.ExecutionProfileSnapshot{Key: k, TenantVersion: 1, AgentAppVersion: 1, ContentDigest: "digest", AppName: "tenant-a/app", AgentKind: agentapp.AgentKindLLM, ModelProfileRef: profile.VersionedRef{ID: "model", Version: 1}})
	bundles := profilememory.NewBundleManager(func(context.Context, profile.ExecutionProfileKey) (profile.RuntimeBundle, func(context.Context) error, error) {
		t.Fatal("recovery entered model path")
		return nil, nil, runtime.ErrCapabilityUnsupported
	})
	payloads := messagingmemory.New()
	p := messaging.PayloadRecord{TenantID: e.TenantID, RequestID: e.RequestID, PayloadRef: e.PayloadRef, Content: []byte(`{"text":"original"}`), ContentDigest: "digest", KeyVersion: 1}
	if err := payloads.PutPayload(ctx, p); err != nil {
		t.Fatal(err)
	}
	sessions := sessionmemory.New()
	w := RunnerExecutor{Tasks: taskStub{envelope: e}, Profiles: profiles, Bundles: bundles, Sessions: sessions, SDKSessions: sdkmemory.NewSessionService(), Payloads: payloads, Inputs: JSONTextInputDecoder{}}
	var task *taskflow.Session
	if planned {
		plans := taskmemory.New()
		scope := taskflow.Scope{Key: taskflow.Key{TenantID: e.TenantID, RequestID: e.RequestID}, Fence: 1}
		if _, err := plans.Open(ctx, scope, taskflow.Digest(p.Content), p.KeyVersion); err != nil {
			t.Fatal(err)
		}
		task = &taskflow.Session{Store: plans, Scope: scope, Tools: unusedToolResults{}, Results: unusedToolResults{}}
		addAnalysis(t, task, "analysis")
		w.TaskPlans, w.ToolExecutions = plans, unusedToolResults{}
	}
	return w, e, p, payloads, sessions, task
}
func addAnalysis(t *testing.T, task *taskflow.Session, id string) {
	t.Helper()
	ctx := context.Background()
	p, err := task.Store.Get(ctx, task.Scope.Key)
	if err != nil {
		t.Fatal(err)
	}
	p, err = task.Define(ctx, p.Version, []taskflow.Step{{ID: id, Kind: "analysis", Title: id, Criterion: "saved calculation"}})
	if err != nil {
		t.Fatal(err)
	}
	p, err = task.Start(ctx, p.Version, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = task.CompleteAnalysis(ctx, p.Version, id, "2+3=5"); err != nil {
		t.Fatal(err)
	}
}

type completionReadView struct {
	*messagingmemory.Store
	record *messaging.CompletionRecord
	err    error
}

func (s completionReadView) GetCompletion(ctx context.Context, tenant, request string) (messaging.CompletionRecord, error) {
	if s.err != nil {
		return messaging.CompletionRecord{}, s.err
	}
	if s.record != nil {
		return *s.record, nil
	}
	return s.Store.GetCompletion(ctx, tenant, request)
}

type failRecoveryCommit struct {
	*sessionmemory.Store
	hit bool
	err error
}

func (s *failRecoveryCommit) CommitTurn(ctx context.Context, r sessionstore.CommitTurnRequest) (sessionstore.CommitTurnResult, error) {
	if !s.hit && strings.Contains(r.CommitID, ":recovery:") {
		s.hit = true
		return sessionstore.CommitTurnResult{}, s.err
	}
	return s.Store.CommitTurn(ctx, r)
}

func TestRecoveryStateMatrixPreservesEvidenceAndConverges(t *testing.T) {
	for _, scenario := range []string{"public_conflict", "changed_plan", "sealed_without_result", "failure_commit_crash", "legacy_candidate", "equivalent_timestamp", "transient_read", "corrupt_candidate_read"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			w, e, p, store, sessions, task := recoveryFixture(t, true)
			ctx = taskflow.WithSession(ctx, task)
			var candidate messaging.CompletionRecord
			var err error
			if scenario != "sealed_without_result" {
				candidate, err = w.saveCompletion(ctx, e, 1, nil, p, governance.RunPermit{}, runnerResult{}, OutboundContent{Content: []byte("approved original"), ContentType: messaging.ContentTypeText})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err = task.Seal(ctx, "result://missing"); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "changed_plan" {
				addAnalysis(t, task, "new-obligation")
			}
			if scenario == "public_conflict" || scenario == "failure_commit_crash" {
				if err = store.PutResult(ctx, messaging.ResultRecord{TenantID: e.TenantID, RequestID: e.RequestID, ResultRef: "result://conflicting", Content: []byte("different answer"), ContentDigest: taskflow.Digest([]byte("different answer")), ContentType: messaging.ContentTypeText, KeyVersion: 1}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "legacy_candidate" {
				var c completionCandidate
				_ = json.Unmarshal(candidate.Content, &c)
				c.SchemaVersion = 1
				c.PlanDigest = ""
				candidate.Content, _ = json.Marshal(c)
				candidate.ContentDigest = taskflow.Digest(candidate.Content)
				w.Payloads = completionReadView{Store: store, record: &candidate}
			}
			if scenario == "equivalent_timestamp" {
				e.CreatedAt = e.CreatedAt.In(time.FixedZone("other", 8*3600))
			}
			if scenario == "corrupt_candidate_read" {
				w.Payloads = completionReadView{Store: store, err: runtime.ErrInvariantViolation}
			}
			if scenario == "transient_read" {
				readErr := errors.New("database unavailable")
				w.Payloads = completionReadView{Store: store, err: readErr}
				if err = w.ExecuteWithLease(context.Background(), e, 2, nil); !errors.Is(err, readErr) {
					t.Fatal(err)
				}
				if _, err = sessions.GetTerminalByInputSeq(ctx, sessionstore.TerminalKey{SessionKey: sessionstore.SessionKey{TenantID: e.TenantID, AgentAppID: e.AgentAppID, SessionID: e.SessionID}, InputSeq: 1}); !errors.Is(err, runtime.ErrNotFound) {
					t.Fatal("temporary failure was made terminal")
				}
				return
			}
			if scenario == "failure_commit_crash" {
				crash := errors.New("commit transport failure")
				w.Sessions = &failRecoveryCommit{Store: sessions, err: crash}
				if err = w.ExecuteWithLease(context.Background(), e, 2, nil); !errors.Is(err, crash) {
					t.Fatal(err)
				}
			}
			if err = w.ExecuteWithLease(context.Background(), e, 3, nil); err != nil {
				t.Fatal(err)
			}
			terminal, err := sessions.GetTerminalByInputSeq(ctx, sessionstore.TerminalKey{SessionKey: sessionstore.SessionKey{TenantID: e.TenantID, AgentAppID: e.AgentAppID, SessionID: e.SessionID}, InputSeq: 1})
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "legacy_candidate" || scenario == "equivalent_timestamp" {
				if terminal.Outcome != runtime.OutcomeSucceeded {
					t.Fatal(terminal)
				}
				return
			}
			if terminal.Outcome != runtime.OutcomeFailed || terminal.ResultRef != "" {
				t.Fatalf("contradiction was treated as success: %+v", terminal)
			}
			_, outbox, _ := sessions.SnapshotEffects(sessionstore.SessionKey{TenantID: e.TenantID, AgentAppID: e.AgentAppID, SessionID: e.SessionID})
			for _, o := range outbox {
				if o.Kind == "reply" {
					t.Fatal("suspect output was published")
				}
			}
			if scenario != "sealed_without_result" {
				saved, err := store.GetCompletion(ctx, e.TenantID, e.RequestID)
				if err != nil || saved.ContentDigest != candidate.ContentDigest {
					t.Fatal("candidate evidence overwritten")
				}
			}
			if _, err = sessions.OpenForRun(ctx, sessionstore.OpenForRunRequest{SessionKey: sessionstore.SessionKey{TenantID: e.TenantID, AgentAppID: e.AgentAppID, SessionID: e.SessionID}, RequestID: "next", InputSeq: 2, Fence: 4}); err != nil {
				t.Fatal("failed recovery blocked next input", err)
			}
			next := e
			next.InputSeq = 2
			next.RequestID = "next"
			history, err := w.committedHistory(ctx, next)
			if err != nil || len(history) != 1 || history[0].Outcome != runtime.OutcomeFailed || strings.Contains(history[0].Answer.Content, "different answer") || strings.Contains(history[0].Answer.Content, "approved original") {
				t.Fatalf("failed recovery polluted next history: %+v %v", history, err)
			}
		})
	}
}
