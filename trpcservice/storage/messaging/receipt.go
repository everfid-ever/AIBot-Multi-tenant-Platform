package messaging

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

// ProviderReceipt is immutable evidence of an acknowledged provider send,
// separate from the mutable Delivery Ledger. It does not prove client display.
type ProviderReceipt struct {
	DeliveryKey
	ClientRequestID, ProviderMessageID, PlanDigest, RouteDigest, SegmentDigest string
	CreatedAt                                                                  time.Time
}
type ProviderReceiptStore interface {
	PutProviderReceipt(context.Context, DeliveryRecord, ProviderReceipt) error
	GetProviderReceipt(context.Context, DeliveryKey) (ProviderReceipt, error)
}

func ReceiptDigest(value any) string {
	b, _ := json.Marshal(value)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
func (r ProviderReceipt) Validate() error {
	if r.TenantID == "" || r.DeliveryKey.DeliveryKey == "" || r.SegmentNo < 0 || r.ClientRequestID == "" || strings.TrimSpace(r.ProviderMessageID) == "" || len(r.ProviderMessageID) > 1024 {
		return runtime.ErrInvalidEnvelope
	}
	for _, d := range []string{r.PlanDigest, r.RouteDigest, r.SegmentDigest} {
		b, err := hex.DecodeString(d)
		if err != nil || len(b) != sha256.Size || d != strings.ToLower(d) {
			return runtime.ErrInvalidEnvelope
		}
	}
	return nil
}
