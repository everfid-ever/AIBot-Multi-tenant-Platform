package postgres

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strconv"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

type TaskRecordsQuery struct {
	TenantID, RequestID, Kind, Cursor string
	Limit                             int
}
type TaskRecordsPage struct {
	TenantID   string            `json:"tenant_id"`
	RequestID  string            `json:"request_id"`
	Kind       string            `json:"kind"`
	Records    []json.RawMessage `json:"records"`
	NextCursor string            `json:"next_cursor,omitempty"`
	ObservedAt time.Time         `json:"observed_at"`
}
type taskRecordsCursor struct {
	Version int      `json:"version"`
	Tenant  string   `json:"tenant"`
	Request string   `json:"request"`
	Kind    string   `json:"kind"`
	After   []string `json:"after"`
}

func parseRecordsCursor(in TaskRecordsQuery) ([]string, error) {
	if ValidateTaskIncidentKey(in.TenantID, in.RequestID) != nil || in.Limit < 0 || in.Limit > 100 {
		return nil, runtime.ErrInvalidEnvelope
	}
	n := 1
	switch in.Kind {
	case "tools", "attempts", "outbox":
	case "commits":
		n = 3
	case "deliveries":
		n = 2
	default:
		return nil, runtime.ErrInvalidEnvelope
	}
	after := make([]string, n)
	if in.Cursor == "" {
		if in.Kind == "attempts" {
			after[0] = "0"
		}
		if in.Kind == "deliveries" {
			after[1] = "-1"
		}
		return after, nil
	}
	if len(in.Cursor) > 8192 {
		return nil, runtime.ErrInvalidEnvelope
	}
	b, err := base64.RawURLEncoding.DecodeString(in.Cursor)
	if err != nil {
		return nil, runtime.ErrInvalidEnvelope
	}
	var c taskRecordsCursor
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.Tenant != in.TenantID || c.Request != in.RequestID || c.Kind != in.Kind || len(c.After) != n {
		return nil, runtime.ErrInvalidEnvelope
	}
	for _, v := range c.After {
		if v == "" || len(v) > 1024 {
			return nil, runtime.ErrInvalidEnvelope
		}
	}
	if in.Kind == "attempts" {
		v, e := strconv.ParseInt(c.After[0], 10, 64)
		if e != nil || v < 1 {
			return nil, runtime.ErrInvalidEnvelope
		}
	}
	if in.Kind == "deliveries" {
		v, e := strconv.ParseInt(c.After[1], 10, 32)
		if e != nil || v < 0 {
			return nil, runtime.ErrInvalidEnvelope
		}
	}
	return c.After, nil
}

// Pages use immutable primary-key order rather than mutable priority/state.
// Each page is a live statement snapshot, not a historical export transaction.
func (s *Store) GetTaskRecordsPage(ctx context.Context, in TaskRecordsQuery) (TaskRecordsPage, error) {
	page := TaskRecordsPage{TenantID: in.TenantID, RequestID: in.RequestID, Kind: in.Kind, Records: []json.RawMessage{}}
	after, err := parseRecordsCursor(in)
	if err != nil {
		return page, err
	}
	limit := in.Limit
	if limit == 0 {
		limit = 50
	}
	query, args := taskRecordsSQL(in, after, limit+1)
	var exists bool
	var b []byte
	if err = s.db.QueryRowContext(ctx, query, args...).Scan(&exists, &b, &page.ObservedAt); err != nil {
		return page, err
	}
	if !exists {
		return page, runtime.ErrNotFound
	}
	if err = json.Unmarshal(b, &page.Records); err != nil {
		return page, err
	}
	if len(page.Records) > limit {
		page.Records = page.Records[:limit]
		position, e := recordPosition(in.Kind, page.Records[limit-1])
		if e != nil {
			return page, e
		}
		c, _ := json.Marshal(taskRecordsCursor{Version: 1, Tenant: in.TenantID, Request: in.RequestID, Kind: in.Kind, After: position})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(c)
	}
	return page, nil
}

