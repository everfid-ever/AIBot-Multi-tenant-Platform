package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/storage/messaging"
)

type ResolutionEvidence struct {
	DeliveryResolution
	CreatedAt time.Time `json:"created_at"`
}
type DeliveryIncident struct {
	Delivery    messaging.DeliveryRecord   `json:"delivery"`
	Receipt     *messaging.ProviderReceipt `json:"receipt,omitempty"`
	Resolutions []ResolutionEvidence       `json:"resolutions"`
}

// Metadata only: no reply plaintext, raw credential, or sender token.
func (s *Store) GetDeliveryIncident(ctx context.Context, k messaging.DeliveryKey) (DeliveryIncident, error) {
	var incident DeliveryIncident
	if k.TenantID == "" || len(k.TenantID) > 256 || k.DeliveryKey == "" || len(k.DeliveryKey) > 1024 || k.SegmentNo < 0 || k.SegmentNo > 1<<31-1 {
		return incident, runtime.ErrInvalidEnvelope
	}
	r, err := s.GetDelivery(ctx, k)
	if err != nil {
		return incident, err
	}
	incident.Delivery = r
	receipt, err := s.GetProviderReceipt(ctx, k)
	if err == nil {
		incident.Receipt = &receipt
	} else if !errors.Is(err, runtime.ErrNotFound) {
		return incident, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT expected_version,actor,evidence_ref,decision,COALESCE(provider_message_id,''),created_at
FROM delivery_resolution WHERE tenant_id=$1 AND delivery_key=$2 AND segment_no=$3 ORDER BY expected_version DESC LIMIT 100`, k.TenantID, k.DeliveryKey, k.SegmentNo)
	if err != nil {
		return incident, err
	}
	defer rows.Close()
	incident.Resolutions = []ResolutionEvidence{}
	for rows.Next() {
		e := ResolutionEvidence{DeliveryResolution: DeliveryResolution{TenantID: k.TenantID, DeliveryKey: k.DeliveryKey, SegmentNo: k.SegmentNo}}
		if err = rows.Scan(&e.ExpectedVersion, &e.Actor, &e.EvidenceRef, &e.Decision, &e.ProviderMessageID, &e.CreatedAt); err != nil {
			return incident, err
		}
		incident.Resolutions = append(incident.Resolutions, e)
	}
	return incident, rows.Err()
}
