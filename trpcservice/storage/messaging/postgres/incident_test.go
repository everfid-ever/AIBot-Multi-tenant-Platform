package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
)

func TestDeliveryIncidentReadsOnlyScopedMetadataAndEvidence(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := messaging.DeliveryKey{TenantID: "tenant-a", DeliveryKey: "reply", SegmentNo: 2}
	now := time.Now()
	mock.ExpectQuery("SELECT COALESCE").WithArgs("tenant-a", "reply", 2).WillReturnRows(sqlmock.NewRows([]string{"provider_message_id", "state", "renderer_version", "format_version", "content_digest", "segment_count", "attempt", "reconcile_attempt", "not_before", "last_error_class", "client_request_id", "claim_owner", "claim_until", "version", "updated_at"}).AddRow("", "ambiguous", "v1", "text", "digest", 3, 1, 0, now, "response_lost", "client", "", now, 4, now))
	mock.ExpectQuery("SELECT client_request_id,provider_message_id").WithArgs("tenant-a", "reply", 2).WillReturnRows(sqlmock.NewRows([]string{"client_request_id", "provider_message_id", "plan_digest", "route_digest", "segment_digest", "created_at"}).AddRow("client", "provider", messaging.ReceiptDigest("plan"), messaging.ReceiptDigest("route"), messaging.ReceiptDigest("segment"), now))
	mock.ExpectQuery("SELECT expected_version,actor").WithArgs("tenant-a", "reply", 2).WillReturnRows(sqlmock.NewRows([]string{"expected_version", "actor", "evidence_ref", "decision", "provider_message_id", "created_at"}).AddRow(2, "deployment-admin/alice", "incident/123", "delivered", "provider", now))
	i, err := New(db).GetDeliveryIncident(context.Background(), key)
	if err != nil || i.Delivery.Version != 4 || i.Receipt == nil || i.Receipt.ProviderMessageID != "provider" || len(i.Resolutions) != 1 || i.Resolutions[0].TenantID != "tenant-a" {
		t.Fatal(i, err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
