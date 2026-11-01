package inmemory

import (
	"context"
	"sort"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
)

func (s *Store) ListTerminalHistory(ctx context.Context, key sessionstore.SessionKey, before uint64, limit int) ([]sessionstore.TerminalHistory, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key.TenantID == "" || key.AgentAppID == "" || key.SessionID == "" {
		return nil, runtime.ErrTenantScope
	}
	if before == 0 || limit < 1 || limit > sessionstore.MaxHistoryTurns {
		return nil, runtime.ErrInvalidEnvelope
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result []sessionstore.TerminalHistory
	for _, c := range s.commits {
		if c.SessionKey == key && c.InputSeq < before && c.Outcome.Terminal() {
			result = append(result, sessionstore.TerminalHistory{RequestID: c.RequestID, CommitID: c.CommitID, InputSeq: c.InputSeq, Fence: c.Fence, Outcome: c.Outcome, ResultRef: c.ResultRef})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].InputSeq < result[j].InputSeq })
	if len(result) > limit {
		result = result[len(result)-limit:]
	}
	return result, nil
}

var _ sessionstore.TerminalHistoryReader = (*Store)(nil)
