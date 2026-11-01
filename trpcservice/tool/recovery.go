package tool

import (
	"context"
	"errors"

	"github.com/liuzengh/trpc-agent-service/trpcservice/runtime"
)

var (
	ErrRecoveryPending = errors.New("external operation is still pending")
	ErrRecoveryFailed  = errors.New("external operation failed; do not replay")
)

type RecoveryState string

const (
	RecoveryCompleted RecoveryState = "completed"
	RecoveryNotFound  RecoveryState = "not_found"
	RecoveryPending   RecoveryState = "pending"
	RecoveryFailed    RecoveryState = "failed"
	RecoveryUnknown   RecoveryState = "unknown"
)

type RecoveryResult struct {
	State  RecoveryState
	Result any
}

// RecoveryLookup adds an optional read-before-replay to idempotent tools.
// It does not grant replay permission: not_found still requires the SAME
// provider idempotency key, because an old request may arrive after the lookup.
type RecoveryLookup interface {
	RecoveryLookupEnabled() bool
	LookupRecovery(context.Context, string) (RecoveryResult, error)
}

// RecoveryPolicy declares what a new owner may safely do after a tool lease
// expires while the previous owner was executing an external operation.
type RecoveryPolicy string

const (
	RecoveryReplaySafe    RecoveryPolicy = "replay_safe"
	RecoveryIdempotentKey RecoveryPolicy = "idempotent_key"
	RecoveryQueryable     RecoveryPolicy = "queryable"
	RecoveryManual        RecoveryPolicy = "manual"
)

// RecoveryDescriptor is implemented by tools with an explicit takeover
// contract. The key is stable for one tenant/request/tool-call/arguments
// tuple and is safe to pass to an external provider as its idempotency key.
type RecoveryDescriptor interface {
	RecoveryPolicy() RecoveryPolicy
	RecoveryKey(context.Context) (string, error)
}

// RecoveryQuerier lets a queryable tool resolve a previously accepted
// external operation by its stable idempotency key without replaying it.
// completed=false must establish that the operation was NOT accepted and may
// safely be started. Pending/unknown operations must return an error, not false.
type RecoveryQuerier interface {
	RecoveryDescriptor
	QueryRecovery(context.Context, string) (result any, completed bool, err error)
}

// RecoveryContext exposes only the durable execution identity to a tool.
func RecoveryContext(ctx context.Context) (runtime.ExecutionContext, bool) {
	return runtime.ExecutionContextFrom(ctx)
}
