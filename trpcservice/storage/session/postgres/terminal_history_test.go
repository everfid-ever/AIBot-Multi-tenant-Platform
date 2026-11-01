package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	sessionstore "github.com/liuzengh/trpc-agent-service/trpcservice/storage/session"
)

func TestTerminalHistoryQueryIsScopedOrderedAndBounded(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	key := sessionstore.SessionKey{TenantID: "tenant", AgentAppID: "app", SessionID: "session"}
	mock.ExpectQuery("SELECT request_id,commit_id,input_seq,fence,outcome,COALESCE").WithArgs("tenant", "app", "session", int64(5), 2).
		WillReturnRows(sqlmock.NewRows([]string{"request_id", "commit_id", "input_seq", "fence", "outcome", "result_ref", "created_at"}).AddRow("r3", "c3", 3, 4, "succeeded", "result://r3", time.Now()).AddRow("r4", "c4", 4, 8, "cancelled", "", time.Now()))
	rows, err := New(db).ListTerminalHistory(context.Background(), key, 5, 2)
	if err != nil || len(rows) != 2 || rows[0].InputSeq != 3 || rows[1].Fence != 8 {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if _, err = New(db).ListTerminalHistory(context.Background(), key, 5, sessionstore.MaxHistoryTurns+1); err == nil {
		t.Fatal("unbounded read allowed")
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
