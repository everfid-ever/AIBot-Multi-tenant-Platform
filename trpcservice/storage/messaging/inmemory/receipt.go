package inmemory

import (
	"context"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
)

func (s *Store) PutProviderReceipt(ctx context.Context, owner messaging.DeliveryRecord, r messaging.ProviderReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, ok := s.deliveries[r.DeliveryKey]
	if !ok || current.State != messaging.DeliverySending || current.Version != owner.Version || current.ClaimOwner != owner.ClaimOwner || current.ClaimOwner == "" || !current.ClaimUntil.After(time.Now()) {
		return runtime.ErrLeaseLost
	}
	if owner.DeliveryKey != r.DeliveryKey || current.ClientRequestID != r.ClientRequestID || r.PlanDigest != messaging.ReceiptDigest(current.Plan) {
		return runtime.ErrVersionMismatch
	}
	if s.receipts == nil {
		s.receipts = map[messaging.DeliveryKey]messaging.ProviderReceipt{}
	}
	if old, ok := s.receipts[r.DeliveryKey]; ok {
		if old.ClientRequestID != r.ClientRequestID || old.ProviderMessageID != r.ProviderMessageID || old.PlanDigest != r.PlanDigest || old.RouteDigest != r.RouteDigest || old.SegmentDigest != r.SegmentDigest {
			return runtime.ErrIdempotencyCollision
		}
		return nil
	}
	r.CreatedAt = time.Now().UTC()
	s.receipts[r.DeliveryKey] = r
	return nil
}
func (s *Store) GetProviderReceipt(ctx context.Context, k messaging.DeliveryKey) (messaging.ProviderReceipt, error) {
	if err := ctx.Err(); err != nil {
		return messaging.ProviderReceipt{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.receipts[k]
	if !ok {
		return r, runtime.ErrNotFound
	}
	return r, nil
}
