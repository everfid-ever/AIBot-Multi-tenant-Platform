package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	channel "github.com/liuzengh/trpc-agent-service/trpcservice/channels/contract"
	"github.com/liuzengh/trpc-agent-service/trpcservice/reliability/inflight"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	memory "github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging/inmemory"
)

type crashAfterReceipt struct{}

func (crashAfterReceipt) Wait(_ context.Context, p inflight.Point) error {
	if p == inflight.PointP4ReceiptPersisted {
		return runtime.ErrLeaseLost
	}
	return nil
}
func receiptFixture(t *testing.T) (*memory.Store, channel.ReplyEvent, *adapterStub, Service) {
	t.Helper()
	store := memory.New()
	if err := store.PutResult(context.Background(), messaging.ResultRecord{TenantID: "tenant", RequestID: "request", ResultRef: "result://request", ContentDigest: "digest", Content: []byte("answer"), KeyVersion: 1}); err != nil {
		t.Fatal(err)
	}
	adapter := &adapterStub{}
	s := Service{Results: store, Ledger: store, Adapters: resolverStub{adapter: adapter}, Owner: "node-a", ClaimTTL: 10 * time.Millisecond, DefaultRetryDelay: time.Nanosecond}
	return store, testReplyEvent("result://request"), adapter, s
}
func TestDurableProviderReceiptRecoversWithoutProviderQueryOrResend(t *testing.T) {
	store, event, adapter, s := receiptFixture(t)
	s.Barrier = crashAfterReceipt{}
	if err := s.Deliver(context.Background(), event); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatal(err)
	}
	key := messaging.DeliveryKey{TenantID: "tenant", DeliveryKey: event.DeliveryKey, SegmentNo: 0}
	receipt, err := store.GetProviderReceipt(context.Background(), key)
	if err != nil || receipt.ProviderMessageID != "provider-message" {
		t.Fatal(receipt, err)
	}
	time.Sleep(15 * time.Millisecond)
	s.Owner = "node-b"
	s.Barrier = nil
	if err = s.Deliver(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	r, err := store.GetDelivery(context.Background(), key)
	if err != nil || r.State != messaging.DeliverySent || r.LastErrorClass != "provider_receipt_recovered" || adapter.calls != 1 || r.Attempt != 1 {
		t.Fatalf("r=%+v calls=%d err=%v", r, adapter.calls, err)
	}
	if err = s.Deliver(context.Background(), event); err != nil || adapter.calls != 1 {
		t.Fatal("sent receipt was resent", err)
	}
}

func TestReceiptWithDifferentDestinationCannotConfirmOrResend(t *testing.T) {
	store, event, adapter, s := receiptFixture(t)
	s.Barrier = crashAfterReceipt{}
	if err := s.Deliver(context.Background(), event); !errors.Is(err, runtime.ErrLeaseLost) {
		t.Fatal(err)
	}
	time.Sleep(15 * time.Millisecond)
	s.Barrier = nil
	s.Owner = "node-b"
	event.Target.ExternalAccountID = "different-account"
	if err := s.Deliver(context.Background(), event); err == nil {
		t.Fatal("wrong route accepted")
	}
	r, err := store.GetDelivery(context.Background(), messaging.DeliveryKey{TenantID: "tenant", DeliveryKey: event.DeliveryKey, SegmentNo: 0})
	if err != nil || r.State != messaging.DeliveryAmbiguous || adapter.calls != 1 {
		t.Fatal(r, err)
	}
}

type brokenReceiptStore struct {
	messaging.ProviderReceiptStore
	err error
}

func (s brokenReceiptStore) PutProviderReceipt(context.Context, messaging.DeliveryRecord, messaging.ProviderReceipt) error {
	return s.err
}
func TestAcceptedSendReceiptFailureCannotTriggerAnotherSend(t *testing.T) {
	store, event, adapter, s := receiptFixture(t)
	s.Receipts = brokenReceiptStore{ProviderReceiptStore: store, err: errors.New("receipt storage unavailable")}
	if err := s.Deliver(context.Background(), event); err == nil {
		t.Fatal("missing receipt ignored")
	}
	r, _ := store.GetDelivery(context.Background(), messaging.DeliveryKey{TenantID: "tenant", DeliveryKey: event.DeliveryKey, SegmentNo: 0})
	if r.State != messaging.DeliveryAmbiguous || r.LastErrorClass != "receipt_persist_failed" {
		t.Fatal(r)
	}
	_ = s.Deliver(context.Background(), event)
	if adapter.calls != 1 {
		t.Fatal("accepted send blindly replayed")
	}
}

