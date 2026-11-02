# 在途任务接管与幂等投递 — 实现代码附录

## local_note 业务事实读取与同键结果认证

来源：`trpcservice/tool/localnote/postgres/store.go`，完整文件。本文件完整展示 Create/Lookup 共用的 AES-GCM 认证路径。同键重试返回原密文结果，不能回显未核验的入参；原 key_version 保留后可跨轮换读取。

```go
package postgres

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/localnote"
)

type Store struct {
	DB   *sql.DB
	Keys messaging.PayloadKeyResolver
}

func (s Store) aead(ctx context.Context, tenant string, version int64) (cipher.AEAD, error) {
	if s.Keys == nil || tenant == "" || version < 1 {
		return nil, runtime.ErrCapabilityUnsupported
	}
	key, err := s.Keys.ResolvePayloadKey(ctx, tenant, version)
	if err != nil {
		return nil, err
	}
	defer clear(key.Bytes)
	if key.Version != version || len(key.Bytes) != 32 {
		return nil, runtime.ErrVersionMismatch
	}
	block, err := aes.NewCipher(key.Bytes)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func noteAAD(k localnote.LookupKey, version int64) []byte {
	b, _ := json.Marshal([]any{"local-note-v1", k.TenantID, k.RequestID, k.BusinessKey, k.ArgsDigest, version})
	return b
}
func (s Store) decode(ctx context.Context, k localnote.LookupKey, id string, encrypted, nonce []byte, version int64) (localnote.Result, error) {
	a, err := s.aead(ctx, k.TenantID, version)
	if err != nil {
		return localnote.Result{}, err
	}
	if len(nonce) != a.NonceSize() || len(encrypted) < a.Overhead() || len(encrypted) > 64<<10 {
		return localnote.Result{}, runtime.ErrInvariantViolation
	}
	b, err := a.Open(nil, nonce, encrypted, noteAAD(k, version))
	if err != nil {
		return localnote.Result{}, runtime.ErrInvariantViolation
	}
	defer clear(b)
	var r localnote.Result
	if err = json.Unmarshal(b, &r); err != nil || r.NoteID != id || r.Status != "created" {
		return localnote.Result{}, runtime.ErrVersionMismatch
	}
	return r, nil
}

// Create returns the authenticated EXISTING result on key collision; it must
// not claim success merely by echoing the current caller's desired content.
func (s Store) Create(ctx context.Context, in localnote.Note) (localnote.Result, error) {
	if in.Result.Status != "created" || in.Result.NoteID == "" || len(in.Result.Title) > 200 || len(in.Result.Content) > 4000 {
		return localnote.Result{}, runtime.ErrInvalidEnvelope
	}
	if s.DB == nil || in.TenantID == "" || in.BusinessKey == "" || in.RequestID == "" || in.ArgsDigest == "" || in.KeyVersion < 1 {
		return localnote.Result{}, runtime.ErrCapabilityUnsupported
	}
	a, err := s.aead(ctx, in.TenantID, in.KeyVersion)
	if err != nil {
		return localnote.Result{}, err
	}
	k := localnote.LookupKey{TenantID: in.TenantID, RequestID: in.RequestID, BusinessKey: in.BusinessKey, ArgsDigest: in.ArgsDigest}
	b, err := json.Marshal(in.Result)
	if err != nil {
		return localnote.Result{}, err
	}
	defer clear(b)
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return localnote.Result{}, err
	}
	encrypted := a.Seal(nil, nonce, b, noteAAD(k, in.KeyVersion))
	var request, digest, id string
	var saved, iv []byte
	var version int64
	err = s.DB.QueryRowContext(ctx, `INSERT INTO local_note(tenant_id,business_key,request_id,note_id,args_digest,ciphertext,nonce,key_version)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(tenant_id,business_key) DO UPDATE SET business_key=EXCLUDED.business_key
RETURNING request_id,note_id,args_digest,ciphertext,nonce,key_version`, in.TenantID, in.BusinessKey, in.RequestID, in.Result.NoteID, in.ArgsDigest, encrypted, nonce, in.KeyVersion).Scan(&request, &id, &digest, &saved, &iv, &version)
	if err != nil {
		return localnote.Result{}, err
	}
	if request != in.RequestID || digest != in.ArgsDigest || id != in.Result.NoteID {
		return localnote.Result{}, runtime.ErrIdempotencyCollision
	}
	r, err := s.decode(ctx, k, id, saved, iv, version)
	if err != nil {
		return localnote.Result{}, err
	}
	if r != in.Result {
		return localnote.Result{}, runtime.ErrIdempotencyCollision
	}
	return r, nil
}

func (s Store) Lookup(ctx context.Context, k localnote.LookupKey) (localnote.Result, error) {
	if s.DB == nil {
		return localnote.Result{}, runtime.ErrCapabilityUnsupported
	}
	if k.TenantID == "" || k.RequestID == "" || k.BusinessKey == "" || k.ArgsDigest == "" {
		return localnote.Result{}, runtime.ErrTenantScope
	}
	var request, id, digest string
	var encrypted, nonce []byte
	var version int64
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,note_id,args_digest,ciphertext,nonce,key_version FROM local_note WHERE tenant_id=$1 AND business_key=$2`, k.TenantID, k.BusinessKey).Scan(&request, &id, &digest, &encrypted, &nonce, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return localnote.Result{}, runtime.ErrNotFound
	}
	if err != nil {
		return localnote.Result{}, err
	}
	if request != k.RequestID || digest != k.ArgsDigest {
		return localnote.Result{}, runtime.ErrIdempotencyCollision
	}
	return s.decode(ctx, k, id, encrypted, nonce, version)
}

var _ localnote.Reader = Store{}
```


## MCP 同端点状态查询与回执解码

来源：`trpcservice/tool/mcp/lookup.go`，完整文件。查询不经过写操作 Call；验证原操作键、声明、MCP isError 和固定状态协议，单文本 JSON 保持数字精度。仅支持经配置审核的供应商协议，不证明远端实际上只读。

```go
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	servicetool "github.com/liuzengh/trpc-agent-service/trpcservice/tool"
)

type LookupConfig struct {
	ToolName                  string
	ExpectedDeclarationDigest string
	KeyParameter              string
}

func (c callable) RecoveryLookupEnabled() bool { return c.config.Lookup != nil }