func recordPosition(kind string, b []byte) ([]string, error) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(b, &fields) != nil {
		return nil, runtime.ErrInvariantViolation
	}
	keys := []string{"tool_call_id"}
	switch kind {
	case "attempts":
		keys = []string{"attempt_no"}
	case "outbox":
		keys = []string{"outbox_id"}
	case "commits":
		keys = []string{"agent_app_id", "session_id", "commit_id"}
	case "deliveries":
		keys = []string{"delivery_key", "segment_no"}
	}
	var out []string
	for _, k := range keys {
		if k == "attempt_no" || k == "segment_no" {
			var n int64
			if json.Unmarshal(fields[k], &n) != nil {
				return nil, runtime.ErrInvariantViolation
			}
			out = append(out, strconv.FormatInt(n, 10))
		} else {
			var v string
			if json.Unmarshal(fields[k], &v) != nil || v == "" {
				return nil, runtime.ErrInvariantViolation
			}
			out = append(out, v)
		}
	}
	return out, nil
}

func taskRecordsSQL(in TaskRecordsQuery, after []string, limit int) (string, []any) {
	args := []any{in.TenantID, in.RequestID}
	var records, order string
	switch in.Kind {
	case "tools":
		records = `SELECT tool_call_id,tool_id,tool_version,state,recovery_policy,attempt,fence,lease_owner,lease_until,result_ref,(last_error IS NOT NULL AND last_error<>'') has_error,created_at,updated_at FROM tool_execution WHERE tenant_id=$1 AND request_id=$2 AND tool_call_id>$3 ORDER BY tool_call_id LIMIT $4`
		order = "q.tool_call_id"
		args = append(args, after[0], limit)
	case "attempts":
		n, _ := strconv.ParseInt(after[0], 10, 64)
		records = `SELECT attempt_no,worker_id,fence,state,started_at,finished_at FROM execution_attempt WHERE tenant_id=$1 AND request_id=$2 AND attempt_no>$3 ORDER BY attempt_no LIMIT $4`
		order = "q.attempt_no"
		args = append(args, n, limit)
	case "outbox":
		records = `SELECT outbox_id,kind,idempotency_key,state,version,attempt,claim_owner,claim_until,next_attempt_at,created_at FROM outbox WHERE tenant_id=$1 AND aggregate_id=$2 AND outbox_id>$3 ORDER BY outbox_id LIMIT $4`
		order = "q.outbox_id"
		args = append(args, after[0], limit)
	case "commits":
		records = `SELECT agent_app_id,session_id,commit_id,stage,outcome,fence,created_at FROM session_commit WHERE tenant_id=$1 AND request_id=$2 AND (agent_app_id,session_id,commit_id)>($3,$4,$5) ORDER BY agent_app_id,session_id,commit_id LIMIT $6`
		order = "q.agent_app_id,q.session_id,q.commit_id"
		args = append(args, after[0], after[1], after[2], limit)
	case "deliveries":
		n, _ := strconv.ParseInt(after[1], 10, 32)
		records = `SELECT d.delivery_key,d.segment_no,d.state,d.version,d.attempt,d.reconcile_attempt,d.last_error_class,d.provider_message_id,d.claim_owner,d.claim_until,d.updated_at,EXISTS(SELECT 1 FROM delivery_receipt r WHERE r.tenant_id=$1 AND r.delivery_key=d.delivery_key AND r.segment_no=d.segment_no) has_receipt FROM delivery_ledger d WHERE d.tenant_id=$1 AND (d.delivery_key,d.segment_no)>($3,$4) AND EXISTS(SELECT 1 FROM outbox o WHERE o.tenant_id=$1 AND o.aggregate_id=$2 AND o.kind='reply' AND o.idempotency_key=d.delivery_key) ORDER BY d.delivery_key,d.segment_no LIMIT $5`
		order = "q.delivery_key,q.segment_no"
		args = append(args, after[0], n, limit)
	}
	// Every inserted SQL fragment comes from the fixed switch, not query input.
	query := `WITH target AS (SELECT tenant_id,request_id FROM inbox WHERE tenant_id=$1 AND request_id=$2 UNION SELECT tenant_id,request_id FROM execution_record WHERE tenant_id=$1 AND request_id=$2) SELECT EXISTS(SELECT 1 FROM target),(SELECT coalesce(jsonb_agg(q ORDER BY ` + order + `),'[]') FROM (` + records + `) q),now()`
	return query, args
}
