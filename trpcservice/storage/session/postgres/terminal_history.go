package postgres

import (
	"context"
	"math"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
)

func (s *Store) ListTerminalHistory(ctx context.Context, key sessionstore.SessionKey, before uint64, limit int) ([]sessionstore.TerminalHistory, error) {
	if key.TenantID == "" || key.AgentAppID == "" || key.SessionID == "" {
		return nil, runtime.ErrTenantScope
	}
	if before == 0 || before > math.MaxInt64 || limit < 1 || limit > sessionstore.MaxHistoryTurns {
		return nil, runtime.ErrInvalidEnvelope
	}
	rows, err := s.db.QueryContext(ctx, `SELECT request_id,commit_id,input_seq,fence,outcome,COALESCE(result_ref,''),created_at
FROM (SELECT request_id,commit_id,input_seq,fence,outcome,result_ref,created_at FROM session_commit
WHERE tenant_id=$1 AND agent_app_id=$2 AND session_id=$3 AND input_seq<$4
AND outcome IN ('succeeded','denied','failed','cancelled','confirmation_denied','confirmation_timeout')
ORDER BY input_seq DESC LIMIT $5) recent ORDER BY input_seq`, key.TenantID, key.AgentAppID, key.SessionID, int64(before), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []sessionstore.TerminalHistory
	for rows.Next() {
		var h sessionstore.TerminalHistory
		if err = rows.Scan(&h.RequestID, &h.CommitID, &h.InputSeq, &h.Fence, &h.Outcome, &h.ResultRef, &h.CommittedAt); err != nil {
			return nil, err
		}
		result = append(result, h)
	}
	return result, rows.Err()
}

var _ sessionstore.TerminalHistoryReader = (*Store)(nil)