func (c callable) LookupRecovery(ctx context.Context, key string) (servicetool.RecoveryResult, error) {
	lookup := c.config.Lookup
	if lookup == nil {
		return servicetool.RecoveryResult{}, runtime.ErrCapabilityUnsupported
	}
	expected, err := c.RecoveryKey(ctx)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	if key == "" || key != expected {
		return servicetool.RecoveryResult{}, runtime.ErrVersionMismatch
	}
	set, remote, err := c.openNamed(ctx, lookup.ToolName, lookup.ExpectedDeclarationDigest, lookup.KeyParameter)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	defer set.Close()
	args, _ := json.Marshal(map[string]string{lookup.KeyParameter: key})
	r, err := remote.Call(ctx, args)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	if failure, ok := r.(interface{ RetryResultError() bool }); ok && failure.RetryResultError() {
		return servicetool.RecoveryResult{}, runtime.ErrEffectUnknown
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > maxResultBytes {
		return servicetool.RecoveryResult{}, runtime.ErrCapabilityUnsupported
	}
	object, err := decodeJSONObject(b)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	b, err = json.Marshal(object)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	var receipt struct {
		OperationKey string                    `json:"operation_key"`
		State        servicetool.RecoveryState `json:"state"`
		Result       json.RawMessage           `json:"result,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&receipt); err != nil || receipt.OperationKey != key {
		return servicetool.RecoveryResult{}, runtime.ErrVersionMismatch
	}
	if d.Decode(new(any)) != io.EOF {
		return servicetool.RecoveryResult{}, runtime.ErrInvariantViolation
	}
	result := servicetool.RecoveryResult{State: receipt.State}
	switch receipt.State {
	case servicetool.RecoveryCompleted:
		if len(receipt.Result) == 0 || bytes.Equal(bytes.TrimSpace(receipt.Result), []byte("null")) {
			return result, runtime.ErrInvariantViolation
		}
		decoder := json.NewDecoder(bytes.NewReader(receipt.Result))
		decoder.UseNumber()
		if err = decoder.Decode(&result.Result); err != nil {
			return result, err
		}
	case servicetool.RecoveryNotFound, servicetool.RecoveryPending, servicetool.RecoveryFailed, servicetool.RecoveryUnknown:
		if len(receipt.Result) > 0 && !bytes.Equal(bytes.TrimSpace(receipt.Result), []byte("null")) {
			return result, runtime.ErrInvariantViolation
		}
	default:
		return result, runtime.ErrInvariantViolation
	}
	return result, nil
}

// Only an object or exactly one MCP text content block containing an object is
// admitted. Multiple blocks, prose prefixes and malformed JSON are ambiguous.
func decodeJSONObject(b []byte) (map[string]any, error) {
	canonical, _, err := governance.CanonicalArguments(b)
	if err != nil {
		return nil, runtime.ErrInvariantViolation
	}
	b = canonical
	object, err := decodeObject(b)
	if err == nil {
		return object, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &blocks); err != nil || len(blocks) != 1 || blocks[0].Type != "text" {
		return nil, runtime.ErrInvariantViolation
	}
	return decodeObject([]byte(blocks[0].Text))
}

func decodeObject(b []byte) (map[string]any, error) {
	canonical, _, err := governance.CanonicalArguments(b)
	if err != nil {
		return nil, runtime.ErrInvariantViolation
	}
	b = canonical
	var object map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&object); err != nil || object == nil {
		return nil, runtime.ErrInvariantViolation
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, runtime.ErrInvariantViolation
	}
	return object, nil
}

var _ servicetool.RecoveryLookup = callable{}
```


## 恢复判定与异常收敛

来源：`trpcservice/worker/recovery.go`，完整文件。候选优先交叉校验，恢复矛盾以原请求 failed/audit 结束并保留证据；普通依赖失败返回原阶段重试。对应 recovery_state_test.go。

```go
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
```


## 完成候选的实际保存与恢复代码

来源：`trpcservice/worker/completion.go`，完整文件。saveCompletion 在任何结算写入前保存候选，resumeCompletion 校验作用域后重放原治理决定；只允许审核通过的内容晋升公开结果。候选密文的字段及 AAD 见 [设计](design.md#000007完成候选与治理结算恢复)。

```go
package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/liuzengh/trpc-agent-service/trpcservice/gateway"
	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/reliability/inflight"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

type completionCandidate struct {
	SchemaVersion int
	Envelope      runtime.ExecutionEnvelope
	InputDigest   string
	Permit        *governance.RunPermit
	Prepared      *governance.Finalization
	Usage         []governance.ModelUsage
	Output        OutboundContent
	PlanDigest    string
}

func (w RunnerExecutor) completionStore() messaging.CompletionStore {
	if w.Completions != nil {
		return w.Completions
	}
	s, _ := w.Payloads.(messaging.CompletionStore)
	return s
}
func envelopeDigest(e runtime.ExecutionEnvelope) string {
	b, _ := json.Marshal(e)
	return taskflow.Digest(b)
}
func (w RunnerExecutor) saveCompletion(ctx context.Context, e runtime.ExecutionEnvelope, fence uint64, before func(context.Context) error, payload messaging.PayloadRecord, permit governance.RunPermit, result runnerResult, out OutboundContent) (messaging.CompletionRecord, error) {
	s := w.completionStore()
	if s == nil {
		return messaging.CompletionRecord{}, runtime.ErrCapabilityUnsupported
	}
	c := completionCandidate{SchemaVersion: 2, Envelope: e, InputDigest: taskflow.Digest(payload.Content), Usage: runnerModelUsages(permit.Model, result), Output: out}
	if task := taskflow.FromContext(ctx); task != nil {
		p, err := task.ValidateFinal(ctx, out.Content)
		if err != nil {
			return messaging.CompletionRecord{}, err
		}
		c.PlanDigest = completionPlanDigest(p)
	}
	if w.Governance != nil {
		c.Permit = &permit
		if guard, ok := w.Governance.(governance.PreparedRunGuard); ok {
			p, err := guard.PrepareFinalization(ctx, permit, c.Usage, out.Content)
			if err != nil {
				return messaging.CompletionRecord{}, err
			}
			c.Prepared = &p
		}
	}
	b, err := json.Marshal(c)
	if err != nil {
		return messaging.CompletionRecord{}, err
	}
	r := messaging.CompletionRecord{TenantID: e.TenantID, RequestID: e.RequestID, EnvelopeDigest: envelopeDigest(e), ContentDigest: taskflow.Digest(b), Content: b, KeyVersion: payload.KeyVersion, Fence: fence}
	if before != nil {
		if err = before(ctx); err != nil {
			return r, err
		}
	}
	if err = s.PutCompletion(ctx, r); err != nil {
		return r, err
	}
	return r, nil
}
func (w RunnerExecutor) resumeCompletion(ctx context.Context, turn *sessionstore.BufferedTurn, e runtime.ExecutionEnvelope, head sessionstore.SessionHead, fence uint64, before func(context.Context) error, payload messaging.PayloadRecord, r messaging.CompletionRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.TenantID != e.TenantID || r.RequestID != e.RequestID || r.KeyVersion != payload.KeyVersion || r.Fence > fence {
		return runtime.ErrVersionMismatch
	}
	var c completionCandidate
	if err := json.Unmarshal(r.Content, &c); err != nil {
		return runtime.ErrInvariantViolation
	}
	validated, typeErr := validateOutbound(c.Output)
	if (c.SchemaVersion != 1 && c.SchemaVersion != 2) || r.EnvelopeDigest != envelopeDigest(c.Envelope) || c.InputDigest != taskflow.Digest(payload.Content) || verifyAuthoritativeEnvelope(c.Envelope, e) != nil || typeErr != nil || validated.ContentType != c.Output.ContentType {
		return runtime.ErrVersionMismatch
	}
	if task := taskflow.FromContext(ctx); task != nil {
		p, err := task.ValidateFinal(ctx, c.Output.Content)
		if err != nil {
			return err
		}
		if c.SchemaVersion == 2 && c.PlanDigest != completionPlanDigest(p) {
			return runtime.ErrVersionMismatch
		}
	} else if c.SchemaVersion == 2 && c.PlanDigest != "" {
		return runtime.ErrVersionMismatch
	}
	ref, err := encodeResultRef(ctx, w.EncodeResult, e, string(c.Output.Content))
	if err != nil {
		return err
	}
	results, ok := w.Payloads.(messaging.ResultStore)
	if !ok {
		return runtime.ErrCapabilityUnsupported
	}
	frozen, frozenErr := results.GetResult(ctx, e.TenantID, e.RequestID)
	if frozenErr == nil {
		if err = validateFrozenResult(frozen, e); err != nil {
			return err
		}
		if frozen.KeyVersion != payload.KeyVersion || frozen.ResultRef != ref || frozen.ContentType != c.Output.ContentType || !bytes.Equal(frozen.Content, c.Output.Content) {
			return runtime.ErrIdempotencyCollision
		}
	} else if !errors.Is(frozenErr, runtime.ErrNotFound) {
		return frozenErr
	}
	latest, err := w.Tasks.GetExecution(ctx, gateway.ExecutionKey{TenantID: e.TenantID, RequestID: e.RequestID})
	if err != nil {
		return err
	}
	if verifyAuthoritativeEnvelope(latest.Envelope, e) != nil {
		return runtime.ErrVersionMismatch
	}
	if latest.CancelRequested {
		return runtime.ErrCancelRequested
	}
	if before != nil {
		if err = before(ctx); err != nil {
			return err
		}
	}
	if w.Governance != nil {
		if c.Permit == nil || c.Permit.Policy.TenantID != e.TenantID || c.Permit.Policy.Version != e.PolicyVersion || c.Permit.Reservation.TenantID != e.TenantID || c.Permit.Reservation.RequestID != e.RequestID || c.Permit.Decision.Action != governance.ActionAllow {
			return runtime.ErrVersionMismatch
		}
		var decision governance.Decision
		if c.Prepared != nil {
			guard, ok := w.Governance.(governance.PreparedRunGuard)
			if !ok || c.Prepared.OutputDigest != taskflow.Digest(c.Output.Content) || c.Prepared.Decision.TenantID != e.TenantID || c.Prepared.Decision.RequestID != e.RequestID || c.Prepared.Decision.PolicyVersion != e.PolicyVersion || c.Prepared.Decision.ReservationID != c.Permit.Reservation.ReservationID || c.Prepared.Settlement.ExpectedVersion != c.Permit.Reservation.Version {
				return runtime.ErrVersionMismatch
			}
			if err = c.Prepared.Validate(*c.Permit, c.Usage, c.Output.Content); err != nil {
				return err
			}
			if frozenErr == nil && c.Prepared.Decision.Action != governance.ActionAllow {
				return runtime.ErrInvariantViolation
			}
			decision, err = guard.CommitFinalization(ctx, *c.Prepared)
		} else {
			if _, ok := w.Governance.(governance.PreparedRunGuard); ok {
				return runtime.ErrVersionMismatch
			}
			if multi, ok := w.Governance.(governance.MultiModelRunGuard); ok {
				decision, err = multi.FinishModels(ctx, *c.Permit, c.Usage, c.Output.Content)
			} else if len(c.Usage) == 1 {
				decision, err = w.Governance.Finish(ctx, *c.Permit, c.Usage[0].Usage, c.Output.Content)
			} else {
				return runtime.ErrCapabilityUnsupported
			}
		}
		if err != nil {
			return err
		}
		if decision.Action != governance.ActionAllow {
			if task := taskflow.FromContext(ctx); task != nil {
				return w.commitTaskFailure(ctx, task, turn, e, head, fence, before, c.Permit.Model, payload.KeyVersion, "task_output_rejected")
			}
			return w.commitGovernanceTerminal(ctx, turn, e, head, fence, before, runtime.OutcomeDenied, decision)
		}
	} else if c.Permit != nil || c.Prepared != nil {
		return runtime.ErrVersionMismatch
	}
	if w.Barrier != nil {
		if err = w.Barrier.Wait(ctx, inflight.PointP2BeforeTerminalCommit); err != nil {
			return err
		}
	}
	if err = results.PutResult(ctx, messaging.ResultRecord{TenantID: e.TenantID, RequestID: e.RequestID, ResultRef: ref, ContentDigest: taskflow.Digest(c.Output.Content), Content: c.Output.Content, ContentType: c.Output.ContentType, KeyVersion: payload.KeyVersion}); err != nil {
		return err
	}
	if w.Barrier != nil {
		if err = w.Barrier.Wait(ctx, inflight.PointResultPersistedBeforeCommit); err != nil {
			return err
		}
	}
	return w.commitFrozenSuccess(ctx, turn, e, head, fence, before, ref)
}

// Fence/version/phase change during adoption/sealing; declared obligations and
// their evidence must remain identical to those reviewed with this candidate.
func completionPlanDigest(p taskflow.Plan) string {
	if p.Acceptance != nil {
		b, _ := json.Marshal(struct {
			InputDigest string
			Contract    taskflow.Contract
			Steps       []taskflow.Step
			Acceptance  *taskflow.Acceptance
		}{p.InputDigest, p.Contract, p.Steps, p.Acceptance})
		return taskflow.Digest(b)
	}
	b, _ := json.Marshal(struct {
		InputDigest string
		Contract    taskflow.Contract
		Steps       []taskflow.Step
	}{p.InputDigest, p.Contract, p.Steps})
	return taskflow.Digest(b)
}
```

可执行证据：`worker/completion_test.go` 覆盖结算前后崩溃、旧资格拒绝和输出拒绝；`storage/messaging/postgres/completion_test.go` 覆盖 AAD 替换与数据库旧 Fence 拒绝。


本篇代码从当前工作区源码逐字提取；省略的文件 import/其他函数以明确的节选说明表示。每段均给出状态变化与故障含义。示例身份、测试时延与状态不是运行结果。

## 普通与确认工具统一持久化

来源：`trpcservice/tool/durable.go`；operation ID、恢复策略、工具 lease 和成功结果复用适用于 allow+ask；grant 不二次消费。

```go
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
```

## 工具 claim/renew 的强制条件

来源：`trpcservice/tool/execution/postgres/store.go`；只有可恢复状态、不可变字段一致和有效租期的 owner 可推进；每次领取提高 fence/attempt。

```go
func (s *Store) Claim(ctx context.Context, in execution.Record, claim execution.Claim) (execution.Record, bool, error) {
	if s == nil || s.db == nil || in.TenantID == "" || in.RequestID == "" || in.ToolCallID == "" || in.ToolID == "" || in.ToolVersion < 1 || in.ArgsDigest == "" || in.IdempotencyKey == "" || claim.Owner == "" || claim.TTL <= 0 {
		return execution.Record{}, false, runtime.ErrInvariantViolation
	}
	var r execution.Record
	var fence int64
	var lease sql.NullTime
	err := s.db.QueryRowContext(ctx, `INSERT INTO tool_execution(tenant_id,request_id,tool_call_id,tool_id,tool_version,args_digest,recovery_policy,idempotency_key,state,attempt,fence,lease_owner,lease_until)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,'running',1,1,$9,now()+$10::interval)
ON CONFLICT (tenant_id,request_id,tool_call_id) DO UPDATE SET state='running',attempt=tool_execution.attempt+1,fence=tool_execution.fence+1,lease_owner=EXCLUDED.lease_owner,lease_until=EXCLUDED.lease_until,updated_at=now()
WHERE (tool_execution.state IN ('pending','effect_unknown') OR (tool_execution.state='running' AND tool_execution.lease_until<now()))
AND tool_execution.tool_id=EXCLUDED.tool_id AND tool_execution.tool_version=EXCLUDED.tool_version
AND tool_execution.args_digest=EXCLUDED.args_digest AND tool_execution.recovery_policy=EXCLUDED.recovery_policy
AND tool_execution.idempotency_key=EXCLUDED.idempotency_key
RETURNING tenant_id,request_id,tool_call_id,tool_id,tool_version,args_digest,recovery_policy,idempotency_key,state,attempt,fence,lease_owner,lease_until,COALESCE(external_handle,''),COALESCE(result_ref,''),COALESCE(last_error,'')`,
		in.TenantID, in.RequestID, in.ToolCallID, in.ToolID, in.ToolVersion, in.ArgsDigest, in.Policy, in.IdempotencyKey, claim.Owner, claim.TTL.String()).Scan(&r.TenantID, &r.RequestID, &r.ToolCallID, &r.ToolID, &r.ToolVersion, &r.ArgsDigest, &r.Policy, &r.IdempotencyKey, &r.State, &r.Attempt, &fence, &r.LeaseOwner, &lease, &r.ExternalHandle, &r.ResultRef, &r.LastError)
	if err == sql.ErrNoRows {
		return in, false, nil
	}
	if err != nil {
		return execution.Record{}, false, err
	}
	r.Fence = uint64(fence)
	if lease.Valid {
		r.LeaseUntil = lease.Time
	}
	return r, true, nil
}

func (s *Store) Renew(ctx context.Context, in execution.Record, ttl time.Duration) (execution.Record, error) {
	if s == nil || s.db == nil || in.LeaseOwner == "" || in.Fence == 0 || ttl <= 0 {
		return execution.Record{}, runtime.ErrInvariantViolation
	}
	res, err := s.db.ExecContext(ctx, `UPDATE tool_execution SET lease_until=now()+$1::interval,updated_at=now() WHERE tenant_id=$2 AND request_id=$3 AND tool_call_id=$4 AND state='running' AND lease_owner=$5 AND fence=$6 AND lease_until>now()`, ttl.String(), in.TenantID, in.RequestID, in.ToolCallID, in.LeaseOwner, int64(in.Fence))
	if err != nil {
		return execution.Record{}, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return execution.Record{}, runtime.ErrVersionConflict
	}
	return s.Get(ctx, in.Key)
}
```

## 结果与 succeeded 同事务

来源：`trpcservice/tool/execution/postgres/results.go`；Complete 验证 owner/fence/lease；密文插入和成功状态要么一起提交，要么一起回滚。

```go
func (s *Store) Complete(ctx context.Context, in execution.Record, content []byte, version int64) (execution.Record, error) {
	if len(content) == 0 || version < 1 || in.Fence == 0 || in.LeaseOwner == "" {
		return in, runtime.ErrInvalidEnvelope
	}
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	ref := "tool-result://sha256/" + digest
	aead, err := s.aead(ctx, in.TenantID, version)
	if err != nil {
		return in, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return in, err
	}
	encrypted := aead.Seal(nil, nonce, content, resultAAD(in.Key, digest, version))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return in, err
	}
	defer tx.Rollback()
	var fence int64
	err = tx.QueryRowContext(ctx, `UPDATE tool_execution SET state='succeeded',result_ref=$6,last_error=NULL,lease_owner=NULL,lease_until=NULL,updated_at=now()
WHERE tenant_id=$1 AND request_id=$2 AND tool_call_id=$3 AND state='running' AND lease_owner=$4 AND fence=$5 AND lease_until>now() RETURNING fence`, in.TenantID, in.RequestID, in.ToolCallID, in.LeaseOwner, int64(in.Fence), ref).Scan(&fence)
	if errors.Is(err, sql.ErrNoRows) {
		return in, runtime.ErrLeaseLost
	}
	if err != nil {
		return in, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO tool_execution_result(tenant_id,request_id,tool_call_id,result_ref,content_digest,ciphertext,nonce,key_version) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, in.TenantID, in.RequestID, in.ToolCallID, ref, digest, encrypted, nonce, version)
	if err != nil {
		return in, err
	}
	if err = tx.Commit(); err != nil {
		return in, err
	}
	in.State, in.ResultRef, in.LeaseOwner = execution.Succeeded, ref, ""
	return in, nil
}

func (s *Store) LoadResult(ctx context.Context, key execution.Key) ([]byte, error) {
	var digest string
	var encrypted, nonce []byte
	var version int64
	err := s.db.QueryRowContext(ctx, `SELECT r.content_digest,r.ciphertext,r.nonce,r.key_version FROM tool_execution_result r JOIN tool_execution e USING(tenant_id,request_id,tool_call_id) WHERE r.tenant_id=$1 AND r.request_id=$2 AND r.tool_call_id=$3 AND e.state='succeeded' AND e.result_ref=r.result_ref`, key.TenantID, key.RequestID, key.ToolCallID).Scan(&digest, &encrypted, &nonce, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runtime.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	aead, err := s.aead(ctx, key.TenantID, version)
	if err != nil {
		return nil, err
	}
	if len(nonce) != aead.NonceSize() {
		return nil, runtime.ErrInvariantViolation
	}
	content, err := aead.Open(nil, nonce, encrypted, resultAAD(key, digest, version))
	if err != nil {
		return nil, runtime.ErrInvariantViolation
	}
	sum := sha256.Sum256(content)
	if hex.EncodeToString(sum[:]) != digest {
		clear(content)
		return nil, runtime.ErrVersionMismatch
	}
	return content, nil
}
```

## Worker 尝试独立记录

来源：`trpcservice/gateway/postgres/attempts.go`；request 行锁串行化日志；HTTP 请求计数不同于上游接受。

```go
func (s *TaskStore) BeginExecutionAttempt(ctx context.Context, key gateway.ExecutionKey, worker string, fence uint64) (gateway.ExecutionAttempt, error) {
	a := gateway.ExecutionAttempt{ExecutionKey: key, WorkerID: worker, Fence: fence}
	if key.TenantID == "" || key.RequestID == "" || worker == "" || fence == 0 || fence > math.MaxInt64 {
		return a, runtime.ErrInvalidEnvelope
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return a, err
	}
	defer tx.Rollback()
	var request string
	if err = tx.QueryRowContext(ctx, `SELECT request_id FROM execution_record WHERE tenant_id=$1 AND request_id=$2 FOR UPDATE`, key.TenantID, key.RequestID).Scan(&request); err != nil {
		return a, err
	}
	var maximum int64
	if err = tx.QueryRowContext(ctx, `SELECT COALESCE(max(fence),0) FROM execution_attempt WHERE tenant_id=$1 AND request_id=$2`, key.TenantID, key.RequestID).Scan(&maximum); err != nil {
		return a, err
	}
	if uint64(maximum) > fence {
		return a, runtime.ErrStaleFence
	}
	if _, err = tx.ExecContext(ctx, `UPDATE execution_attempt SET state='superseded',finished_at=now() WHERE tenant_id=$1 AND request_id=$2 AND state='running'`, key.TenantID, key.RequestID); err != nil {
		return a, err
	}
	err = tx.QueryRowContext(ctx, `INSERT INTO execution_attempt(tenant_id,request_id,attempt_no,worker_id,fence,state)
SELECT $1,$2,COALESCE(max(attempt_no),0)+1,$3,$4,'running' FROM execution_attempt WHERE tenant_id=$1 AND request_id=$2 RETURNING attempt_no`, key.TenantID, key.RequestID, worker, int64(fence)).Scan(&a.Number)
	if err != nil {
		return a, err
	}
	return a, tx.Commit()
}

func (s *TaskStore) FinishExecutionAttempt(ctx context.Context, a gateway.ExecutionAttempt, completed bool) error {
	state := "retryable"
	if completed {
		state = "completed"
	}
	var version int64
	err := s.db.QueryRowContext(ctx, `UPDATE execution_attempt SET state=$6,finished_at=now() WHERE tenant_id=$1 AND request_id=$2 AND attempt_no=$3 AND worker_id=$4 AND fence=$5 AND state='running' RETURNING attempt_no`, a.TenantID, a.RequestID, a.Number, a.WorkerID, int64(a.Fence), state).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.ErrVersionConflict
	}
	return err
}

func (s *TaskStore) RecordModelRequest(ctx context.Context, key gateway.ExecutionKey) error {
	if key.TenantID == "" || key.RequestID == "" {
		return runtime.ErrTenantScope
	}
	result, err := s.db.ExecContext(ctx, `UPDATE execution_record SET model_request_count=model_request_count+1 WHERE tenant_id=$1 AND request_id=$2`, key.TenantID, key.RequestID)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return runtime.ErrNotFound
	}
	return nil
}
```

## 发送接受后的确认及不确定核对

来源：`trpcservice/channels/delivery/service.go`；只有可信查询结果允许重试；unknown 有界核对后进入 failed/reconcile_exhausted。

```go
func (s Service) reconcile(ctx context.Context, event channel.ReplyEvent, record messaging.DeliveryRecord) error {
	if record.NotBefore.After(time.Now()) {
		return DeferredError{NotBefore: record.NotBefore}
	}
	adapter, err := s.resolveAdapter(ctx, event)
	if err != nil {
		return s.deferReconciliation(ctx, record, err)
	}
	reconciler, ok := adapter.(channel.DeliveryReconciler)
	if !ok {
		return s.deferReconciliation(ctx, record, runtime.ErrCapabilityUnsupported)
	}
	result, err := reconciler.ReconcileDelivery(ctx, channel.ReconciliationRequest{Event: event, ClientRequestID: record.ClientRequestID})
	if err != nil {
		return s.deferReconciliation(ctx, record, err)
	}
	switch result.Status {
	case channel.ReconciliationDelivered:
		if result.ProviderMessageID == "" {
			return s.deferReconciliation(ctx, record, runtime.ErrInvariantViolation)
		}
		record.State, record.ProviderMessageID, record.LastErrorClass = messaging.DeliverySent, result.ProviderMessageID, ""
		_, err = s.Ledger.ReconcileDelivery(ctx, record, record.Version)
		return err
	case channel.ReconciliationNotDelivered:
		record.State, record.NotBefore, record.LastErrorClass = messaging.DeliveryRetryWait, time.Now().UTC(), "reconciled_not_delivered"
		if _, err = s.Ledger.ReconcileDelivery(ctx, record, record.Version); err != nil {
			return err
		}
		return DeferredError{NotBefore: record.NotBefore}
	case channel.ReconciliationUnknown:
		return s.deferReconciliation(ctx, record, runtime.ErrCommitConflict)
	default:
		return s.deferReconciliation(ctx, record, runtime.ErrInvariantViolation)
	}
}

func (s Service) deferReconciliation(ctx context.Context, record messaging.DeliveryRecord, cause error) error {
	nextAttempt := record.ReconcileAttempt + 1
	if nextAttempt >= s.maxReconcileAttempts() {
		return s.finishFailed(ctx, record, cause, "reconcile_exhausted", true)
	}
	record.ReconcileAttempt = nextAttempt
	record.NotBefore = time.Now().Add(s.backoff(nextAttempt, 0))
	record.LastErrorClass = "reconcile_retryable"
	updated, err := s.Ledger.DeferDeliveryReconciliation(ctx, record, record.Version)
	if err != nil {
		return errors.Join(cause, err)
	}
	return DeferredError{NotBefore: updated.NotBefore}
}

func (s Service) finishFailed(ctx context.Context, record messaging.DeliveryRecord, cause error, class string, reconcile bool) error {
	record.State = messaging.DeliveryFailed
	record.LastErrorClass = normalizeErrorClass(class)
	var err error
	if reconcile {
		_, err = s.Ledger.ReconcileDelivery(ctx, record, record.Version)
	} else {
		_, err = s.Ledger.FinishDelivery(ctx, record, record.Version)
	}
	if err != nil {
		return errors.Join(cause, err)
	}
	return TerminalError{Err: cause}
}
```

## 人工处置不重跑任务

来源：`trpcservice/storage/messaging/postgres/operations.go`；expected_version 与状态限制防并发冲突，审计和 Outbox 重调度同事务。

```go
func (s *Store) ResolveUncertainDelivery(ctx context.Context, in DeliveryResolution) error {
	if err := in.Validate(); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	state, class := "sent", "operator_confirmed_delivered"
	if in.Decision == "not_delivered" {
		state, class = "failed", "operator_confirmed_not_delivered"
	}
	if in.Decision == "retry_not_delivered" {
		state, class = "retry_wait", "operator_confirmed_not_delivered"
	}
	var version int64
	err = tx.QueryRowContext(ctx, `UPDATE delivery_ledger
SET state=$5,provider_message_id=NULLIF($6,''),last_error_class=$7,not_before=now(),version=version+1,updated_at=now()
WHERE tenant_id=$1 AND delivery_key=$2 AND segment_no=$3 AND version=$4
AND (state='ambiguous' OR (state='failed' AND last_error_class='reconcile_exhausted'))
RETURNING version`, in.TenantID, in.DeliveryKey, in.SegmentNo, in.ExpectedVersion, state, in.ProviderMessageID, class).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.ErrVersionConflict
	}
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO delivery_resolution
(tenant_id,delivery_key,segment_no,expected_version,actor,evidence_ref,decision,provider_message_id)
VALUES($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''))`, in.TenantID, in.DeliveryKey, in.SegmentNo, in.ExpectedVersion, in.Actor, in.EvidenceRef, in.Decision, in.ProviderMessageID)
	if err != nil {
		return err
	}
	if in.Decision != "not_delivered" {
		// The Redis entry may already have been ACKed on reconcile exhaustion.
		// Republish the original frozen reply with its original delivery key;
		// sent segments remain ACK-only, including the externally confirmed one.
		var outboxID string
		err = tx.QueryRowContext(ctx, `UPDATE outbox SET state='pending',claim_owner=NULL,claim_until=NULL,
published_at=NULL,next_attempt_at=now(),version=version+1
WHERE tenant_id=$1 AND kind='reply' AND idempotency_key=$2 AND state<>'claimed'
RETURNING outbox_id`, in.TenantID, in.DeliveryKey).Scan(&outboxID)
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.ErrVersionConflict
		}
		if err != nil {
			return err
		}
	}
	return tx.Commit()
}
```

## 结论与证据回查

字段及约束见 [design.md](design.md)，故障判定见 [testing-and-acceptance.md](testing-and-acceptance.md)。代码只能证明条件更新和恢复分支存在，真实渠道可见次数仍需客户端证据，不能从 Ledger 一行推断下游接收次数。

## 把代码条件翻译成恢复判据

| 代码条件 | 状态含义 | 审查时的反问 |
|---|---|---|
| tool执行不可变字段match | 同逻辑操作的工具版本/参数/恢复键不漂移 | 如果模型新call ID不同，是否仍能命中普通operation？ |
| Claim只选可恢复状态 | 有效running owner不被抢占 | 两个进程能否同时得到claimed=true？ |
| Complete验证有效租期 | 过期owner不写平台成功结果 | 外部已生效时如何通过业务键恢复？ |
| LoadResult解密并校验摘要 | 结果不能跨tenant/request/call替换 | 旧密钥不可读时是否错误地重跑？ |
| Ledger对ambiguous调用核对 | 未知不当未送达 | adapter没有查询能力时是否盲重发？ |
| Resolve事务带expected_version | 管理员与自动核对不能互相覆盖 | Outbox被领取时是否完整回滚？ |

## 从成功到确认之间的四个失败点

工具Complete之前失败，依靠工具恢复策略；Complete后镜像失败，复用主结果；最终PutResult后terminal失败，复用冻结答案；provider接受后Ledger失败，进入P4核对。它们不能由一个“重试整个函数”策略覆盖。

代码中的错误返回也有不同含义：ErrVersionConflict可能是并发推进，ErrLeaseLost表示已无执行资格，ErrEffectUnknown表示外部效果无法安全确定，TerminalError表示发送失败已durable可确认队列。读代码时应同时看调用方如何处理，而不是把非nil都当成同一种重试。

## P4 持久回执的数据库证据

来源：`trpcservice/storage/messaging/postgres/receipt.go`，完整文件。插入与 Ledger 行锁在同一 SQL 中，过期执行权不能追加证据；冲突只可核对原值，不能覆盖旧回执。回执通过 tenant/key/segment 关联且校验计划、路由和实际发送字节；恢复算法见参考实现的 Service 完整源码。

```go
package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
)

