package worker

import (
	"context"
	"fmt"

	"github.com/liuzengh/trpc-agent-service/trpcservice/gateway"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

func (w RunnerExecutor) committedHistory(ctx context.Context, e runtime.ExecutionEnvelope) ([]sessionstore.CommittedHistoryTurn, error) {
	reader, ok := w.Sessions.(sessionstore.TerminalHistoryReader)
	if !ok {
		return nil, runtime.ErrCapabilityUnsupported
	}
	key := sessionstore.SessionKey{TenantID: e.TenantID, AgentAppID: e.AgentAppID, SessionID: e.SessionID}
	rows, err := reader.ListTerminalHistory(ctx, key, e.InputSeq, sessionstore.MaxHistoryTurns)
	if err != nil {
		return nil, err
	}
	if e.InputSeq > 1 && (len(rows) == 0 || rows[len(rows)-1].InputSeq != e.InputSeq-1) {
		return nil, runtime.ErrInvariantViolation
	}
	results, ok := w.Payloads.(messaging.ResultStore)
	if !ok {
		return nil, runtime.ErrCapabilityUnsupported
	}
	var history []sessionstore.CommittedHistoryTurn
	var previous uint64
	for _, row := range rows {
		if row.RequestID == "" || row.CommitID == "" || row.Fence == 0 || !row.Outcome.Terminal() || row.InputSeq <= previous || (previous > 0 && row.InputSeq != previous+1) || row.InputSeq >= e.InputSeq {
			return nil, runtime.ErrInvariantViolation
		}
		previous = row.InputSeq
		status, err := w.Tasks.GetExecution(ctx, gateway.ExecutionKey{TenantID: e.TenantID, RequestID: row.RequestID})
		if err != nil {
			return nil, err
		}
		original := status.Envelope
		if original.TenantID != e.TenantID || original.AgentAppID != e.AgentAppID || original.SessionID != e.SessionID || original.UserID != e.UserID || original.RequestID != row.RequestID || original.InputSeq != row.InputSeq {
			return nil, runtime.ErrTenantScope
		}
		if row.CommittedAt.IsZero() {
			row.CommittedAt = original.CreatedAt
		}
		h := sessionstore.CommittedHistoryTurn{TerminalHistory: row}
		var inputKeyVersion int64
		// Rejected/cancelled input may never have passed input DLP. Do not
		// introduce it into a later model context through the history path.
		if row.Outcome == runtime.OutcomeSucceeded {
			payload, err := w.executionPayload(ctx, original)
			if err != nil {
				return nil, err
			}
			if payload.TenantID != e.TenantID || payload.RequestID != row.RequestID || payload.PayloadRef != original.PayloadRef {
				return nil, runtime.ErrVersionMismatch
			}
			input, err := w.Inputs.DecodeInput(ctx, original, payload.Content)
			if err != nil {
				return nil, err
			}
			if input.Role != model.RoleUser || !validPreparedMessage(input) {
				return nil, runtime.ErrInvalidEnvelope
			}
			if err = w.hydrateArtifacts(ctx, original, &input); err != nil {
				return nil, err
			}
			inputKeyVersion = payload.KeyVersion
			h.Input = &input
		}
		if row.ResultRef != "" {
			if row.Outcome != runtime.OutcomeSucceeded && row.Outcome != runtime.OutcomeFailed {
				return nil, runtime.ErrInvariantViolation
			}
			r, err := results.GetResult(ctx, e.TenantID, row.RequestID)
			if err != nil {
				return nil, err
			}
			if err = validateFrozenResult(r, original); err != nil {
				return nil, err
			}
			if r.ResultRef != row.ResultRef {
				return nil, runtime.ErrVersionMismatch
			}
			if inputKeyVersion > 0 && r.KeyVersion != inputKeyVersion {
				return nil, runtime.ErrVersionMismatch
			}
			// A failed terminal can carry the deliberately safe failure result.
			// Never substitute an uncommitted model draft for this exact payload.
			if r.ContentType == messaging.ContentTypeText || r.ContentType == messaging.ContentTypeCard {
				h.Answer = model.NewAssistantMessage(string(r.Content))
			} else {
				h.Answer = model.NewAssistantMessage(fmt.Sprintf("[服务端已提交 %s 结果，引用 %s；渠道送达状态需另查投递记录]", r.ContentType, row.ResultRef))
			}
		} else {
			if row.Outcome == runtime.OutcomeSucceeded {
				return nil, runtime.ErrInvariantViolation
			}
			h.Answer = model.NewAssistantMessage(fmt.Sprintf("[服务端执行状态：%s；未提交可用最终答案。此状态不表示外部副作用已回滚，也不表示已向用户发送通知。]", row.Outcome))
		}
		history = append(history, h)
	}
	return history, nil
}

func validateFrozenResult(r messaging.ResultRecord, e runtime.ExecutionEnvelope) error {
	contentType, err := messaging.NormalizeContentType(r.ContentType)
	if r.TenantID != e.TenantID || r.RequestID != e.RequestID || r.ResultRef == "" || r.KeyVersion < 1 || r.ContentDigest != taskflow.Digest(r.Content) || err != nil {
		return runtime.ErrVersionMismatch
	}
	_, err = validateOutbound(OutboundContent{Content: r.Content, ContentType: contentType})
	return err
}
