package tool

import (
	"context"
	"fmt"
	agenttool "trpc.group/trpc-go/trpc-agent-go/tool"

	"github.com/liuzengh/trpc-agent-service/trpcservice/agentapp"
	"github.com/liuzengh/trpc-agent-service/trpcservice/profile"
	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

// ValidateTaskRequirements resolves metadata/build declarations only. It must
// never invoke Call or execute the business operation while publishing.
func (r Resolver) ValidateTaskRequirements(ctx context.Context, revision agentapp.Revision) error {
	if err := revision.ValidateTaskRequirements(); err != nil {
		return err
	}
	requirements, err := taskflow.ParseRequirements(revision.RuntimePolicy)
	if err != nil {
		return err
	}
	checked := map[string]agenttool.Tool{}
	for _, required := range requirements {
		if required.ToolID == "" {
			continue
		}
		value := checked[required.ToolID]
		if value == nil {
			var ref profile.VersionedRef
			for _, pinned := range revision.ToolRefs {
				if pinned.ID == required.ToolID && pinned.Version == required.ToolVersion {
					ref = profile.VersionedRef{ID: pinned.ID, Version: pinned.Version, ContentDigest: pinned.ContentDigest}
				}
			}
			values, e := r.ResolveTools(ctx, revision.TenantID, []profile.VersionedRef{ref})
			if e != nil {
				return fmt.Errorf("%w: task requirement %s tool unavailable: %w", agentapp.ErrInvalid, required.ID, e)
			}
			if len(values) != 1 {
				return fmt.Errorf("%w: task requirement tool unavailable", agentapp.ErrInvalid)
			}
			value = values[0]
			descriptor, ok := value.(taskflow.EvidenceDescriptor)
			if !ok {
				return fmt.Errorf("%w: missing trusted completion check", agentapp.ErrInvalid)
			}
			check := descriptor.TaskEvidenceCheck()
			if check == nil || !check.Valid() {
				return fmt.Errorf("%w: invalid trusted completion check", agentapp.ErrInvalid)
			}
			if final, ok := value.(taskflow.FinalEvidenceDescriptor); ok {
				checks := final.TaskFinalEvidenceChecks()
				if len(checks) > 8 {
					return agentapp.ErrInvalid
				}
				for _, c := range checks {
					if c == nil || !c.Valid() || !c.NonEmpty || validateEvidencePath(value.Declaration().OutputSchema, c.Path) != nil {
						return fmt.Errorf("%w: invalid final evidence declaration", agentapp.ErrInvalid)
					}
				}
			}
			checked[required.ToolID] = value
		}
		if len(required.EvidencePath) > 0 {
			if err = validateEvidencePath(value.Declaration().OutputSchema, required.EvidencePath); err != nil {
				return err
			}
		}

	}
	return nil
}

// An explicit OutputSchema must support the configured scalar path. Schemaless
// tools retain runtime checking; publication cannot infer a provider schema.
func validateEvidencePath(schema *agenttool.Schema, path []string) error {
	if schema == nil {
		return nil
	}
	for _, part := range path {
		if schema.Type != "object" || schema.Properties[part] == nil {
			return fmt.Errorf("%w: unknown evidence output path", agentapp.ErrInvalid)
		}
		schema = schema.Properties[part]
	}
	if schema.Type != "string" && schema.Type != "integer" && schema.Type != "number" {
		return fmt.Errorf("%w: evidence output must be a scalar reference", agentapp.ErrInvalid)
	}
	return nil
}