func (s *Store) PutProviderReceipt(ctx context.Context, owner messaging.DeliveryRecord, r messaging.ProviderReceipt) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if owner.DeliveryKey != r.DeliveryKey || owner.ClientRequestID != r.ClientRequestID || r.PlanDigest != messaging.ReceiptDigest(owner.Plan) || owner.ClaimOwner == "" {
		return runtime.ErrVersionMismatch
	}
	var stored messaging.ProviderReceipt
	stored.DeliveryKey = r.DeliveryKey
	err := s.db.QueryRowContext(ctx, `INSERT INTO delivery_receipt(tenant_id,delivery_key,segment_no,client_request_id,provider_message_id,plan_digest,route_digest,segment_digest)
SELECT $1,$2,$3,$4,$5,$6,$7,$10 FROM delivery_ledger
WHERE tenant_id=$1 AND delivery_key=$2 AND segment_no=$3 AND state='sending' AND client_request_id=$4
AND version=$8 AND claim_owner=$9 AND claim_until>now() FOR UPDATE
ON CONFLICT(tenant_id,delivery_key,segment_no) DO UPDATE SET delivery_key=EXCLUDED.delivery_key
RETURNING client_request_id,provider_message_id,plan_digest,route_digest,segment_digest`, r.TenantID, r.DeliveryKey.DeliveryKey, r.SegmentNo, r.ClientRequestID, r.ProviderMessageID, r.PlanDigest, r.RouteDigest, owner.Version, owner.ClaimOwner, r.SegmentDigest).Scan(&stored.ClientRequestID, &stored.ProviderMessageID, &stored.PlanDigest, &stored.RouteDigest, &stored.SegmentDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.ErrLeaseLost
	}
	if err != nil {
		return err
	}
	if stored.ClientRequestID != r.ClientRequestID || stored.ProviderMessageID != r.ProviderMessageID || stored.PlanDigest != r.PlanDigest || stored.RouteDigest != r.RouteDigest || stored.SegmentDigest != r.SegmentDigest {
		return runtime.ErrIdempotencyCollision
	}
	return nil
}
func (s *Store) GetProviderReceipt(ctx context.Context, k messaging.DeliveryKey) (messaging.ProviderReceipt, error) {
	r := messaging.ProviderReceipt{DeliveryKey: k}
	if k.TenantID == "" || k.DeliveryKey == "" || k.SegmentNo < 0 {
		return r, runtime.ErrTenantScope
	}
	err := s.db.QueryRowContext(ctx, `SELECT client_request_id,provider_message_id,plan_digest,route_digest,segment_digest,created_at FROM delivery_receipt WHERE tenant_id=$1 AND delivery_key=$2 AND segment_no=$3`, k.TenantID, k.DeliveryKey, k.SegmentNo).Scan(&r.ClientRequestID, &r.ProviderMessageID, &r.PlanDigest, &r.RouteDigest, &r.SegmentDigest, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return r, runtime.ErrNotFound
	}
	if err != nil {
		return r, err
	}
	return r, r.Validate()
}
```

## 完成条件代码：trpcservice/taskflow/acceptance.go

来源：`trpcservice/taskflow/acceptance.go`，完整文件。字段与边界见 design；最终判定针对冻结合同和实际输出，不依赖模型自报完成。

```go
package taskflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

