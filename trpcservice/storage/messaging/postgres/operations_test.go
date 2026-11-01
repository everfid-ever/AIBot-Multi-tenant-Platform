package postgres

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

func TestResolveUncertainDeliveryAtomicEvidenceAndRepublish(t *testing.T) {
	for _, decision := range []string{"delivered", "not_delivered", "retry_not_delivered"} {
		t.Run(decision, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			in := DeliveryResolution{TenantID: "tenant-a", DeliveryKey: "reply-1", SegmentNo: 0, ExpectedVersion: 3, Actor: "operator", EvidenceRef: "incident/123", Decision: decision}
			state, provider := "sent", "provider-1"
			if decision != "delivered" {
				provider = ""
				state = "failed"
			}
			if decision == "retry_not_delivered" {
				state = "retry_wait"
			}
			in.ProviderMessageID = provider
			mock.ExpectBegin()
			mock.ExpectQuery("UPDATE delivery_ledger").WithArgs("tenant-a", "reply-1", 0, int64(3), state, provider, sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
			mock.ExpectExec("INSERT INTO delivery_resolution").WithArgs("tenant-a", "reply-1", 0, int64(3), "operator", "incident/123", decision, provider).WillReturnResult(sqlmock.NewResult(0, 1))
			if decision != "not_delivered" {
				mock.ExpectQuery("UPDATE outbox").WithArgs("tenant-a", "reply-1").WillReturnRows(sqlmock.NewRows([]string{"outbox_id"}).AddRow("outbox-1"))
			}
			mock.ExpectCommit()
			if err := New(db).ResolveUncertainDelivery(context.Background(), in); err != nil {
				t.Fatal(err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResolveUncertainDeliveryRollsBackStaleVersionAndAuditFailure(t *testing.T) {
	for _, stale := range []bool{true, false} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		in := DeliveryResolution{TenantID: "tenant-a", DeliveryKey: "reply-1", ExpectedVersion: 3, Actor: "operator", EvidenceRef: "incident/123", Decision: "not_delivered"}
		mock.ExpectBegin()
		query := mock.ExpectQuery("UPDATE delivery_ledger")
		if stale {
			query.WillReturnError(sql.ErrNoRows)
			mock.ExpectQuery("SELECT actor,evidence_ref").WithArgs("tenant-a", "reply-1", 0, int64(3)).WillReturnError(sql.ErrNoRows)
		} else {
			query.WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
			mock.ExpectExec("INSERT INTO delivery_resolution").WillReturnError(errors.New("audit unavailable"))
		}
		mock.ExpectRollback()
		err = New(db).ResolveUncertainDelivery(context.Background(), in)
		if err == nil || (stale && !errors.Is(err, runtime.ErrVersionConflict)) {
			t.Fatal(err)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestResolveUncertainDeliveryResponseRetryIsIdempotentAndCannotChangeEvidence(t *testing.T) {
	for _, same := range []bool{true, false} {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		in := DeliveryResolution{TenantID: "tenant-a", DeliveryKey: "reply-1", ExpectedVersion: 3, Actor: "operator", EvidenceRef: "incident/123", Decision: "delivered", ProviderMessageID: "provider-1"}
		mock.ExpectBegin()
		mock.ExpectQuery("UPDATE delivery_ledger").WillReturnError(sql.ErrNoRows)
		evidence := "incident/123"
		if !same {
			evidence = "different"
		}
		mock.ExpectQuery("SELECT actor,evidence_ref").WithArgs("tenant-a", "reply-1", 0, int64(3)).WillReturnRows(sqlmock.NewRows([]string{"actor", "evidence_ref", "decision", "provider_message_id"}).AddRow("operator", evidence, "delivered", "provider-1"))
		if same {
			mock.ExpectCommit()
		} else {
			mock.ExpectRollback()
		}
		err = New(db).ResolveUncertainDelivery(context.Background(), in)
		if same && err != nil || !same && !errors.Is(err, runtime.ErrIdempotencyCollision) {
			t.Fatal(err)
		}
		if err = mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestResolveUncertainDeliveryRollsBackWhenOutboxCannotBeRearmed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	in := DeliveryResolution{TenantID: "tenant-a", DeliveryKey: "reply-1", ExpectedVersion: 3, Actor: "operator", EvidenceRef: "incident/123", Decision: "retry_not_delivered"}
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE delivery_ledger").WillReturnRows(sqlmock.NewRows([]string{"version"}).AddRow(4))
	mock.ExpectExec("INSERT INTO delivery_resolution").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery("UPDATE outbox").WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()
	if err := New(db).ResolveUncertainDelivery(context.Background(), in); !errors.Is(err, runtime.ErrVersionConflict) {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
