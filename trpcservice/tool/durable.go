package tool

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/reliability/inflight"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	toolexecution "github.com/liuzengh/trpc-agent-service/trpcservice/tool/execution"
)

func (g GuardedCallable) callDurable(ctx context.Context, args []byte, confirmed bool, results toolexecution.DurableResults) (any, error) {
	execution, _ := runtime.ExecutionContextFrom(ctx)
	if !confirmed && len(args) == 0 {
		args = []byte(`{}`)
	}
	canonical, digest, err := governance.CanonicalArguments(args)
	if err != nil {
		return nil, err
	}
	if execution.PayloadKeyVersion < 1 {
		return nil, runtime.ErrCapabilityUnsupported
	}
	if confirmed {
		if execution.ToolCallID == "" || execution.ArgsDigest != digest || execution.GrantID == "" || execution.GrantVersion < 1 || g.Grants == nil || g.Results == nil {
			return nil, runtime.ErrCapabilityUnsupported
		}
	} else {
		// Model-generated call IDs change on retry. One ordinary operation is
		// identified by tenant/request/tool version/canonical arguments, not a
		// regenerated provider call ID. Intentional distinct effects must carry
		// distinct business arguments/keys.
		identity, _ := json.Marshal([]string{execution.TenantID, execution.RequestID, g.Tool.ID, strconv.FormatInt(g.Tool.Version, 10), digest})
		sum := sha256.Sum256(identity)
		execution.ToolCallID = "op_" + hex.EncodeToString(sum[:])
		execution.ArgsDigest = digest
		ctx = runtime.WithExecutionContext(ctx, execution)
	}
	policy, key := RecoveryManual, ""
	if descriptor, ok := g.Inner.(RecoveryDescriptor); ok {
		policy = descriptor.RecoveryPolicy()
		key, err = descriptor.RecoveryKey(ctx)
		if err != nil {
			return nil, err
		}
	}
	switch policy {
	case RecoveryManual, RecoveryReplaySafe, RecoveryIdempotentKey, RecoveryQueryable:
	default:
		return nil, runtime.ErrCapabilityUnsupported
	}
	if (policy == RecoveryIdempotentKey || policy == RecoveryQueryable) && key == "" {
		return nil, runtime.ErrCapabilityUnsupported
	}
	if key == "" {
		key = execution.TenantID + ":" + execution.RequestID + ":" + execution.ToolCallID
	}
	in := toolexecution.Record{Key: toolexecution.Key{TenantID: execution.TenantID, RequestID: execution.RequestID, ToolCallID: execution.ToolCallID}, ToolID: g.Tool.ID, ToolVersion: g.Tool.Version, ArgsDigest: digest, Policy: string(policy), IdempotencyKey: key}
	if task := taskflow.FromContext(ctx); task != nil {
		var check *taskflow.ResultCheck
		if descriptor, ok := g.Inner.(taskflow.EvidenceDescriptor); ok {
			check = descriptor.TaskEvidenceCheck()
		}
		if err := task.BeforeTool(ctx, in, canonical, check); err != nil {
			return nil, err
		}
	}
	match := func(record toolexecution.Record) bool {
		return record.ToolID == in.ToolID && record.ToolVersion == in.ToolVersion && record.ArgsDigest == in.ArgsDigest && record.Policy == in.Policy && record.IdempotencyKey == in.IdempotencyKey
	}
	reuse := func(record toolexecution.Record) (any, error) {
		content, err := results.LoadResult(ctx, in.Key)
		if err != nil {
			return nil, err
		}
		if record.ResultRef != "tool-result://sha256/"+taskflow.Digest(content) {
			return nil, runtime.ErrVersionMismatch
		}
		if err = verifyToolReceipt(g.Inner, content); err != nil {
			return nil, err
		}
		if confirmed {
			if err = g.mirrorConfirmedResult(ctx, execution, content, record.ResultRef); err != nil {
				return nil, err
			}
		}
		var value any
		decoder := json.NewDecoder(bytes.NewReader(content))
		decoder.UseNumber()
		err = decoder.Decode(&value)
		if err == nil {
			if task := taskflow.FromContext(ctx); task != nil {
				err = task.RecordTool(ctx)
			}
		}
		return value, err
	}
	existing, err := g.Executions.Get(ctx, in.Key)
	if err == nil {
		if !match(existing) {
			return nil, runtime.ErrVersionMismatch
		}
		if existing.State == toolexecution.Succeeded {
			return reuse(existing)
		}
		if existing.State == toolexecution.Failed {
			return nil, runtime.ErrEffectUnknown
		}
	} else if !errors.Is(err, runtime.ErrNotFound) {
		return nil, err
	}
	owner := make([]byte, 16)
	if _, err = rand.Read(owner); err != nil {
		return nil, err
	}
	ttl, renewInterval := g.LeaseTTL, g.RenewInterval
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if renewInterval <= 0 {
		renewInterval = ttl / 3
	}
	if renewInterval <= 0 || renewInterval >= ttl {
		return nil, runtime.ErrInvariantViolation
	}
	running, claimed, err := g.Executions.Claim(ctx, in, toolexecution.Claim{Owner: "tool-" + hex.EncodeToString(owner), TTL: ttl})
	if err != nil {
		return nil, err
	}
	if !claimed {
		return nil, runtime.ErrVersionConflict
	}
	if !match(running) {
		return nil, runtime.ErrVersionMismatch
	}
	callCtx, stop := context.WithCancelCause(ctx)
	renewCtx, stopRenew := context.WithCancel(callCtx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(renewInterval)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				if _, err := g.Executions.Renew(renewCtx, running, ttl); err != nil {
					if renewCtx.Err() != nil {
						return
					}
					stop(runtime.ErrLeaseLost)
					return
				}
			}
		}
	}()
	defer func() { stopRenew(); stop(context.Canceled); <-done }()
	finishState := func(state toolexecution.State, reason string) error {
		stopRenew()
		<-done
		if cause := context.Cause(callCtx); cause != nil {
			return cause
		}
		_, finishErr := g.Executions.Finish(callCtx, running, state, "", reason)
		if finishErr == nil && state == toolexecution.Failed {
			if task := taskflow.FromContext(ctx); task != nil {
				return task.RecordTool(callCtx)
			}
		}
		return finishErr
	}
	resumingConsumed := false
	if confirmed {
		_, err = g.Grants.ConsumeGrant(callCtx, governance.GrantClaim{TenantID: execution.TenantID, GrantID: execution.GrantID, RequestID: execution.RequestID, SubjectID: execution.SubjectID, Tool: g.Tool, ToolCallID: execution.ToolCallID, ArgsDigest: digest, PolicyVersion: execution.PolicyVersion, ExpectedVersion: execution.GrantVersion})
		if err != nil {
			attempt, readErr := g.Grants.GetToolAttempt(callCtx, execution.TenantID, execution.GrantID)
			if !errors.Is(err, runtime.ErrVersionConflict) || readErr != nil || attempt.RequestID != execution.RequestID || attempt.ToolCallID != execution.ToolCallID || attempt.State != governance.ToolAttemptEffectUnknown {
				return nil, err
			}
			resumingConsumed = true
		}
	}
	if (running.Attempt > 1 || resumingConsumed) && policy == RecoveryManual {
		err = finishState(toolexecution.EffectUnknown, "manual recovery required")
		return nil, errors.Join(runtime.ErrEffectUnknown, err)
	}
	var value any
	completed := false
	if lookup, ok := g.Inner.(RecoveryLookup); ok && policy == RecoveryIdempotentKey && lookup.RecoveryLookupEnabled() && (running.Attempt > 1 || resumingConsumed) {
		var observed RecoveryResult
		observed, err = lookup.LookupRecovery(callCtx, key)
		if cause := context.Cause(callCtx); cause != nil {
			return nil, cause
		}
		if err != nil {
			finishErr := finishState(toolexecution.EffectUnknown, "recovery_lookup_error")
			return nil, errors.Join(err, finishErr)
		}
		switch observed.State {
		case RecoveryCompleted:
			value, completed = observed.Result, true
		case RecoveryNotFound:
		case RecoveryPending:
			finishErr := finishState(toolexecution.EffectUnknown, "recovery_pending")
			return nil, errors.Join(ErrRecoveryPending, finishErr)
		case RecoveryFailed:
			finishErr := finishState(toolexecution.Failed, "external_operation_failed")
			return nil, errors.Join(ErrRecoveryFailed, finishErr)
		default:
			finishErr := finishState(toolexecution.EffectUnknown, "recovery_unknown")
			return nil, errors.Join(runtime.ErrEffectUnknown, finishErr)
		}
	}
	if policy == RecoveryQueryable {
		querier, ok := g.Inner.(RecoveryQuerier)
		if !ok {
			return nil, runtime.ErrCapabilityUnsupported
		}
		value, completed, err = querier.QueryRecovery(callCtx, key)
		if err != nil {
			return nil, errors.Join(err, finishState(toolexecution.EffectUnknown, "recovery_lookup_error"))
		}
	}
	if !completed {
		value, err = g.Inner.Call(callCtx, canonical)
	}
	if cause := context.Cause(callCtx); cause != nil {
		return nil, cause
	}
	if err != nil {
		finishErr := finishState(toolexecution.EffectUnknown, "tool call outcome unknown")
		return nil, errors.Join(err, finishErr)
	}
	if err = inflight.Wait(inflight.WithScope(callCtx, execution.TenantID, execution.RequestID, "tool_call:"+execution.ToolCallID), inflight.PointToolEffectBeforeCommit); err != nil {
		return nil, err
	}
	content, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if err = verifyToolReceipt(g.Inner, content); err != nil {
		finishErr := finishState(toolexecution.Failed, "tool_receipt_rejected")
		return nil, errors.Join(err, finishErr)
	}
	// Stop renewal before the atomic success transition: a renewal observing
	// our own 'succeeded' row must not cancel the confirmation-result mirror.
	stopRenew()
	<-done
	if cause := context.Cause(callCtx); cause != nil {
		return nil, cause
	}
	running, err = results.Complete(callCtx, running, content, execution.PayloadKeyVersion)
	if err != nil {
		return nil, err
	}
	if confirmed {
		err = g.mirrorConfirmedResult(callCtx, execution, content, running.ResultRef)
	}
	if err == nil {
		if task := taskflow.FromContext(ctx); task != nil {
			err = task.RecordTool(ctx)
		}
	}
	return value, err
}

func verifyToolReceipt(inner interface{}, content []byte) error {
	if _, _, err := governance.CanonicalArguments(content); err != nil {
		return taskflow.ErrEvidenceRejected
	}
	if d, ok := inner.(taskflow.EvidenceDescriptor); ok {
		return d.TaskEvidenceCheck().Verify(content)
	}
	return nil
}

func (g GuardedCallable) mirrorConfirmedResult(ctx context.Context, execution runtime.ExecutionContext, content []byte, ref string) error {
	sum := sha256.Sum256(content)
	if err := g.Results.PutToolResult(ctx, messaging.ToolResultRecord{TenantID: execution.TenantID, GrantID: execution.GrantID, RequestID: execution.RequestID, ResultRef: ref, ContentDigest: hex.EncodeToString(sum[:]), Content: content, KeyVersion: execution.PayloadKeyVersion}); err != nil {
		return err
	}
	_, err := g.Grants.FinishToolAttempt(ctx, governance.FinishToolAttemptRequest{TenantID: execution.TenantID, GrantID: execution.GrantID, State: governance.ToolAttemptSucceeded, ResultRef: ref})
	return err
}