// Requirement is supplied by trusted application code, never checkpoint arguments.
// Literal and receipt checks are deterministic constraints, not semantic graders.
type Requirement struct {
	ID             string   `json:"id"`
	Description    string   `json:"description"`
	ToolID         string   `json:"tool_id,omitempty"`
	ToolVersion    int64    `json:"tool_version,omitempty"`
	OutputContains []string `json:"output_contains,omitempty"`
	EvidencePath   []string `json:"evidence_path,omitempty"`
}

type Acceptance struct {
	Version      int           `json:"version"`
	InputDigest  string        `json:"input_digest"`
	Objective    string        `json:"objective"`
	Requirements []Requirement `json:"requirements"`
}

func (a Acceptance) Validate() error {
	if a.Version != 1 || len(a.InputDigest) != 64 || strings.TrimSpace(a.Objective) == "" || len(a.Objective) > 64<<10 || len(a.Requirements) > 32 {
		return runtime.ErrInvalidEnvelope
	}
	seen := map[string]bool{}
	for _, r := range a.Requirements {
		if !validID(r.ID) || seen[r.ID] || strings.TrimSpace(r.Description) == "" || len(r.Description) > 1024 || len(r.ToolID) > 256 || (r.ToolID == "") != (r.ToolVersion == 0) || r.ToolVersion < 0 || len(r.OutputContains) > 16 || len(r.EvidencePath) > 8 || (len(r.EvidencePath) > 0 && r.ToolID == "") {
			return runtime.ErrInvalidEnvelope
		}
		seen[r.ID] = true
		for _, v := range r.OutputContains {
			if strings.TrimSpace(v) == "" || len(v) > 4096 {
				return runtime.ErrInvalidEnvelope
			}
		}
		for _, v := range r.EvidencePath {
			if v == "" || len(v) > 128 {
				return runtime.ErrInvalidEnvelope
			}
		}
	}
	return nil
}

