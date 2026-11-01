package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
)

func TestProviderReceiptRequiresLiveOwnerAndPreservesImmutableEvidence(t *testing.T) {
	for _, scenario := range []string{"saved", "lost_owner", "different_receipt"} {
		t.Run(scenario, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			owner := messaging.DeliveryRecord{DeliveryKey: messaging.DeliveryKey{TenantID: "tenant", DeliveryKey: "reply", SegmentNo: 0}, ClientRequestID: "client", ClaimOwner: "node-a", Version: 2, Plan: messaging.DeliveryPlan{RendererVersion: "v1", FormatVersion: "text", ContentDigest: "content", SegmentCount: 1}}
			r := messaging.ProviderReceipt{DeliveryKey: owner.DeliveryKey, ClientRequestID: "client", ProviderMessageID: "provider", PlanDigest: messaging.ReceiptDigest(owner.Plan), RouteDigest: messaging.ReceiptDigest("route"), SegmentDigest: messaging.ReceiptDigest("segment")}
			q := mock.ExpectQuery("INSERT INTO delivery_receipt").WithArgs("tenant", "reply", 0, "client", "provider", r.PlanDigest, r.RouteDigest, int64(2), "node-a", r.SegmentDigest)
			if scenario == "lost_owner" {
				q.WillReturnError(sql.ErrNoRows)
			} else {
				id := "provider"
				if scenario == "different_receipt" {
					id = "other"
				}
				q.WillReturnRows(sqlmock.NewRows([]string{"client_request_id", "provider_message_id", "plan_digest", "route_digest", "segment_digest"}).AddRow("client", id, r.PlanDigest, r.RouteDigest, r.SegmentDigest))
			}
			err = New(db).PutProviderReceipt(context.Background(), owner, r)
			switch scenario {
			case "saved":
				if err != nil {
					t.Fatal(err)
				}
			case "lost_owner":
				if !errors.Is(err, runtime.ErrLeaseLost) {
					t.Fatal(err)
				}
			case "different_receipt":
				if !errors.Is(err, runtime.ErrIdempotencyCollision) {
					t.Fatal(err)
				}
			}
			if err = mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
