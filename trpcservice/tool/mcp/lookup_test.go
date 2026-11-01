package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/liuzengh/trpc-agent-service/trpcservice/profile"
	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
	"github.com/liuzengh/trpc-agent-service/trpcservice/secrets"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	servicetool "github.com/liuzengh/trpc-agent-service/trpcservice/tool"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool"
	tmcp "trpc.group/trpc-go/trpc-mcp-go"
)

type lookupProtocol struct {
	state                                string
	wrongKey, failed, changedDeclaration bool
	writes, queries                      int
	keys                                 []string
	headers                              []http.Header
}

func (h *lookupProtocol) Handle(_ context.Context, _ *http.Client, r *http.Request) (*http.Response, error) {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil, err
	}
	var req struct {
		ID     any    `json:"id"`
		Method string `json:"method"`
		Params struct {
			Name      string            `json:"name"`
			Arguments map[string]string `json:"arguments"`
		} `json:"params"`
	}
	if err = json.Unmarshal(b, &req); err != nil {
		return nil, err
	}
	h.headers = append(h.headers, r.Header.Clone())
	if req.ID == nil {
		return mcpResponse(http.StatusAccepted, nil, nil), nil
	}
	switch req.Method {
	case "initialize":
		return mcpResponse(http.StatusOK, req.ID, map[string]any{"protocolVersion": "2025-03-26", "serverInfo": map[string]any{"name": "recovery", "version": "1"}, "capabilities": map[string]any{}}), nil
	case "tools/list":
		lookup := map[string]any{"name": "read_operation", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"operation_key": map[string]any{"type": "string"}}}}
		if h.changedDeclaration {
			lookup["description"] = "unreviewed change"
		}
		return mcpResponse(http.StatusOK, req.ID, map[string]any{"tools": []any{map[string]any{"name": "create_record", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"operation_key": map[string]any{"type": "string"}}}}, lookup}}), nil
	case "tools/call":
		key := req.Params.Arguments["operation_key"]
		if key == "" {
			return nil, errors.New("missing trusted operation key")
		}
		h.keys = append(h.keys, key)
		var value any
		if req.Params.Name == "read_operation" {
			h.queries++
			if h.wrongKey {
				key = "different-operation"
			}
			receipt := map[string]any{"operation_key": key, "state": h.state}
			if h.state == "completed" {
				receipt["result"] = map[string]any{"status": "created", "record_id": json.Number("9007199254740993")}
			}
			value = receipt
		} else if req.Params.Name == "create_record" {
			h.writes++
			value = map[string]any{"status": "created", "record_id": json.Number("9007199254740993")}
		} else {
			return nil, errors.New("unexpected tool")
		}
		encoded, _ := json.Marshal(value)
		return mcpResponse(http.StatusOK, req.ID, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(encoded)}}, "isError": h.failed}), nil
	default:
		return mcpResponse(http.StatusOK, req.ID, map[string]any{}), nil
	}
}