// BindAcceptance freezes the original objective before planning. An adopted
// legacy plan must not acquire new obligations retroactively.
func (s *Session) BindAcceptance(ctx context.Context, a Acceptance) (Plan, error) {
	p, err := s.load(ctx)
	if err != nil {
		return p, err
	}
	if err = a.Validate(); err != nil {
		return p, err
	}
	if a.InputDigest != p.InputDigest {
		return p, runtime.ErrVersionMismatch
	}
	if p.Acceptance != nil {
		x, _ := json.Marshal(p.Acceptance)
		y, _ := json.Marshal(a)
		if string(x) != string(y) {
			return p, runtime.ErrIdempotencyCollision
		}
		return p, nil
	}
	if p.Phase != "open" || p.Attempts != 0 || len(p.Steps) != 0 {
		return p, runtime.ErrVersionConflict
	}
	// Copy caller-owned slices before the store receives the contract.
	b, _ := json.Marshal(a)
	var frozen Acceptance
	_ = json.Unmarshal(b, &frozen)
	p.Acceptance = &frozen
	return s.save(ctx, p)
}

func validateStepRequirements(a *Acceptance, st Step) error {
	seen := map[string]bool{}
	for _, id := range st.RequirementIDs {
		if a == nil || seen[id] {
			return runtime.ErrInvalidEnvelope
		}
		seen[id] = true
		found := false
		for _, r := range a.Requirements {
			if r.ID == id {
				found = true
				if r.ToolID != "" && (st.Kind != "tool" || st.ToolID != r.ToolID || st.ToolVersion != r.ToolVersion) {
					return ErrEvidenceRequired
				}
			}
		}
		if !found {
			return runtime.ErrInvalidEnvelope
		}
	}
	return nil
}

