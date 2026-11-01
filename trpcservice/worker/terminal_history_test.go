package worker

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	serviceagent "github.com/liuzengh/trpc-agent-service/trpcservice/agent"
	"github.com/liuzengh/trpc-agent-service/trpcservice/gateway"
	"github.com/liuzengh/trpc-agent-service/trpcservice/profile"
	profilememory "github.com/liuzengh/trpc-agent-service/trpcservice/profile/inmemory"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	"trpc.group/trpc-go/trpc-agent-go/event"
	"trpc.group/trpc-go/trpc-agent-go/model"
	sdksession "trpc.group/trpc-go/trpc-agent-go/session"
)

type historyTasks struct {
	taskStub
	envelopes map[string]runtime.ExecutionEnvelope
}

func (s historyTasks) GetExecution(_ context.Context, k gateway.ExecutionKey) (gateway.ExecutionStatus, error) {
	e, ok := s.envelopes[k.RequestID]
	if !ok || e.TenantID != k.TenantID {
		return gateway.ExecutionStatus{}, runtime.ErrNotFound
	}
	return gateway.ExecutionStatus{Envelope: e}, nil
}

type historyModel struct{ requests [][]model.Message }

func (m *historyModel) GenerateContent(_ context.Context, r *model.Request) (<-chan *model.Response, error) {
	m.requests = append(m.requests, append([]model.Message(nil), r.Messages...))
	ch := make(chan *model.Response, 1)
	ch <- &model.Response{Done: true, Choices: []model.Choice{{Message: model.NewAssistantMessage("raw model draft")}}}
	close(ch)
	return ch, nil
}
func (*historyModel) Info() model.Info { return model.Info{Name: "history-model"} }

