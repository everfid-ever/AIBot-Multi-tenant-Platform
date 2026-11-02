package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	messagingpostgres "github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging/postgres"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	taskmemory "github.com/liuzengh/trpc-agent-service/trpcservice/taskflow/inmemory"
)

func TestTaskIncidentReturnsOnlyMetadataAndReportsSeparateCheckpointRead(t *testing.T) {
	ctx := context.Background()
	plans := taskmemory.New()
	scope := taskflow.Scope{Key: taskflow.Key{TenantID: "a", RequestID: "r"}, Fence: 1}
	p, err := plans.Open(ctx, scope, taskflow.Digest([]byte("private input")), 1)
	if err != nil {
		t.Fatal(err)
	}
	s := taskflow.Session{Store: plans, Scope: scope}
	p, err = s.BindAcceptance(ctx, taskflow.Acceptance{Version: 1, InputDigest: p.InputDigest, Objective: "SECRET ORIGINAL GOAL", Requirements: []taskflow.Requirement{{ID: "required", Description: "SECRET DESCRIPTION", OutputContains: []string{"SECRET ANSWER"}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshotVersion := range []int64{p.Version, p.Version - 1} {
		db, m, _ := sqlmock.New()
		mux := http.NewServeMux()
		registerRuntimeOperations(mux, "admin-token", db, messagingpostgres.New(db), nil, plans)
		meta, _ := json.Marshal(map[string]any{"tenant_id": "a", "request_id": "r", "plan": map[string]any{"version": snapshotVersion, "fence": 1}})
		m.ExpectQuery("WITH target AS").WithArgs("a", "r").WillReturnRows(sqlmock.NewRows([]string{"metadata"}).AddRow(meta))
		r := httptest.NewRequest("GET", "/ops/task/incident?tenant_id=a&request_id=r", nil)
		r.Header.Set("X-TRPC-Local-Token", "admin-token")
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		status := "matches_snapshot"
		if snapshotVersion != p.Version {
			status = "changed_since_snapshot"
		}
		if w.Code != 200 || !strings.Contains(w.Body.String(), status) || strings.Contains(w.Body.String(), "SECRET") || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal(w.Code, w.Body.String())
		}
		if err = m.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
		db.Close()
	}
}

func TestTaskIncidentMissingOriginalRequestReturnsNotFound(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer db.Close()
	mux := http.NewServeMux()
	registerRuntimeOperations(mux, "admin-token", db, messagingpostgres.New(db), nil)
	m.ExpectQuery("WITH target AS").WithArgs("other", "r").WillReturnError(sql.ErrNoRows)
	r := httptest.NewRequest("GET", "/ops/task/incident?tenant_id=other&request_id=r", nil)
	r.Header.Set("X-TRPC-Local-Token", "admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 404 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestTaskRecordPageIsAuthenticatedMetadataWithContinuation(t *testing.T) {
	db, m, _ := sqlmock.New()
	defer db.Close()
	mux := http.NewServeMux()
	registerRuntimeOperations(mux, "admin-token", db, messagingpostgres.New(db), nil)
	m.ExpectQuery("WITH target AS").WithArgs("a", "r", "", 2).WillReturnRows(sqlmock.NewRows([]string{"exists", "records", "observed"}).AddRow(true, `[{"tool_call_id":"op1","state":"running"},{"tool_call_id":"op2","state":"succeeded"}]`, time.Now()))
	r := httptest.NewRequest("GET", "/ops/task/records?tenant_id=a&request_id=r&kind=tools&limit=1", nil)
	r.Header.Set("X-TRPC-Local-Token", "admin-token")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	var page messagingpostgres.TaskRecordsPage
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Records) != 1 || page.NextCursor == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
