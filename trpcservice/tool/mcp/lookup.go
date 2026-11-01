package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"

	"github.com/liuzengh/trpc-agent-service/trpcservice/governance"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	servicetool "github.com/liuzengh/trpc-agent-service/trpcservice/tool"
)

type LookupConfig struct {
	ToolName                  string
	ExpectedDeclarationDigest string
	KeyParameter              string
}

func (c callable) RecoveryLookupEnabled() bool { return c.config.Lookup != nil }

func (c callable) LookupRecovery(ctx context.Context, key string) (servicetool.RecoveryResult, error) {
	lookup := c.config.Lookup
	if lookup == nil {
		return servicetool.RecoveryResult{}, runtime.ErrCapabilityUnsupported
	}
	expected, err := c.RecoveryKey(ctx)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	if key == "" || key != expected {
		return servicetool.RecoveryResult{}, runtime.ErrVersionMismatch
	}
	set, remote, err := c.openNamed(ctx, lookup.ToolName, lookup.ExpectedDeclarationDigest, lookup.KeyParameter)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	defer set.Close()
	args, _ := json.Marshal(map[string]string{lookup.KeyParameter: key})
	r, err := remote.Call(ctx, args)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	if failure, ok := r.(interface{ RetryResultError() bool }); ok && failure.RetryResultError() {
		return servicetool.RecoveryResult{}, runtime.ErrEffectUnknown
	}
	b, err := json.Marshal(r)
	if err != nil || len(b) > maxResultBytes {
		return servicetool.RecoveryResult{}, runtime.ErrCapabilityUnsupported
	}
	object, err := decodeJSONObject(b)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	b, err = json.Marshal(object)
	if err != nil {
		return servicetool.RecoveryResult{}, err
	}
	var receipt struct {
		OperationKey string                    `json:"operation_key"`
		State        servicetool.RecoveryState `json:"state"`
		Result       json.RawMessage           `json:"result,omitempty"`
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err = d.Decode(&receipt); err != nil || receipt.OperationKey != key {
		return servicetool.RecoveryResult{}, runtime.ErrVersionMismatch
	}
	if d.Decode(new(any)) != io.EOF {
		return servicetool.RecoveryResult{}, runtime.ErrInvariantViolation
	}
	result := servicetool.RecoveryResult{State: receipt.State}
	switch receipt.State {
	case servicetool.RecoveryCompleted:
		if len(receipt.Result) == 0 || bytes.Equal(bytes.TrimSpace(receipt.Result), []byte("null")) {
			return result, runtime.ErrInvariantViolation
		}
		decoder := json.NewDecoder(bytes.NewReader(receipt.Result))
		decoder.UseNumber()
		if err = decoder.Decode(&result.Result); err != nil {
			return result, err
		}
	case servicetool.RecoveryNotFound, servicetool.RecoveryPending, servicetool.RecoveryFailed, servicetool.RecoveryUnknown:
		if len(receipt.Result) > 0 && !bytes.Equal(bytes.TrimSpace(receipt.Result), []byte("null")) {
			return result, runtime.ErrInvariantViolation
		}
	default:
		return result, runtime.ErrInvariantViolation
	}
	return result, nil
}

// Only an object or exactly one MCP text content block containing an object is
// admitted. Multiple blocks, prose prefixes and malformed JSON are ambiguous.
func decodeJSONObject(b []byte) (map[string]any, error) {
	canonical, _, err := governance.CanonicalArguments(b)
	if err != nil {
		return nil, runtime.ErrInvariantViolation
	}
	b = canonical
	object, err := decodeObject(b)
	if err == nil {
		return object, nil
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(b, &blocks); err != nil || len(blocks) != 1 || blocks[0].Type != "text" {
		return nil, runtime.ErrInvariantViolation
	}
	return decodeObject([]byte(blocks[0].Text))
}

func decodeObject(b []byte) (map[string]any, error) {
	canonical, _, err := governance.CanonicalArguments(b)
	if err != nil {
		return nil, runtime.ErrInvariantViolation
	}
	b = canonical
	var object map[string]any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&object); err != nil || object == nil {
		return nil, runtime.ErrInvariantViolation
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, runtime.ErrInvariantViolation
	}
	return object, nil
}

var _ servicetool.RecoveryLookup = callable{}