func TestUnknownOrContradictoryOutcomeNeverEntersRetryWait(t *testing.T) {
	for _, scenario := range []string{"unknown_error", "no_outcome", "contradiction"} {
		t.Run(scenario, func(t *testing.T) {
			store, event, adapter, s := receiptFixture(t)
			switch scenario {
			case "unknown_error":
				adapter.err = errors.New("transport outcome lost")
			case "no_outcome":
				adapter.result = &channel.DeliveryResult{}
			case "contradiction":
				adapter.err = channel.RetryableDeliveryError{Err: errors.New("not accepted")}
			}
			if scenario == "contradiction" {
				s.Adapters = resolverStub{adapter: contradictionAdapter{adapter}}
			}
			if err := s.Deliver(context.Background(), event); err == nil {
				t.Fatal("uncertain outcome ignored")
			}
			r, _ := store.GetDelivery(context.Background(), messaging.DeliveryKey{TenantID: "tenant", DeliveryKey: event.DeliveryKey, SegmentNo: 0})
			if r.State != messaging.DeliveryAmbiguous {
				t.Fatal(r)
			}
			_ = s.Deliver(context.Background(), event)
			if adapter.calls != 1 {
				t.Fatal("uncertain send repeated")
			}
		})
	}
}

type contradictionAdapter struct{ *adapterStub }

func (a contradictionAdapter) Deliver(_ context.Context, r channel.DeliveryRequest) (channel.DeliveryResult, error) {
	a.calls++
	return channel.DeliveryResult{Delivered: true, ProviderMessageID: "accepted"}, channel.RetryableDeliveryError{Err: errors.New("conflicting rejection")}
}

func TestFailedSegmentBlocksAllLaterSegmentsOnReplay(t *testing.T) {
	_, event, adapter, s := receiptFixture(t)
	adapter.maxTextBytes = 3
	adapter.err = PermanentError{Err: errors.New("rejected"), Class: "recipient_blocked"}
	for i := 0; i < 2; i++ {
		var terminal TerminalError
		if err := s.Deliver(context.Background(), event); !errors.As(err, &terminal) {
			t.Fatal(err)
		}
	}
	if adapter.calls != 1 {
		t.Fatal("sent later segment after terminal failure", adapter.calls)
	}
}

type renewFailureLedger struct{ messaging.DeliveryLedger }

func (renewFailureLedger) RenewDeliveryClaim(context.Context, messaging.DeliveryRecord, time.Duration) (messaging.DeliveryRecord, error) {
	return messaging.DeliveryRecord{}, runtime.ErrLeaseLost
}

type cancelledAdapter struct{ *adapterStub }

func (a cancelledAdapter) Deliver(ctx context.Context, _ channel.DeliveryRequest) (channel.DeliveryResult, error) {
	a.calls++
	<-ctx.Done()
	return channel.DeliveryResult{}, ctx.Err()
}
func TestClaimRenewalLossWithoutResponseRemainsAmbiguous(t *testing.T) {
	store, event, adapter, s := receiptFixture(t)
	s.Ledger = renewFailureLedger{store}
	s.Adapters = resolverStub{adapter: cancelledAdapter{adapter}}
	s.ClaimRenewInterval = time.Millisecond
	if err := s.Deliver(context.Background(), event); err == nil {
		t.Fatal("lost lease ignored")
	}
	r, _ := store.GetDelivery(context.Background(), messaging.DeliveryKey{TenantID: "tenant", DeliveryKey: event.DeliveryKey, SegmentNo: 0})
	if r.State != messaging.DeliveryAmbiguous || adapter.calls != 1 {
		t.Fatal(r)
	}
}

func TestChangedSegmentationWithSameCountCannotReuseSentReceipt(t *testing.T) {
	_, event, adapter, s := receiptFixture(t)
	adapter.maxTextBytes = 3
	if err := s.Deliver(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	if adapter.calls != 2 {
		t.Fatal(adapter.calls)
	}
	adapter.maxTextBytes = 4
	if err := s.Deliver(context.Background(), event); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatal("changed segment bytes accepted", err)
	}
	if adapter.calls != 2 {
		t.Fatal("changed format was resent")
	}
}
