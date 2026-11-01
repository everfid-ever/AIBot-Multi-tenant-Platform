package inmemory

import (
	"context"
	"fmt"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
)

func TestTerminalHistoryIsolationAndNewestWindow(t *testing.T) {
	s := New()
	ctx := context.Background()
	for _, tenant := range []string{"a", "b"} {
		key := sessionstore.SessionKey{TenantID: tenant, AgentAppID: "app", SessionID: "same"}
		for i := uint64(1); i <= 4; i++ {
			r := fmt.Sprintf("%s-%d", tenant, i)
			head, err := s.OpenForRun(ctx, sessionstore.OpenForRunRequest{SessionKey: key, RequestID: r, InputSeq: i, Fence: i})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.CommitTurn(ctx, sessionstore.CommitTurnRequest{SessionKey: key, RequestID: r, CommitID: r, Stage: "terminal", InputSeq: i, Fence: i, ExpectedVersion: head.Version, Outcome: runtime.OutcomeCancelled}); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows, err := s.ListTerminalHistory(ctx, sessionstore.SessionKey{TenantID: "a", AgentAppID: "app", SessionID: "same"}, 4, 2)
	if err != nil || len(rows) != 2 || rows[0].RequestID != "a-2" || rows[1].RequestID != "a-3" {
		t.Fatalf("history=%+v err=%v", rows, err)
	}
}
