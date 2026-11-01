package inmemory

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
)

func TestProviderReceiptCannotBeOverwrittenOrSavedByExpiredOwner(t *testing.T) {
	ctx := context.Background()
	s := New()
	key := messaging.DeliveryKey{TenantID: "a", DeliveryKey: "reply"}
	plan := messaging.DeliveryPlan{RendererVersion: "v1", FormatVersion: "text", ContentDigest: "digest", SegmentCount: 1}
	owner, _, err := s.ClaimDelivery(ctx, key, plan, messaging.DeliveryClaim{Owner: "node", TTL: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	r := messaging.ProviderReceipt{DeliveryKey: key, ClientRequestID: owner.ClientRequestID, ProviderMessageID: "accepted", PlanDigest: messaging.ReceiptDigest(plan), RouteDigest: messaging.ReceiptDigest("route"), SegmentDigest: messaging.ReceiptDigest("segment")}
	if err = s.PutProviderReceipt(ctx, owner, r); err != nil {
		t.Fatal(err)
	}
	if err = s.PutProviderReceipt(ctx, owner, r); err != nil {
		t.Fatal(err)
	}
	r.ProviderMessageID = "other"
	if err = s.PutProviderReceipt(ctx, owner, r); !errors.Is(err, runtime.ErrIdempotencyCollision) {
		t.Fatal(err)
	}
	wrong := key
	wrong.TenantID = "b"
	if _, err = s.GetProviderReceipt(ctx, wrong); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal("cross tenant receipt", err)
	}
	s.mu.Lock()
	expired := s.deliveries[key]
	expired.ClaimUntil = time.Now().Add(-time.Second)
	s.deliveries[key] = expired
	s.mu.Unlock()
	r.ProviderMessageID = "accepted"
	if err = s.PutProviderReceipt(ctx, owner, r); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatal(err)
	}
}