func TestNextModelContextUsesCommittedRenderedAnswerAfterTakeover(t *testing.T) {
	ctx := context.Background()
	w, first, _, payloads, _, _ := recoveryFixture(t, false)
	second := first
	second.RequestID = "second"
	second.InputSeq = 2
	second.PayloadRef = "payload://second"
	w.Tasks = historyTasks{envelopes: map[string]runtime.ExecutionEnvelope{first.RequestID: first, second.RequestID: second}}
	m := &historyModel{}
	factory := serviceagent.Factory{Profiles: w.Profiles, Models: staticModelResolver{model: m}}
	w.Bundles = profilememory.NewBundleManager(func(ctx context.Context, k profile.ExecutionProfileKey) (profile.RuntimeBundle, func(context.Context) error, error) {
		p, err := w.Profiles.Resolve(ctx, k)
		if err != nil {
			return nil, nil, err
		}
		root, err := factory.Build(ctx, p)
		return &serviceagent.Bundle{AppName: p.AppName, Root: root}, nil, err
	})
	w.OutputRenderer = OutboundRendererFunc(func(context.Context, runtime.ExecutionEnvelope, string) (OutboundContent, error) {
		return OutboundContent{Content: []byte("exact committed TOKEN-A"), ContentType: messaging.ContentTypeText}, nil
	})
	w.Barrier = crashAfterSettlement{}
	if err := w.ExecuteWithLease(ctx, first, 1, nil); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatal(err)
	}
	w.Barrier = nil
	if err := w.ExecuteWithLease(ctx, first, 2, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 1 {
		t.Fatal("takeover regenerated answer")
	}
	// Simulate a late old process which writes a high-fence draft to SDK.
	key := sdksession.Key{AppName: "tenant-a/app", UserID: first.UserID, SessionID: first.SessionID}
	raw, err := w.SDKSessions.GetSession(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	stamp, _ := json.Marshal(map[string]any{"request_id": first.RequestID, "fence": 999})
	if err = w.SDKSessions.AppendEvent(ctx, raw, &event.Event{ID: "late-draft", RequestID: first.RequestID, Author: "", Response: &model.Response{Done: true, Choices: []model.Choice{{Message: model.NewAssistantMessage("incorrect late TOKEN-B")}}}, Extensions: map[string]json.RawMessage{"trpc.execution.v1": stamp}}); err != nil {
		t.Fatal(err)
	}
	input := []byte(`{"text":"what did you just answer?"}`)
	if err = payloads.PutPayload(ctx, messaging.PayloadRecord{TenantID: second.TenantID, RequestID: second.RequestID, PayloadRef: second.PayloadRef, Content: input, ContentDigest: taskflow.Digest(input), KeyVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err = w.ExecuteWithLease(ctx, second, 3, nil); err != nil {
		t.Fatal(err)
	}
	if len(m.requests) != 2 {
		t.Fatal("missing follow-up run")
	}
	foundInput, foundAnswer := false, false
	for _, msg := range m.requests[1] {
		if strings.Contains(msg.Content, "TOKEN-B") || msg.Content == "raw model draft" {
			t.Fatalf("uncommitted output entered context: %+v", msg)
		}
		if msg.Role == model.RoleAssistant && msg.Content == "exact committed TOKEN-A" {
			foundAnswer = true
		}
		if msg.Role == model.RoleUser && msg.Content == "original" {
			foundInput = true
		}
	}
	if !foundInput || !foundAnswer {
		t.Fatalf("committed turn absent or roles changed: %+v", m.requests[1])
	}
	raw, err = w.SDKSessions.GetSession(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	foundAudit := false
	for _, e := range raw.Events {
		if e.ID == "late-draft" {
			foundAudit = true
		}
	}
	if !foundAudit {
		t.Fatal("raw audit trail was deleted")
	}
}

func TestRejectedCancelledAndFailedHistoryDoesNotReuseDraftOrRejectedInput(t *testing.T) {
	ctx := context.Background()
	w, e, _, payloads, sessions, _ := recoveryFixture(t, false)
	statuses := []runtime.Outcome{runtime.OutcomeDenied, runtime.OutcomeCancelled, runtime.OutcomeFailed, runtime.OutcomeConfirmationDenied, runtime.OutcomeConfirmationTimeout}
	envelopes := map[string]runtime.ExecutionEnvelope{}
	key := sessionstore.SessionKey{TenantID: e.TenantID, AgentAppID: e.AgentAppID, SessionID: e.SessionID}
	for i, outcome := range statuses {
		original := e
		original.RequestID = string(outcome)
		original.InputSeq = uint64(i + 1)
		envelopes[original.RequestID] = original
		head, err := sessions.OpenForRun(ctx, sessionstore.OpenForRunRequest{SessionKey: key, RequestID: original.RequestID, InputSeq: original.InputSeq, Fence: original.InputSeq})
		if err != nil {
			t.Fatal(err)
		}
		ref := ""
		if outcome == runtime.OutcomeFailed {
			ref = "result://safe-failure"
			content := []byte("safe failure: effect remains unknown")
			if err = payloads.PutResult(ctx, messaging.ResultRecord{TenantID: e.TenantID, RequestID: original.RequestID, ResultRef: ref, Content: content, ContentDigest: taskflow.Digest(content), ContentType: messaging.ContentTypeText, KeyVersion: 1}); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = sessions.CommitTurn(ctx, sessionstore.CommitTurnRequest{SessionKey: key, RequestID: original.RequestID, CommitID: original.RequestID, Stage: "terminal", InputSeq: original.InputSeq, Fence: original.InputSeq, ExpectedVersion: head.Version, Outcome: outcome, ResultRef: ref}); err != nil {
			t.Fatal(err)
		}
	}
	w.Tasks = historyTasks{envelopes: envelopes}
	e.RequestID = "next"
	e.InputSeq = 6
	history, err := w.committedHistory(ctx, e)
	if err != nil || len(history) != len(statuses) {
		t.Fatalf("history=%+v err=%v", history, err)
	}
	for _, h := range history {
		if h.Input != nil || h.Answer.Content == "" {
			t.Fatal("rejected/unreviewed input loaded or terminal missing")
		}
		if h.Outcome == runtime.OutcomeFailed && h.Answer.Content != "safe failure: effect remains unknown" {
			t.Fatal("failure replaced with success draft")
		}
	}
	// Terminal metadata cannot redirect a read to another user's conversation.
	wrong := envelopes[string(runtime.OutcomeDenied)]
	wrong.UserID = "other-user"
	envelopes[wrong.RequestID] = wrong
	if _, err = w.committedHistory(ctx, e); !errors.Is(err, runtime.ErrTenantScope) {
		t.Fatalf("history scope bypass: %v", err)
	}
}