func validateCoverage(p Plan) error {
	if p.Acceptance == nil {
		return nil
	}
	if err := p.Acceptance.Validate(); err != nil {
		return err
	}
	if p.Acceptance.InputDigest != p.InputDigest {
		return runtime.ErrVersionMismatch
	}
	covered := map[string]bool{}
	for _, st := range p.Steps {
		if err := validateStepRequirements(p.Acceptance, st); err != nil {
			return err
		}
		for _, id := range st.RequirementIDs {
			covered[id] = true
		}
	}
	for _, r := range p.Acceptance.Requirements {
		if !covered[r.ID] {
			return fmt.Errorf("missing required deliverable %s: %w", r.ID, ErrIncomplete)
		}
	}
	return nil
}

// ValidateFinal rechecks authoritative receipts, coverage and the actual rendered
// output before candidate persistence and again before recovery/terminal commit.
func (s *Session) ValidateFinal(ctx context.Context, content []byte) (p Plan, err error) {
	defer func() {
		if err != nil && errors.Is(err, ErrIncomplete) && p.Phase == "open" && p.Acceptance != nil && p.CompletionIssue != err.Error() {
			p.CompletionIssue = err.Error()
			if len(p.CompletionIssue) > 1024 {
				p.CompletionIssue = p.CompletionIssue[:1024]
			}
			saved, saveErr := s.save(ctx, p)
			if saveErr != nil {
				err = saveErr
			} else {
				p = saved
			}
		}
	}()
	p, err = s.ValidateCompletion(ctx)
	if err != nil {
		return p, err
	}
	if p.Acceptance == nil {
		return p, nil
	}
	if strings.TrimSpace(string(content)) == "" {
		return p, fmt.Errorf("empty final deliverable: %w", ErrIncomplete)
	}
	if p.Phase == "sealed" && p.FinalDigest != Digest(content) {
		return p, runtime.ErrVersionMismatch
	}
	for _, r := range p.Acceptance.Requirements {
		for _, text := range r.OutputContains {
			if !strings.Contains(string(content), text) {
				return p, fmt.Errorf("final answer missing required content for %s: %w", r.ID, ErrIncomplete)
			}
		}
		if len(r.EvidencePath) == 0 {
			continue
		}
		matched := false
		for _, st := range p.Steps {
			if !containsID(st.RequirementIDs, r.ID) {
				continue
			}
			_, b, e := s.verifyTool(ctx, st, p.Contract.RequireToolChecks)
			if e != nil {
				return p, e
			}
			v, e := decodeJSON(b)
			if e != nil {
				return p, ErrEvidenceRejected
			}
			for _, path := range r.EvidencePath {
				obj, ok := v.(map[string]any)
				if !ok {
					v = nil
					break
				}
				v = obj[path]
			}
			var text string
			switch x := v.(type) {
			case string:
				text = x
			case json.Number:
				text = x.String()
			}
			if strings.TrimSpace(text) != "" && strings.Contains(string(content), text) {
				matched = true
			}
		}
		if !matched {
			return p, fmt.Errorf("final answer lacks verified receipt value for %s: %w", r.ID, ErrIncomplete)
		}
	}
	return p, nil
}

func containsID(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}
```

## 完成条件代码：trpcservice/worker/acceptance.go

来源：`trpcservice/worker/acceptance.go`，完整文件。字段与边界见 design；最终判定针对冻结合同和实际输出，不依赖模型自报完成。

```go
package worker

import (
	"context"
	"strings"
	"unicode/utf8"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	"trpc.group/trpc-go/trpc-agent-go/model"
)

// TaskRequirementResolver derives reviewed business acceptance conditions from
// the pinned execution configuration/input. It runs only before first planning;
// takeover reuses the frozen contract, even when configuration has changed.
type TaskRequirementResolver func(context.Context, runtime.ExecutionEnvelope, model.Message) ([]taskflow.Requirement, error)

func (w RunnerExecutor) bindTaskAcceptance(ctx context.Context, task *taskflow.Session, e runtime.ExecutionEnvelope, message model.Message, policy map[string]any) error {
	p, err := task.Store.Get(ctx, task.Scope.Key)
	if err != nil {
		return err
	}
	if p.Acceptance != nil {
		if p.Acceptance.InputDigest != p.InputDigest {
			return runtime.ErrVersionMismatch
		}
		return p.Acceptance.Validate()
	}
	// Retain the interpretation of candidates and plans created before this feature.
	if p.Phase != "open" || p.Attempts != 0 || len(p.Steps) != 0 {
		return nil
	}
	objective := message.Content
	if strings.TrimSpace(objective) == "" {
		objective = "Respond to the original persisted multimodal user input."
	}
	// Full input remains authoritative in payload storage, bound by InputDigest.
	if len(objective) > 60<<10 {
		end := 60 << 10
		for !utf8.ValidString(objective[:end]) {
			end--
		}
		objective = objective[:end] + "\n[Objective excerpt; full original input is retained under input_digest.]"
	}
	a := taskflow.Acceptance{Version: 1, InputDigest: p.InputDigest, Objective: objective}
	a.Requirements, err = taskflow.ParseRequirements(policy)
	if err != nil {
		return err
	}
	if w.TaskRequirements != nil {
		extra, resolveErr := w.TaskRequirements(ctx, e, message)
		if resolveErr != nil {
			return resolveErr
		}
		a.Requirements = append(a.Requirements, extra...)
	}
	_, err = task.BindAcceptance(ctx, a)
	return err
}
```

## 发布与观察代码：trpcservice/taskflow/config.go

来源：`trpcservice/taskflow/config.go`，完整文件。职责、读取时序和边界见 design；这些读取不替代原工具执行授权。

```go
package taskflow

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

