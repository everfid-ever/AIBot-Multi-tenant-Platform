package tool

import (
	"context"
	"fmt"

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
	checked := map[string]bool{}
	for _, required := range requirements {
		if required.ToolID == "" || checked[required.ToolID] {
			continue
		}
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
		descriptor, ok := values[0].(taskflow.EvidenceDescriptor)
		if !ok || descriptor.TaskEvidenceCheck() == nil || !descriptor.TaskEvidenceCheck().Valid() {
			return fmt.Errorf("%w: task requirement %s has no trusted completion check", agentapp.ErrInvalid, required.ID)
		}
		checked[required.ToolID] = true
	}
	return nil
}
