package worker

import (
	"context"
	"errors"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

func completionConflict(err error) bool {
	return errors.Is(err, governance.ErrFinalizationConflict) || errors.Is(err, runtime.ErrVersionMismatch) || errors.Is(err, runtime.ErrInvariantViolation) || errors.Is(err, runtime.ErrIdempotencyCollision) || errors.Is(err, runtime.ErrInvalidEnvelope) || errors.Is(err, taskflow.ErrIncomplete) || errors.Is(err, taskflow.ErrEvidenceRejected) || errors.Is(err, taskflow.ErrEvidenceRequired)
}

func recoveryReason(reason string) bool {
	switch reason {
	case "completion_conflict", "legacy_result_conflict", "sealed_result_missing", "closed_reservation_without_completion":
		return true
	}
	return false
}

func (w RunnerExecutor) recoverCompletion(ctx context.Context, turn *sessionstore.BufferedTurn, e runtime.ExecutionEnvelope, head sessionstore.SessionHead, fence uint64, before func(context.Context) error, payload messaging.PayloadRecord, modelRef governance.VersionedRef) (bool, error) {
	store := w.completionStore()
	if store == nil {
		return true, runtime.ErrCapabilityUnsupported
	}
	// Candidates carry approval and plan evidence; the presence of a public
	// payload is not permission to skip validating that evidence.
	r, err := store.GetCompletion(ctx, e.TenantID, e.RequestID)
	if err == nil {
		err = w.resumeCompletion(ctx, turn, e, head, fence, before, payload, r)
		if completionConflict(err) {
			err = w.commitRecoveryFailure(ctx, turn, e, head, fence, before, modelRef, "completion_conflict")
		}
		return true, err
	}
	if !errors.Is(err, runtime.ErrNotFound) {
		if completionConflict(err) {
			return true, w.commitRecoveryFailure(ctx, turn, e, head, fence, before, modelRef, "completion_conflict")
		}
		return true, err
	}
	results, ok := w.Payloads.(messaging.ResultStore)
	if !ok {
		return true, runtime.ErrCapabilityUnsupported
	}
	frozen, err := results.GetResult(ctx, e.TenantID, e.RequestID)
	if err == nil {
		if err = validateFrozenResult(frozen, e); err == nil && frozen.KeyVersion != payload.KeyVersion {
			err = runtime.ErrVersionMismatch
		}
		if err == nil {
			err = w.commitFrozenSuccess(ctx, turn, e, head, fence, before, frozen.ResultRef)
		}
		if completionConflict(err) {
			err = w.commitRecoveryFailure(ctx, turn, e, head, fence, before, modelRef, "legacy_result_conflict")
		}
		return true, err
	}
	if !errors.Is(err, runtime.ErrNotFound) {
		if completionConflict(err) {
			return true, w.commitRecoveryFailure(ctx, turn, e, head, fence, before, modelRef, "legacy_result_conflict")
		}
		return true, err
	}
	if task := taskflow.FromContext(ctx); task != nil {
		p, err := task.Store.Get(ctx, task.Scope.Key)
		if err != nil {
			return true, err
		}
		if p.Phase == "sealed" {
			return true, w.commitRecoveryFailure(ctx, turn, e, head, fence, before, modelRef, "sealed_result_missing")
		}
	}
	return false, nil
}

// Contradictory immutable evidence is not repairable by generating another
// answer. Preserve the records, record a failed terminal, and advance the gate.
// No suspect result is referenced by a reply Outbox or exposed as history.
func (w RunnerExecutor) commitRecoveryFailure(ctx context.Context, turn *sessionstore.BufferedTurn, e runtime.ExecutionEnvelope, head sessionstore.SessionHead, fence uint64, before func(context.Context) error, modelRef governance.VersionedRef, reason string) error {
	if before != nil {
		if err := before(ctx); err != nil {
			return err
		}
	}
	if task := taskflow.FromContext(ctx); task != nil {
		if _, err := task.Fail(ctx, reason); err != nil {
			return err
		}
	}
	if w.Governance != nil {
		if err := w.Governance.Abort(ctx, e, modelRef, reason); err != nil && !errors.Is(err, runtime.ErrNotFound) {
			return err
		}
		d := governance.Decision{DecisionID: governance.StableDecisionID(e.TenantID, e.RequestID, "recovery", e.PolicyVersion), TenantID: e.TenantID, RequestID: e.RequestID, Stage: "recovery", Action: governance.ActionDeny, ReasonCode: reason, PolicyVersion: e.PolicyVersion}
		if err := w.Governance.Record(ctx, d); err != nil {
			return err
		}
	}
	if turn != nil {
		_ = turn.Rollback(ctx)
	}
	if before != nil {
		if err := before(ctx); err != nil {
			return err
		}
	}
	_, err := w.Sessions.CommitTurn(ctx, sessionstore.CommitTurnRequest{SessionKey: head.SessionKey, RequestID: e.RequestID, CommitID: e.RequestID + ":recovery:" + reason, Stage: "terminal", InputSeq: e.InputSeq, Fence: fence, ExpectedVersion: head.Version, Outcome: runtime.OutcomeFailed, Outbox: []sessionstore.OutboxEvent{terminalAuditOutbox(ctx, e, runtime.OutcomeFailed)}})
	if errors.Is(err, runtime.ErrAlreadyTerminal) {
		return nil
	}
	return err
}