// ParseRequirements is shared by publication and execution. Policy is trusted
// versioned application configuration, never model-supplied checkpoint data.
func ParseRequirements(policy map[string]any) ([]Requirement, error) {
	v, ok := policy["task_requirements"]
	if !ok {
		return nil, nil
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > 64<<10 || string(b) == "null" {
		return nil, runtime.ErrInvalidEnvelope
	}
	var requirements []Requirement
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&requirements); err != nil {
		return nil, runtime.ErrInvalidEnvelope
	}
	a := Acceptance{Version: 1, InputDigest: strings.Repeat("0", 64), Objective: "configuration validation", Requirements: requirements}
	if err = a.Validate(); err != nil {
		return nil, err
	}
	return requirements, nil
}
```

## 发布与观察代码：trpcservice/taskflow/recovery_summary.go

来源：`trpcservice/taskflow/recovery_summary.go`，完整文件。职责、读取时序和边界见 design；这些读取不替代原工具执行授权。

```go
package taskflow

import "time"

type StepProgress struct {
	ID             string `json:"id"`
	Kind           string `json:"kind"`
	State          string `json:"state"`
	ToolID         string `json:"tool_id,omitempty"`
	ToolVersion    int64  `json:"tool_version,omitempty"`
	ResultRef      string `json:"result_ref,omitempty"`
	EvidenceDigest string `json:"evidence_digest,omitempty"`
}
type BlockedStep struct {
	ID           string   `json:"id"`
	Reason       string   `json:"reason"`
	Dependencies []string `json:"dependencies,omitempty"`
}
type RecoverySummary struct {
	Failed                 []StepProgress `json:"failed"`
	NeedsFinalRepair       bool           `json:"needs_final_repair"`
	PlanVersion            int64          `json:"plan_version"`
	Fence                  uint64         `json:"fence"`
	Phase                  string         `json:"phase"`
	InputDigest            string         `json:"input_digest"`
	Completed              []StepProgress `json:"completed"`
	Active                 []StepProgress `json:"active"`
	Ready                  []string       `json:"ready"`
	Blocked                []BlockedStep  `json:"blocked"`
	UnplannedRequirements  []string       `json:"unplanned_requirements"`
	UnfinishedRequirements []string       `json:"unfinished_requirements"`
	UnknownEffects         []string       `json:"unknown_effects"`
	NextAction             string         `json:"next_action"`
	FailureReason          string         `json:"failure_reason,omitempty"`
	Attempts               int            `json:"attempts"`
	NoProgress             int            `json:"no_progress"`
	RemainingAttempts      int            `json:"remaining_attempts"`
	Deadline               time.Time      `json:"deadline,omitempty"`
}

// SummarizeRecovery is a read-only projection, not another task authority or
// permission to execute. Runtime callers must Recover/verify evidence first.
// Operations callers expose it as observed checkpoint metadata, never as a
// fresh assertion that a provider effect has completed.
func SummarizeRecovery(p Plan, now time.Time) RecoverySummary {
	s := RecoverySummary{PlanVersion: p.Version, Fence: p.Fence, Phase: p.Phase, InputDigest: p.InputDigest, Completed: []StepProgress{}, Failed: []StepProgress{}, NeedsFinalRepair: p.CompletionIssue != "", Active: []StepProgress{}, Ready: []string{}, Blocked: []BlockedStep{}, UnplannedRequirements: []string{}, UnfinishedRequirements: []string{}, UnknownEffects: []string{}, FailureReason: p.FailureReason, Attempts: p.Attempts, NoProgress: p.NoProgress, RemainingAttempts: p.Contract.MaxAttempts - p.Attempts}
	if s.RemainingAttempts < 0 {
		s.RemainingAttempts = 0
	}
	if !p.StartedAt.IsZero() && p.Contract.MaxAgeSeconds > 0 {
		s.Deadline = p.StartedAt.Add(time.Duration(p.Contract.MaxAgeSeconds) * time.Second)
	}
	states := map[string]string{}
	mapped := map[string]bool{}
	unfinished := map[string]bool{}
	for _, st := range p.Steps {
		states[st.ID] = st.State
		for _, id := range st.RequirementIDs {
			mapped[id] = true
			if st.State != "succeeded" {
				unfinished[id] = true
			}
		}
		progress := StepProgress{ID: st.ID, Kind: st.Kind, State: st.State, ToolID: st.ToolID, ToolVersion: st.ToolVersion, ResultRef: st.ResultRef, EvidenceDigest: st.EvidenceDigest}
		if st.State == "succeeded" {
			s.Completed = append(s.Completed, progress)
		}
		if st.State == "failed" {
			s.Failed = append(s.Failed, progress)
		}
		if st.State == "running" || st.State == "effect_unknown" {
			s.Active = append(s.Active, progress)
		}
		if st.State == "effect_unknown" {
			s.UnknownEffects = append(s.UnknownEffects, st.ID)
		}
	}
	if p.Acceptance != nil {
		for _, r := range p.Acceptance.Requirements {
			if !mapped[r.ID] {
				s.UnplannedRequirements = append(s.UnplannedRequirements, r.ID)
			}
			if !mapped[r.ID] || unfinished[r.ID] {
				s.UnfinishedRequirements = append(s.UnfinishedRequirements, r.ID)
			}
		}
	}
	expired := !s.Deadline.IsZero() && !now.Before(s.Deadline)
	for _, st := range p.Steps {
		if st.State != "pending" {
			continue
		}
		blocked := BlockedStep{ID: st.ID, Dependencies: []string{}}
		for _, dep := range st.DependsOn {
			if states[dep] != "succeeded" {
				blocked.Dependencies = append(blocked.Dependencies, dep)
			}
		}
		switch {
		case p.Phase != "open":
			blocked.Reason = "task_not_open"
		case expired:
			blocked.Reason = "deadline_exceeded"
		case len(s.Failed) > 0:
			blocked.Reason = "failed_step_must_resolve"
		case len(blocked.Dependencies) > 0:
			blocked.Reason = "dependencies_incomplete"
		case len(s.Active) > 0:
			blocked.Reason = "active_step_must_finish"
		default:
			s.Ready = append(s.Ready, st.ID)
			continue
		}
		s.Blocked = append(s.Blocked, blocked)
	}
	switch {
	case p.Phase == "failed":
		s.NextAction = "stop_failed"
	case p.Phase == "sealed":
		s.NextAction = "resume_frozen_result"
	case expired:
		s.NextAction = "stop_deadline"
	case len(s.UnknownEffects) > 0:
		s.NextAction = "reconcile_unknown_effect"
	case len(s.Failed) > 0:
		s.NextAction = "inspect_failed_step"
	case len(s.Active) > 0:
		s.NextAction = "resume_active_step"
	case len(s.UnplannedRequirements) > 0 || len(p.Steps) == 0:
		s.NextAction = "plan_missing_obligations"
	case len(s.Ready) > 0:
		s.NextAction = "start_ready_step"
	case len(s.Blocked) > 0:
		s.NextAction = "inspect_blockers"
	case s.NeedsFinalRepair:
		s.NextAction = "repair_final_deliverable"
	default:
		s.NextAction = "validate_final_deliverable"
	}
	return s
}
```

## 发布与观察代码：trpcservice/tool/requirements.go

来源：`trpcservice/tool/requirements.go`，完整文件。职责、读取时序和边界见 design；这些读取不替代原工具执行授权。

```go
package tool

