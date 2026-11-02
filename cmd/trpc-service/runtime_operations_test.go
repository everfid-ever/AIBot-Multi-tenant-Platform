package main

import (
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	messagingpostgres "github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging/postgres"
)

func TestRuntimeOperationsRequireAdminAndExplicitScope(t *testing.T) {
	mux := http.NewServeMux()
	registerRuntimeOperations(mux, "admin-token", nil, nil, nil)
	for _, tc := range []struct {
		method, path, token, body string
		status                    int
	}{
		{"GET", "/ops/task/records?tenant_id=a&request_id=r&kind=tools", "", "", 401},
		{"GET", "/ops/task/records?tenant_id=a&request_id=r&kind=tools&limit=101", "admin-token", "", 400},
		{"GET", "/ops/task/records?tenant_id=a&request_id=r&kind=invalid", "admin-token", "", 400},
		{"GET", "/ops/task/records?tenant_id=a&request_id=r&kind=tools&cursor=bad", "admin-token", "", 400},
		{"GET", "/ops/task/incident?tenant_id=a&request_id=r", "", "", 401},
		{"GET", "/ops/task/incident?tenant_id=a", "admin-token", "", 400},
		{"GET", "/ops/reliability?tenant_id=a", "", "", 401},
		{"GET", "/ops/reliability", "admin-token", "", 400},
		{"GET", "/ops/session/lease?tenant_id=a", "admin-token", "", 400},
		{"POST", "/ops/delivery/resolve", "", "{}", 401},
		{"POST", "/ops/delivery/resolve", "admin-token", `{"tenant_id":"a","unknown":true}`, 400},
		{"POST", "/ops/delivery/resolve", "admin-token", `{} {}`, 400},
		{"POST", "/ops/delivery/resolve", "admin-token", `{"decision":"retry"}`, 400},
		{"GET", "/ops/delivery/incident?tenant_id=a&delivery_key=reply&segment_no=0", "", "", 401},
		{"GET", "/ops/delivery/incident?tenant_id=a&delivery_key=reply&segment_no=-1", "admin-token", "", 400},
		{"POST", "/ops/delivery/resolve", "admin-token", `{"tenant_id":"a","tenant_id":"b"}`, 400},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("X-TRPC-Local-Token", tc.token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != tc.status {
			t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
		}
	}
}

func TestResolutionRetryUsesAuthenticatedAdminNamespaceAndReturnsNoContent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectBegin()
	mock.ExpectQuery("UPDATE delivery_ledger").WithArgs("tenant-a", "reply", 0, int64(3), "sent", "provider", "operator_confirmed_delivered").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT actor,evidence_ref").WithArgs("tenant-a", "reply", 0, int64(3)).WillReturnRows(sqlmock.NewRows([]string{"actor", "evidence_ref", "decision", "provider_message_id"}).AddRow("deployment-admin/alice", "incident/123", "delivered", "provider"))
	mock.ExpectCommit()
	mux := http.NewServeMux()
	registerRuntimeOperations(mux, "admin-token", db, messagingpostgres.New(db), nil)
	r := httptest.NewRequest("POST", "/ops/delivery/resolve", strings.NewReader(`{"tenant_id":"tenant-a","delivery_key":"reply","segment_no":0,"expected_version":3,"actor":"alice","evidence_ref":"incident/123","decision":"delivered","provider_message_id":"provider"}`))
	r.Header.Set("X-TRPC-Local-Token", "admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 204 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestIncidentMissingRecordIs404WithoutExposingStorageError(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT COALESCE").WithArgs("tenant-a", "reply", 0).WillReturnError(sql.ErrNoRows)
	mux := http.NewServeMux()
	registerRuntimeOperations(mux, "admin-token", db, messagingpostgres.New(db), nil)
	r := httptest.NewRequest("GET", "/ops/delivery/incident?tenant_id=tenant-a&delivery_key=reply&segment_no=0", nil)
	r.Header.Set("X-TRPC-Local-Token", "admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(w)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRuntimeOperationsScopesSnapshotToTenant(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	mock.ExpectQuery("SELECT jsonb_build_object").WithArgs("tenant-a").WillReturnRows(sqlmock.NewRows([]string{"snapshot"}).AddRow([]byte(`{"tenant_id":"tenant-a","deliveries":{"ambiguous":1}}`)))
	mux := http.NewServeMux()
	registerRuntimeOperations(mux, "admin-token", db, messagingpostgres.New(db), nil)
	r := httptest.NewRequest("GET", "/ops/reliability?tenant_id=tenant-a", nil)
	r.Header.Set("X-TRPC-Local-Token", "admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" || !strings.Contains(w.Body.String(), "ambiguous") {
		t.Fatal(w)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
