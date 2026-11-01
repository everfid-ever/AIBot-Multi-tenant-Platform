package postgres

import (
	"bytes"
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/localnote"
)

type keys struct{}

func (keys) ResolvePayloadKey(context.Context, string, int64) (messaging.PayloadCipherKey, error) {
	return messaging.PayloadCipherKey{Bytes: bytes.Repeat([]byte{3}, 32), Version: 1}, nil
}

type encryptedArgument struct{}

func (encryptedArgument) Match(v driver.Value) bool {
	data, ok := v.([]byte)
	return ok && len(data) > 16 && !bytes.Contains(data, []byte("private note"))
}
func TestNoteEffectUsesStableBusinessKeyAndRejectsCollision(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	store := Store{DB: db, Keys: keys{}}
	note := localnote.Note{TenantID: "tenant", RequestID: "request", BusinessKey: "business", ArgsDigest: "digest", KeyVersion: 1, Result: localnote.Result{NoteID: "note-1", Status: "created", Content: "private note"}}
	ciphertext, nonce := noteCipher(t, store, note, 1)
	for i := 0; i < 2; i++ {
		mock.ExpectQuery("INSERT INTO local_note").WithArgs("tenant", "business", "request", "note-1", "digest", encryptedArgument{}, sqlmock.AnyArg(), int64(1)).WillReturnRows(sqlmock.NewRows([]string{"request_id", "note_id", "args_digest", "ciphertext", "nonce", "key_version"}).AddRow("request", "note-1", "digest", ciphertext, nonce, 1))
		if result, err := store.Create(context.Background(), note); err != nil || result.NoteID != "note-1" {
			t.Fatalf("%+v %v", result, err)
		}
	}
	mock.ExpectQuery("INSERT INTO local_note").WillReturnRows(sqlmock.NewRows([]string{"request_id", "note_id", "args_digest", "ciphertext", "nonce", "key_version"}).AddRow("request", "note-1", "other-digest", ciphertext, nonce, 1))
	if _, err := store.Create(context.Background(), note); !errors.Is(err, runtime.ErrIdempotencyCollision) {
		t.Fatal(err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func noteCipher(t *testing.T, s Store, n localnote.Note, version int64) ([]byte, []byte) {
	t.Helper()
	a, err := s.aead(context.Background(), n.TenantID, version)
	if err != nil {
		t.Fatal(err)
	}
	iv := bytes.Repeat([]byte{9}, a.NonceSize())
	b, _ := json.Marshal(n.Result)
	return a.Seal(nil, iv, b, noteAAD(localnote.LookupKey{TenantID: n.TenantID, RequestID: n.RequestID, BusinessKey: n.BusinessKey, ArgsDigest: n.ArgsDigest}, version)), iv
}

type rotatingKeys struct{}

func (rotatingKeys) ResolvePayloadKey(_ context.Context, _ string, v int64) (messaging.PayloadCipherKey, error) {
	return messaging.PayloadCipherKey{Bytes: bytes.Repeat([]byte{byte(v)}, 32), Version: v}, nil
}

func TestLookupAuthenticatesStoredEffectAndCreateCannotEchoUnverifiedSuccess(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	s := Store{DB: db, Keys: rotatingKeys{}}
	n := localnote.Note{TenantID: "tenant", RequestID: "request", BusinessKey: "business", ArgsDigest: "digest", KeyVersion: 1, Result: localnote.Result{Status: "created", NoteID: "note-1", Title: "original", Content: "private note"}}
	c, iv := noteCipher(t, s, n, 1)
	k := localnote.LookupKey{TenantID: n.TenantID, RequestID: n.RequestID, BusinessKey: n.BusinessKey, ArgsDigest: n.ArgsDigest}
	rows := func(request, digest string, cipher []byte) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"request_id", "note_id", "args_digest", "ciphertext", "nonce", "key_version"}).AddRow(request, "note-1", digest, cipher, iv, 1)
	}
	mock.ExpectQuery("SELECT request_id").WithArgs("tenant", "business").WillReturnRows(rows("request", "digest", c))
	if r, err := s.Lookup(context.Background(), k); err != nil || r != n.Result {
		t.Fatalf("lookup=%+v %v", r, err)
	}
	// An idempotent retry after rotation must decrypt the original key version.
	n.KeyVersion = 2
	mock.ExpectQuery("INSERT INTO local_note").WillReturnRows(rows("request", "digest", c))
	if r, err := s.Create(context.Background(), n); err != nil || r != n.Result {
		t.Fatalf("rotation replay=%+v %v", r, err)
	}
	n.Result.Title = "different incoming title"
	mock.ExpectQuery("INSERT INTO local_note").WillReturnRows(rows("request", "digest", c))
	if _, err = s.Create(context.Background(), n); !errors.Is(err, runtime.ErrIdempotencyCollision) {
		t.Fatal("echoed incoming result instead of stored receipt", err)
	}
	tampered := append([]byte(nil), c...)
	tampered[0] ^= 1
	mock.ExpectQuery("SELECT request_id").WillReturnRows(rows("request", "digest", tampered))
	if _, err = s.Lookup(context.Background(), k); !errors.Is(err, runtime.ErrInvariantViolation) {
		t.Fatal("accepted corrupted effect", err)
	}
	mock.ExpectQuery("SELECT request_id").WillReturnRows(rows("other-request", "digest", c))
	if _, err = s.Lookup(context.Background(), k); !errors.Is(err, runtime.ErrIdempotencyCollision) {
		t.Fatal("cross-request effect", err)
	}
	mock.ExpectQuery("SELECT request_id").WillReturnRows(sqlmock.NewRows([]string{"request_id", "note_id", "args_digest", "ciphertext", "nonce", "key_version"}))
	if _, err = s.Lookup(context.Background(), k); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal("absence not distinguished", err)
	}
	if err = mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
