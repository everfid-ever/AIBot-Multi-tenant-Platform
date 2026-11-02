package agentapp

import (
	"context"
	"fmt"

	"github.com/liuzengh/trpc-agent-service/trpcservice/taskflow"
)

type RequirementValidator func(context.Context, Revision) error

func (r Revision) ValidateTaskRequirements() error {
	requirements, err := taskflow.ParseRequirements(r.RuntimePolicy)
	if err != nil {
		return fmt.Errorf("%w: malformed task requirements", ErrInvalid)
	}
	for _, required := range requirements {
		if required.ToolID == "" {
			continue
		}
		found := false
		for _, ref := range r.ToolRefs {
			if ref.ID == required.ToolID && ref.Version == required.ToolVersion && ref.Required {
				found = true
			}
		}
		if !found {
			return fmt.Errorf("%w: task requirement %s must reference an exact mandatory tool", ErrInvalid, required.ID)
		}
	}
	return nil
}

// ValidatePublication fails closed when tool requirements cannot be checked
// against the deployment's actual tenant-scoped tool catalog.
func ValidatePublication(ctx context.Context, r Revision, validate RequirementValidator) error {
	if err := r.ValidateDraft(); err != nil {
		return err
	}
	requirements, err := taskflow.ParseRequirements(r.RuntimePolicy)
	if err != nil {
		return ErrInvalid
	}
	for _, required := range requirements {
		if required.ToolID != "" {
			if validate == nil {
				return fmt.Errorf("%w: tool requirement validator unavailable", ErrInvalid)
			}
			return validate(ctx, r)
		}
	}
	return nil
}
