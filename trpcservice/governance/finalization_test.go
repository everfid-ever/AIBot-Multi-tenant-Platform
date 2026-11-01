package governance

import (
	"math"
	"testing"
)

func TestFrozenFinalizationBindsPermitUsageDecisionAndOutput(t *testing.T) {
	p := RunPermit{Policy: PolicySnapshot{TenantID: "tenant", Version: 1}, Decision: Decision{Action: ActionAllow}, Reservation: Reservation{TenantID: "tenant", RequestID: "request", ReservationID: "reservation", PolicyVersion: 1, Version: 1, ReservedCostMicros: 100, ReservedTokens: 100}}
	u := []ModelUsage{{Model: VersionedRef{ID: "model", Version: 1}, Usage: Usage{InputTokens: 10, OutputTokens: 5}}}
	f := Finalization{Settlement: SettleRequest{TenantID: "tenant", RequestID: "request", ReservationID: "reservation", Stage: "model", UsageKind: "tokens", ExpectedVersion: 1, Usage: u[0].Usage, ActualCostMicros: 5}, Decision: Decision{TenantID: "tenant", RequestID: "request", ReservationID: "reservation", PolicyVersion: 1, Stage: "output", Action: ActionAllow, ReasonCode: ReasonAllowed, DecisionID: StableDecisionID("tenant", "request", "output", 1)}, OutputDigest: contentDigest([]byte("answer"))}
	if err := f.Validate(p, u, []byte("answer")); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"request", "reservation", "stage", "usage", "cost", "decision", "output"} {
		x := f
		output := []byte("answer")
		switch field {
		case "request":
			x.Settlement.RequestID = "other"
		case "reservation":
			x.Settlement.ReservationID = "other"
		case "stage":
			x.Settlement.Stage = "tool"
		case "usage":
			x.Settlement.Usage.InputTokens++
		case "cost":
			x.Settlement.ActualCostMicros = 101
		case "decision":
			x.Decision.ReasonCode = ReasonOutputRejected
		case "output":
			output = []byte("changed")
		}
		if err := x.Validate(p, u, output); err != ErrFinalizationConflict {
			t.Fatalf("accepted changed %s: %v", field, err)
		}
	}
}
func TestUsageAggregationRejectsOverflowAndInvalidCachedTokens(t *testing.T) {
	for _, usages := range [][]ModelUsage{
		{{Model: VersionedRef{ID: "a", Version: 1}, Usage: Usage{InputTokens: math.MaxInt64}}, {Model: VersionedRef{ID: "b", Version: 1}, Usage: Usage{InputTokens: 1}}},
		{{Model: VersionedRef{ID: "a", Version: 1}, Usage: Usage{InputTokens: math.MaxInt64, OutputTokens: 1}}},
		{{Model: VersionedRef{ID: "a", Version: 1}, Usage: Usage{InputTokens: 1, CachedInputTokens: 2}}},
	} {
		if _, err := aggregateModelUsage(usages); err == nil {
			t.Fatal("invalid metering accepted")
		}
	}
}