import (
	"context"
	"fmt"

	"github.com/liuzengh/trpc-agent-service/trpcservice/agentapp"
	"github.com/liuzengh/trpc-agent-service/trpcservice/profile"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

// ValidateTaskRequirements resolves metadata/build declarations only. It must
// never invoke Call or execute the business operation while publishing.
func (r Resolver) ValidateTaskRequirements(ctx context.Context, revision agentapp.Revision) error {
	if err := revision.ValidateTaskRequirements(); err != nil {
		return err
	}
	requirements, err := taskflow.ParseRequirements(revision.RuntimePolicy)
	if err != nil {
		return err
	}
	checked := map[string]bool{}
	for _, required := range requirements {
		if required.ToolID == "" || checked[required.ToolID] {
			continue
		}
		var ref profile.VersionedRef
		for _, pinned := range revision.ToolRefs {
			if pinned.ID == required.ToolID && pinned.Version == required.ToolVersion {
				ref = profile.VersionedRef{ID: pinned.ID, Version: pinned.Version, ContentDigest: pinned.ContentDigest}
			}
		}
		values, e := r.ResolveTools(ctx, revision.TenantID, []profile.VersionedRef{ref})
		if e != nil {
			return fmt.Errorf("%w: task requirement %s tool unavailable: %w", agentapp.ErrInvalid, required.ID, e)
		}
		if len(values) != 1 {
			return fmt.Errorf("%w: task requirement tool unavailable", agentapp.ErrInvalid)
		}
		descriptor, ok := values[0].(taskflow.EvidenceDescriptor)
		if !ok || descriptor.TaskEvidenceCheck() == nil || !descriptor.TaskEvidenceCheck().Valid() {
			return fmt.Errorf("%w: task requirement %s has no trusted completion check", agentapp.ErrInvalid, required.ID)
		}
		checked[required.ToolID] = true
	}
	return nil
}
```

## 发布与观察代码：trpcservice/storage/messaging/postgres/task_incident.go

来源：`trpcservice/storage/messaging/postgres/task_incident.go`，完整文件。职责、读取时序和边界见 design；这些读取不替代原工具执行授权。

```go
package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

func ValidateTaskIncidentKey(tenant, request string) error {
	if strings.TrimSpace(tenant) == "" || len(tenant) > 256 || strings.TrimSpace(request) == "" || len(request) > 1024 || strings.IndexFunc(tenant, unicode.IsControl) >= 0 || strings.IndexFunc(request, unicode.IsControl) >= 0 {
		return runtime.ErrInvalidEnvelope
	}
	return nil
}

// GetTaskIncident returns one statement snapshot of request-scoped metadata.
// Bodies, arguments, notes, raw tool errors/handles and ciphertext are excluded.
// Arrays are bounded; counts describe the entire request, not just the page.
func (s *Store) GetTaskIncident(ctx context.Context, tenant, request string) (json.RawMessage, error) {
	if err := ValidateTaskIncidentKey(tenant, request); err != nil {
		return nil, err
	}
	var b []byte
	err := s.db.QueryRowContext(ctx, taskIncidentSQL, tenant, request).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, runtime.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return json.RawMessage(b), nil
}

const taskIncidentSQL = `WITH target AS (SELECT tenant_id,request_id FROM inbox WHERE tenant_id=$1 AND request_id=$2 UNION SELECT tenant_id,request_id FROM execution_record WHERE tenant_id=$1 AND request_id=$2) SELECT jsonb_build_object(
'tenant_id',target.tenant_id,'request_id',target.request_id,'observed_at',now(),'array_limit',100,
'inbox',(SELECT jsonb_build_object('channel',i.channel,'agent_app_id',i.agent_app_id,'session_id',i.session_id,'input_seq',i.input_seq,'state',i.state,'version',i.version,'created_at',i.created_at,'updated_at',i.updated_at) FROM inbox i WHERE i.tenant_id=$1 AND i.request_id=$2),
'preprocess',(SELECT jsonb_build_object('job_id',p.job_id,'state',p.state,'attempt',p.attempt,'lease_owner',p.lease_owner,'lease_until',p.lease_until,'not_before',p.not_before) FROM preprocess_job p WHERE p.tenant_id=$1 AND p.request_id=$2),
'execution',CASE WHEN e.request_id IS NULL THEN NULL ELSE jsonb_build_object('agent_app_id',e.agent_app_id,'session_id',e.session_id,'input_seq',e.input_seq,'outcome',e.outcome,'version',e.version,'model_calls',e.model_call_count,'model_request_attempts',e.model_request_count,'cancel_requested_at',e.cancel_requested_at,'blocked_reason',e.blocked_reason,'not_before',e.not_before) END,
'plan',(SELECT jsonb_build_object('phase',p.phase,'version',p.version,'fence',p.fence,'updated_at',p.updated_at) FROM task_plan p WHERE p.tenant_id=$1 AND p.request_id=$2),
'checkpoint_count',(SELECT count(*) FROM task_checkpoint c WHERE c.tenant_id=$1 AND c.request_id=$2),
'attempt_count',(SELECT count(*) FROM execution_attempt a WHERE a.tenant_id=$1 AND a.request_id=$2),
'attempts',(SELECT coalesce(jsonb_agg(q ORDER BY q.attempt_no DESC),'[]') FROM (SELECT attempt_no,worker_id,fence,state,started_at,finished_at FROM execution_attempt WHERE tenant_id=$1 AND request_id=$2 ORDER BY attempt_no DESC LIMIT 100) q),
'tool_count',(SELECT count(*) FROM tool_execution t WHERE t.tenant_id=$1 AND t.request_id=$2),
'tools',(SELECT coalesce(jsonb_agg(q ORDER BY (q.state<>'succeeded') DESC,q.updated_at DESC,q.tool_call_id),'[]') FROM (SELECT tool_call_id,tool_id,tool_version,state,recovery_policy,attempt,fence,lease_owner,lease_until,result_ref,(last_error IS NOT NULL AND last_error<>'') has_error,created_at,updated_at FROM tool_execution WHERE tenant_id=$1 AND request_id=$2 ORDER BY (state<>'succeeded') DESC,updated_at DESC,tool_call_id LIMIT 100) q),
'completion',(SELECT jsonb_build_object('content_digest',c.content_digest,'envelope_digest',c.envelope_digest,'fence',c.fence,'created_at',c.created_at) FROM execution_completion c WHERE c.tenant_id=$1 AND c.request_id=$2),
'commits',(SELECT coalesce(jsonb_agg(q ORDER BY q.created_at),'[]') FROM (SELECT commit_id,stage,outcome,fence,created_at FROM session_commit WHERE tenant_id=$1 AND request_id=$2 ORDER BY created_at DESC LIMIT 100) q),
'outbox_count',(SELECT count(*) FROM outbox o WHERE o.tenant_id=$1 AND o.aggregate_id=$2),
'outbox',(SELECT coalesce(jsonb_agg(q ORDER BY (q.state<>'published') DESC,q.created_at,q.outbox_id),'[]') FROM (SELECT outbox_id,kind,idempotency_key,state,version,attempt,claim_owner,claim_until,next_attempt_at,created_at FROM outbox WHERE tenant_id=$1 AND aggregate_id=$2 ORDER BY (state<>'published') DESC,created_at,outbox_id LIMIT 100) q),
'delivery_count',(SELECT count(*) FROM delivery_ledger d WHERE d.tenant_id=$1 AND EXISTS(SELECT 1 FROM outbox o WHERE o.tenant_id=$1 AND o.aggregate_id=$2 AND o.kind='reply' AND o.idempotency_key=d.delivery_key)),
'deliveries',(SELECT coalesce(jsonb_agg(q ORDER BY (q.state<>'sent') DESC,q.updated_at DESC,q.delivery_key,q.segment_no),'[]') FROM (SELECT d.delivery_key,d.segment_no,d.state,d.version,d.attempt,d.reconcile_attempt,d.last_error_class,d.provider_message_id,d.claim_owner,d.claim_until,d.updated_at,EXISTS(SELECT 1 FROM delivery_receipt r WHERE r.tenant_id=$1 AND r.delivery_key=d.delivery_key AND r.segment_no=d.segment_no) has_receipt FROM delivery_ledger d WHERE d.tenant_id=$1 AND EXISTS(SELECT 1 FROM outbox o WHERE o.tenant_id=$1 AND o.aggregate_id=$2 AND o.kind='reply' AND o.idempotency_key=d.delivery_key) ORDER BY (d.state<>'sent') DESC,d.updated_at DESC,d.delivery_key,d.segment_no LIMIT 100) q)
) FROM target LEFT JOIN execution_record e ON e.tenant_id=target.tenant_id AND e.request_id=target.request_id`
```
