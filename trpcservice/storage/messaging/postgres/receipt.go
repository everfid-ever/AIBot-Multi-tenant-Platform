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