func TestMCPStatusLookupThroughRealSDKKeepsKeyScopeAndExactIDs(t *testing.T) {
	h := &lookupProtocol{state: "completed"}
	digest := func(name string) string {
		d, err := DeclarationDigest(&agenttool.Declaration{Name: name, InputSchema: &agenttool.Schema{Type: "object", Properties: map[string]*agenttool.Schema{"operation_key": {Type: "string"}}}})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	c := Config{Transport: "streamable", ServerURL: "https://mcp.test/tools", RemoteToolName: "create_record", ExpectedDeclarationDigest: digest("create_record"), Timeout: time.Second, IdempotencyParameter: "operation_key", CompletionCheck: &taskflow.ResultCheck{Path: []string{"status"}, EqualsJSON: `"created"`}, JSONTextResult: true, Lookup: &LookupConfig{ToolName: "read_operation", ExpectedDeclarationDigest: digest("read_operation"), KeyParameter: "operation_key"}, SecretHeader: "Authorization", SecretPrefix: "Bearer ", clientOptions: []tmcp.ClientOption{tmcp.WithHTTPReqHandler(h)}}
	reg, err := NewRegistration("tenant-a", "create_record", 1, c, secrets.SecretRef{Ref: "secret://provider", Version: 1})
	if err != nil {
		t.Fatal(err)
	}
	c.Lookup.KeyParameter = "mutated after registration"
	catalog, err := servicetool.NewCatalog(reg)
	if err != nil {
		t.Fatal(err)
	}
	provider := &recordingSecrets{value: []byte("scoped-token")}
	ctx := runtime.WithExecutionContext(context.Background(), runtime.ExecutionContext{TenantID: "tenant-a", RequestID: "request", SubjectID: "worker", ToolCallID: "stable-op", ArgsDigest: taskflow.Digest([]byte(`{}`))})
	values, err := (servicetool.Resolver{Catalog: catalog, Secrets: provider}).ResolveTools(ctx, "tenant-a", []profile.VersionedRef{{ID: "create_record", Version: 1, ContentDigest: reg.ContentDigest}})
	if err != nil {
		t.Fatal(err)
	}
	lookup := values[0].(servicetool.RecoveryLookup)
	key, err := values[0].(servicetool.RecoveryDescriptor).RecoveryKey(ctx)
	if err != nil {
		t.Fatal(err)
	}
	result, err := values[0].(agenttool.CallableTool).Call(ctx, []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(result)
	var actual map[string]json.RawMessage
	_ = json.Unmarshal(b, &actual)
	if string(actual["record_id"]) != "9007199254740993" {
		t.Fatalf("rounded write receipt: %s", b)
	}
	for _, state := range []string{"completed", "not_found", "pending", "failed", "unknown"} {
		h.state = state
		r, err := lookup.LookupRecovery(ctx, key)
		if err != nil || string(r.State) != state {
			t.Fatalf("lookup=%+v %v", r, err)
		}
		if state == "completed" {
			b, _ := json.Marshal(r.Result)
			_ = json.Unmarshal(b, &actual)
			if string(actual["record_id"]) != "9007199254740993" {
				t.Fatalf("rounded lookup receipt: %s", b)
			}
		}
	}
	if h.writes != 1 || h.queries != 5 {
		t.Fatalf("query caused write: %d/%d", h.writes, h.queries)
	}
	for _, got := range h.keys {
		if got != key {
			t.Fatal("provider key drift")
		}
	}
	for _, header := range h.headers {
		if header.Get("Authorization") != "Bearer scoped-token" {
			t.Fatal("lost lookup credential scope")
		}
	}
	if provider.scope.ResourceID != "create_record" || provider.scope.TenantID != "tenant-a" {
		t.Fatal(provider.scope)
	}
	h.wrongKey = true
	if _, err = lookup.LookupRecovery(ctx, key); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatal("wrong operation accepted", err)
	}
	h.wrongKey = false
	h.failed = true
	if _, err = lookup.LookupRecovery(ctx, key); !errors.Is(err, runtime.ErrEffectUnknown) {
		t.Fatal("MCP error treated as absence", err)
	}
	h.failed = false
	h.changedDeclaration = true
	if _, err = lookup.LookupRecovery(ctx, key); !errors.Is(err, runtime.ErrVersionMismatch) {
		t.Fatal("changed lookup admitted", err)
	}
	other := runtime.WithExecutionContext(context.Background(), runtime.ExecutionContext{TenantID: "tenant-b", RequestID: "request", ToolCallID: "stable-op", ArgsDigest: "digest"})
	if _, err = lookup.LookupRecovery(other, key); !errors.Is(err, runtime.ErrTenantScope) {
		t.Fatal("cross-tenant lookup", err)
	}
}

func TestMCPRecoveryConfigAndUnambiguousJSON(t *testing.T) {
	c := Config{Transport: "streamable", ServerURL: "https://mcp.test/tools", RemoteToolName: "write", ExpectedDeclarationDigest: taskflow.Digest([]byte("write")), Timeout: time.Second}
	base, _ := BindingDigest(c, secrets.SecretRef{})
	c.Lookup = &LookupConfig{ToolName: "lookup", KeyParameter: "key", ExpectedDeclarationDigest: taskflow.Digest([]byte("lookup"))}
	if _, err := BindingDigest(c, secrets.SecretRef{}); err == nil {
		t.Fatal("lookup without idempotent write permitted")
	}
	c.IdempotencyParameter = "key"
	c.CompletionCheck = &taskflow.ResultCheck{NonEmpty: true}
	next, err := BindingDigest(c, secrets.SecretRef{})
	if err != nil || next == base {
		t.Fatal("lookup contract not pinned", err)
	}
	for _, bad := range []string{`null`, `[{"type":"text","text":"{}"},{"type":"text","text":"{}"}]`, `[{"type":"text","text":"prefix {}"}]`, `{"status":"created"} {}`, `[{"type":"image","text":"{}"}]`, `{"state":"failed","state":"completed"}`} {
		if _, err := decodeJSONObject([]byte(bad)); err == nil {
			t.Fatalf("ambiguous JSON accepted: %s", bad)
		}
	}
}
