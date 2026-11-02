package tool

import (
	"context"
	"errors"
	"testing"

	"github.com/liuzengh/trpc-agent-service/trpcservice/agentapp"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool"
)

type checkedCatalogTool struct{ catalogCallable }

func (checkedCatalogTool) TaskEvidenceCheck() *taskflow.ResultCheck {
	return &taskflow.ResultCheck{Path: []string{"status"}, EqualsJSON: `"created"`}
}

type schemaCatalogTool struct{ checkedCatalogTool }

func (schemaCatalogTool) Declaration() *agenttool.Declaration {
	return &agenttool.Declaration{Name: "create", OutputSchema: &agenttool.Schema{Type: "object", Properties: map[string]*agenttool.Schema{"record_id": {Type: "string"}, "ok": {Type: "boolean"}}}}
}

func TestPublicationChecksEachEvidencePathEvenForSameCachedTool(t *testing.T) {
	catalog, err := NewCatalog(Registration{TenantID: "t", ID: "create", Version: 2, Status: StatusActive, Build: func(context.Context, BuildRequest) (agenttool.CallableTool, error) { return schemaCatalogTool{}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := agentapp.Revision{TenantID: "t", ToolRefs: []agentapp.VersionedRef{{ID: "create", Version: 2, Required: true}}, RuntimePolicy: map[string]any{"task_requirements": []taskflow.Requirement{{ID: "valid", Description: "record", ToolID: "create", ToolVersion: 2, EvidencePath: []string{"record_id"}}}}}
	resolver := Resolver{Catalog: catalog}
	if err = resolver.ValidateTaskRequirements(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"missing", "ok"} {
		r.RuntimePolicy["task_requirements"] = []taskflow.Requirement{{ID: "valid", Description: "record", ToolID: "create", ToolVersion: 2, EvidencePath: []string{"record_id"}}, {ID: "invalid", Description: "invalid", ToolID: "create", ToolVersion: 2, EvidencePath: []string{path}}}
		if err = resolver.ValidateTaskRequirements(context.Background(), r); !errors.Is(err, agentapp.ErrInvalid) {
			t.Fatalf("invalid or non-scalar output path accepted: %v", err)
		}
	}
}

func TestPublishToolValidationChecksTenantAndTrustedContractWithoutCallingBusinessTool(t *testing.T) {
	for _, trusted := range []bool{false, true} {
		calls := 0
		catalog, err := NewCatalog(Registration{TenantID: "t", ID: "create", Version: 2, Status: StatusActive, Build: func(context.Context, BuildRequest) (agenttool.CallableTool, error) {
			v := catalogCallable{name: "create", call: func(context.Context, []byte) (any, error) { calls++; return nil, nil }}
			if trusted {
				return checkedCatalogTool{v}, nil
			}
			return v, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		r := agentapp.Revision{TenantID: "t", ToolRefs: []agentapp.VersionedRef{{ID: "create", Version: 2, Required: true}}, RuntimePolicy: map[string]any{"task_requirements": []taskflow.Requirement{{ID: "record", Description: "record", ToolID: "create", ToolVersion: 2}}}}
		resolver := Resolver{Catalog: catalog}
		err = resolver.ValidateTaskRequirements(context.Background(), r)
		if trusted && err != nil {
			t.Fatal(err)
		}
		if !trusted && !errors.Is(err, agentapp.ErrInvalid) {
			t.Fatalf("missing trusted check accepted: %v", err)
		}
		r.TenantID = "other"
		if err = resolver.ValidateTaskRequirements(context.Background(), r); err == nil {
			t.Fatal("cross tenant registration accepted")
		}
		if calls != 0 {
			t.Fatal("publication executed business operation")
		}
	}
}
