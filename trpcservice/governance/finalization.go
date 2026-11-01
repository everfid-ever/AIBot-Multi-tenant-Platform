package governance

import (
	"errors"
	"math"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

// ErrFinalizationConflict is a permanent contradiction in a frozen settlement,
// not an infrastructure timeout. A worker must not rerun the model to repair it.
var ErrFinalizationConflict = errors.New("frozen finalization conflicts with reservation or usage")

func aggregateModelUsage(usages []ModelUsage) (Usage, error) {
	var total Usage
	if len(usages) == 0 {
		return total, runtime.ErrInvariantViolation
	}
	for _, m := range usages {
		u := m.Usage
		if m.Model.ID == "" || m.Model.Version < 1 || u.InputTokens < 0 || u.OutputTokens < 0 || u.CachedInputTokens < 0 || u.CachedInputTokens > u.InputTokens || u.InputTokens > math.MaxInt64-total.InputTokens || u.OutputTokens > math.MaxInt64-total.OutputTokens || u.CachedInputTokens > math.MaxInt64-total.CachedInputTokens {
			return Usage{}, runtime.ErrInvariantViolation
		}
		total.InputTokens += u.InputTokens
		total.OutputTokens += u.OutputTokens
		total.CachedInputTokens += u.CachedInputTokens
	}
	if total.OutputTokens > math.MaxInt64-total.InputTokens {
		return Usage{}, runtime.ErrInvariantViolation
	}
	return total, nil
}

// Validate binds the exact reviewed output and metering to the original permit.
// It deliberately does not rescan content or reprice at takeover time.
func (f Finalization) Validate(p RunPermit, usages []ModelUsage, output []byte) error {
	u, err := aggregateModelUsage(usages)
	r, d := f.Settlement, f.Decision
	if err != nil || f.OutputDigest != contentDigest(output) {
		return ErrFinalizationConflict
	}
	if p.Decision.Action != ActionAllow || p.Reservation.TenantID != p.Policy.TenantID ||
		p.Reservation.RequestID == "" || p.Reservation.ReservationID == "" ||
		p.Reservation.PolicyVersion != p.Policy.Version || p.Reservation.Version < 1 {
		return ErrFinalizationConflict
	}
	if r.Usage != u || r.Stage != "model" || r.UsageKind != "tokens" ||
		r.ExpectedVersion != p.Reservation.Version || r.ReservationID != p.Reservation.ReservationID ||
		r.TenantID != p.Policy.TenantID || r.RequestID != p.Reservation.RequestID {
		return ErrFinalizationConflict
	}
	if d.TenantID != r.TenantID || d.RequestID != r.RequestID || d.ReservationID != r.ReservationID ||
		d.PolicyVersion != p.Policy.Version || d.Stage != "output" ||
		d.DecisionID != StableDecisionID(d.TenantID, d.RequestID, "output", d.PolicyVersion) {
		return ErrFinalizationConflict
	}
	if r.ActualCostMicros < 0 || r.ActualCostMicros > p.Reservation.ReservedCostMicros ||
		(p.Reservation.ReservedTokens > 0 && u.InputTokens+u.OutputTokens > p.Reservation.ReservedTokens) {
		return ErrFinalizationConflict
	}
	if d.Action == ActionAllow && d.ReasonCode == ReasonAllowed || d.Action == ActionDeny && d.ReasonCode == ReasonOutputRejected {
		return nil
	}
	return ErrFinalizationConflict
}
