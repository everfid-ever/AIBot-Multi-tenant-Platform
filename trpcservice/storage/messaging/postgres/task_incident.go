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
