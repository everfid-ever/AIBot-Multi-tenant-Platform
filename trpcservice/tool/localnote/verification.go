package localnote

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

func (Tool) TaskFinalEvidenceChecks() []*taskflow.ResultCheck {
	return []*taskflow.ResultCheck{{Path: []string{"note_id"}, NonEmpty: true}}
}

// VerifyBusinessResult independently reads the actual business row. Compare
// both original approved arguments and the candidate receipt, not a text status.
func (t Tool) VerifyBusinessResult(ctx context.Context, args, content []byte) error {
	e, ok := runtime.ExecutionContextFrom(ctx)
	if !ok {
		return runtime.ErrTenantScope
	}
	canonical, digest, err := governance.CanonicalArguments(args)
	if err != nil || digest != e.ArgsDigest {
		return runtime.ErrVersionMismatch
	}
	var in input
	decoder := json.NewDecoder(bytes.NewReader(canonical))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&in) != nil {
		return runtime.ErrInvalidEnvelope
	}
	key, err := t.RecoveryKey(ctx)
	if err != nil {
		return err
	}
	expected := Result{Status: "created", NoteID: "note_" + key[:24], Title: strings.TrimSpace(in.Title), Content: strings.TrimSpace(in.Content)}
	canonicalReceipt, _, receiptErr := governance.CanonicalArguments(content)
	if receiptErr != nil {
		return runtime.ErrVersionMismatch
	}
	var receipt Result
	receiptDecoder := json.NewDecoder(bytes.NewReader(canonicalReceipt))
	receiptDecoder.DisallowUnknownFields()
	if receiptDecoder.Decode(&receipt) != nil || receipt != expected {
		return runtime.ErrVersionMismatch
	}
	reader, ok := t.Writer.(Reader)
	if !ok {
		return runtime.ErrCapabilityUnsupported
	}
	observed, err := reader.Lookup(ctx, LookupKey{TenantID: e.TenantID, RequestID: e.RequestID, BusinessKey: key, ArgsDigest: digest})
	if err != nil {
		return err
	}
	if observed != expected {
		return runtime.ErrVersionMismatch
	}
	return nil
}
