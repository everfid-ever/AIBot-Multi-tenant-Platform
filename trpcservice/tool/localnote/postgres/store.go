package postgres

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
	"github.com/liuzengh/trpc-agent-service/trpcservice/tool/localnote"
)

type Store struct {
	DB   *sql.DB
	Keys messaging.PayloadKeyResolver
}

func (s Store) aead(ctx context.Context, tenant string, version int64) (cipher.AEAD, error) {
	if s.Keys == nil || tenant == "" || version < 1 {
		return nil, runtime.ErrCapabilityUnsupported
	}
	key, err := s.Keys.ResolvePayloadKey(ctx, tenant, version)
	if err != nil {
		return nil, err
	}
	defer clear(key.Bytes)
	if key.Version != version || len(key.Bytes) != 32 {
		return nil, runtime.ErrVersionMismatch
	}
	block, err := aes.NewCipher(key.Bytes)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func noteAAD(k localnote.LookupKey, version int64) []byte {
	b, _ := json.Marshal([]any{"local-note-v1", k.TenantID, k.RequestID, k.BusinessKey, k.ArgsDigest, version})
	return b
}
func (s Store) decode(ctx context.Context, k localnote.LookupKey, id string, encrypted, nonce []byte, version int64) (localnote.Result, error) {
	a, err := s.aead(ctx, k.TenantID, version)
	if err != nil {
		return localnote.Result{}, err
	}
	if len(nonce) != a.NonceSize() || len(encrypted) < a.Overhead() || len(encrypted) > 64<<10 {
		return localnote.Result{}, runtime.ErrInvariantViolation
	}
	b, err := a.Open(nil, nonce, encrypted, noteAAD(k, version))
	if err != nil {
		return localnote.Result{}, runtime.ErrInvariantViolation
	}
	defer clear(b)
	var r localnote.Result
	if err = json.Unmarshal(b, &r); err != nil || r.NoteID != id || r.Status != "created" {
		return localnote.Result{}, runtime.ErrVersionMismatch
	}
	return r, nil
}

// Create returns the authenticated EXISTING result on key collision; it must
// not claim success merely by echoing the current caller's desired content.
func (s Store) Create(ctx context.Context, in localnote.Note) (localnote.Result, error) {
	if in.Result.Status != "created" || in.Result.NoteID == "" || len(in.Result.Title) > 200 || len(in.Result.Content) > 4000 {
		return localnote.Result{}, runtime.ErrInvalidEnvelope
	}
	if s.DB == nil || in.TenantID == "" || in.BusinessKey == "" || in.RequestID == "" || in.ArgsDigest == "" || in.KeyVersion < 1 {
		return localnote.Result{}, runtime.ErrCapabilityUnsupported
	}
	a, err := s.aead(ctx, in.TenantID, in.KeyVersion)
	if err != nil {
		return localnote.Result{}, err
	}
	k := localnote.LookupKey{TenantID: in.TenantID, RequestID: in.RequestID, BusinessKey: in.BusinessKey, ArgsDigest: in.ArgsDigest}
	b, err := json.Marshal(in.Result)
	if err != nil {
		return localnote.Result{}, err
	}
	defer clear(b)
	nonce := make([]byte, a.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return localnote.Result{}, err
	}
	encrypted := a.Seal(nil, nonce, b, noteAAD(k, in.KeyVersion))
	var request, digest, id string
	var saved, iv []byte
	var version int64
	err = s.DB.QueryRowContext(ctx, `INSERT INTO local_note(tenant_id,business_key,request_id,note_id,args_digest,ciphertext,nonce,key_version)
VALUES($1,$2,$3,$4,$5,$6,$7,$8) ON CONFLICT(tenant_id,business_key) DO UPDATE SET business_key=EXCLUDED.business_key
RETURNING request_id,note_id,args_digest,ciphertext,nonce,key_version`, in.TenantID, in.BusinessKey, in.RequestID, in.Result.NoteID, in.ArgsDigest, encrypted, nonce, in.KeyVersion).Scan(&request, &id, &digest, &saved, &iv, &version)
	if err != nil {
		return localnote.Result{}, err
	}
	if request != in.RequestID || digest != in.ArgsDigest || id != in.Result.NoteID {
		return localnote.Result{}, runtime.ErrIdempotencyCollision
	}
	r, err := s.decode(ctx, k, id, saved, iv, version)
	if err != nil {
		return localnote.Result{}, err
	}
	if r != in.Result {
		return localnote.Result{}, runtime.ErrIdempotencyCollision
	}
	return r, nil
}

func (s Store) Lookup(ctx context.Context, k localnote.LookupKey) (localnote.Result, error) {
	if s.DB == nil {
		return localnote.Result{}, runtime.ErrCapabilityUnsupported
	}
	if k.TenantID == "" || k.RequestID == "" || k.BusinessKey == "" || k.ArgsDigest == "" {
		return localnote.Result{}, runtime.ErrTenantScope
	}
	var request, id, digest string
	var encrypted, nonce []byte
	var version int64
	err := s.DB.QueryRowContext(ctx, `SELECT request_id,note_id,args_digest,ciphertext,nonce,key_version FROM local_note WHERE tenant_id=$1 AND business_key=$2`, k.TenantID, k.BusinessKey).Scan(&request, &id, &digest, &encrypted, &nonce, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return localnote.Result{}, runtime.ErrNotFound
	}
	if err != nil {
		return localnote.Result{}, err
	}
	if request != k.RequestID || digest != k.ArgsDigest {
		return localnote.Result{}, runtime.ErrIdempotencyCollision
	}
	return s.decode(ctx, k, id, encrypted, nonce, version)
}

var _ localnote.Reader = Store{}
