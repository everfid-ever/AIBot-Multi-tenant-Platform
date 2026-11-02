package postgres

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

func TestTaskIncidentScopesOneSnapshotAndDistinguishesAbsentFromUnavailable(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := New(db)
	for _, tc := range []struct {
		tenant  string
		failure error
		want    error
	}{{"a", nil, nil}, {"other", sql.ErrNoRows, runtime.ErrNotFound}, {"a", errors.New("db unavailable"), nil}} {
		q := m.ExpectQuery(regexp.QuoteMeta(taskIncidentSQL)).WithArgs(tc.tenant, "original-request")
		if tc.failure != nil {
			q.WillReturnError(tc.failure)
		} else {
			q.WillReturnRows(sqlmock.NewRows([]string{"metadata"}).AddRow(`{"tenant_id":"a","request_id":"original-request","inbox":{"state":"dispatch_pending"},"execution":null}`))
		}
		got, e := s.GetTaskIncident(context.Background(), tc.tenant, "original-request")
		if tc.failure == nil && (e != nil || len(got) == 0) {
			t.Fatal(string(got), e)
		}
		if tc.failure != nil && tc.want != nil && !errors.Is(e, tc.want) {
			t.Fatal(e)
		}
		if tc.failure != nil && tc.want == nil && (e == nil || errors.Is(e, runtime.ErrNotFound)) {
			t.Fatalf("storage failure treated as absent: %v", e)
		}
	}
	if _, err = s.GetTaskIncident(context.Background(), "a\n", "r"); !errors.Is(err, runtime.ErrInvalidEnvelope) {
		t.Fatal(err)
	}
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
