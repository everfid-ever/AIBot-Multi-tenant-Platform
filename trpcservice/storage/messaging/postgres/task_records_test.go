package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/base64"
	"encoding/json"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

func TestTaskRecordsKeysetPagesBindScopeAndDoNotRepeatBoundary(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer db.Close()
	s := New(db)
	now := time.Now()
	m.ExpectQuery("WITH target AS").WithArgs("a", "r", "", 3).WillReturnRows(sqlmock.NewRows([]string{"exists", "records", "observed"}).AddRow(true, `[{"tool_call_id":"op-1","state":"running"},{"tool_call_id":"op-2","state":"succeeded"},{"tool_call_id":"op-3","state":"failed"}]`, now))
	in := TaskRecordsQuery{TenantID: "a", RequestID: "r", Kind: "tools", Limit: 2}
	p, err := s.GetTaskRecordsPage(context.Background(), in)
	if err != nil || len(p.Records) != 2 || p.NextCursor == "" {
		t.Fatal(p, err)
	}
	in.Cursor = p.NextCursor
	// Mutable state is absent from the cursor: only immutable primary keys advance.
	m.ExpectQuery("WITH target AS").WithArgs("a", "r", "op-2", 3).WillReturnRows(sqlmock.NewRows([]string{"exists", "records", "observed"}).AddRow(true, `[{"tool_call_id":"op-3","state":"succeeded"}]`, now))
	p, err = s.GetTaskRecordsPage(context.Background(), in)
	if err != nil || len(p.Records) != 1 || p.NextCursor != "" {
		t.Fatal(p, err)
	}
	for _, changed := range []TaskRecordsQuery{{TenantID: "other", RequestID: "r", Kind: "tools", Cursor: in.Cursor}, {TenantID: "a", RequestID: "other", Kind: "tools", Cursor: in.Cursor}, {TenantID: "a", RequestID: "r", Kind: "outbox", Cursor: in.Cursor}} {
		if _, err = s.GetTaskRecordsPage(context.Background(), changed); !errors.Is(err, runtime.ErrInvalidEnvelope) {
			t.Fatalf("cursor crossed scope: %v", err)
		}
	}
	if err = m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestAllTaskRecordKindsRoundTripCompositeCursor(t *testing.T) {
	for _, tc := range []struct {
		kind, rows string
		want       []string
	}{
		{"attempts", `[{"attempt_no":1},{"attempt_no":2}]`, []string{"1"}},
		{"commits", `[{"agent_app_id":"app","session_id":"s","commit_id":"c1"},{"agent_app_id":"app","session_id":"s","commit_id":"c2"}]`, []string{"app", "s", "c1"}},
		{"outbox", `[{"outbox_id":"o1"},{"outbox_id":"o2"}]`, []string{"o1"}},
		{"deliveries", `[{"delivery_key":"reply","segment_no":0},{"delivery_key":"reply","segment_no":1}]`, []string{"reply", "0"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			db, m, _ := sqlmock.New()
			defer db.Close()
			in := TaskRecordsQuery{TenantID: "a", RequestID: "r", Kind: tc.kind, Limit: 1}
			after, _ := parseRecordsCursor(in)
			query, args := taskRecordsSQL(in, after, 2)
			values := make([]driver.Value, len(args))
			for i, a := range args {
				values[i] = a
			}
			m.ExpectQuery(regexp.QuoteMeta(query)).WithArgs(values...).WillReturnRows(sqlmock.NewRows([]string{"exists", "records", "observed"}).AddRow(true, tc.rows, time.Now()))
			page, err := New(db).GetTaskRecordsPage(context.Background(), in)
			if err != nil {
				t.Fatal(err)
			}
			in.Cursor = page.NextCursor
			got, err := parseRecordsCursor(in)
			if err != nil || len(got) != len(tc.want) {
				t.Fatal(got, err)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatal(got)
				}
			}
			if err = m.ExpectationsWereMet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestTaskRecordCursorRejectsUnknownKindOverflowAndMissingOriginal(t *testing.T) {
	in := TaskRecordsQuery{TenantID: "a", RequestID: "r", Kind: "attempts"}
	b, _ := json.Marshal(taskRecordsCursor{Version: 1, Tenant: "a", Request: "r", Kind: "attempts", After: []string{"9223372036854775808"}})
	in.Cursor = base64.RawURLEncoding.EncodeToString(b)
	if _, err := parseRecordsCursor(in); !errors.Is(err, runtime.ErrInvalidEnvelope) {
		t.Fatal(err)
	}
	in.Kind = "tools; DROP TABLE inbox"
	in.Cursor = ""
	if _, err := parseRecordsCursor(in); !errors.Is(err, runtime.ErrInvalidEnvelope) {
		t.Fatal(err)
	}
	db, m, _ := sqlmock.New()
	defer db.Close()
	m.ExpectQuery("WITH target AS").WithArgs("a", "r", int64(0), 51).WillReturnRows(sqlmock.NewRows([]string{"exists", "records", "observed"}).AddRow(false, `[]`, time.Now()))
	in.Kind = "attempts"
	if _, err := New(db).GetTaskRecordsPage(context.Background(), in); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
