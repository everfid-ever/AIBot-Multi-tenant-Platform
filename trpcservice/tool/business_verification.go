package tool

import "context"

// BusinessResultVerifier performs a read-only business postcondition check.
// A successful API response alone must not substitute for persisted effects.
// Verification failure preserves uncertainty; it never grants write permission.
type BusinessResultVerifier interface {
	VerifyBusinessResult(context.Context, []byte, []byte) error
}

func verifyBusinessResult(ctx context.Context, inner any, args, result []byte) error {
	if verifier, ok := inner.(BusinessResultVerifier); ok {
		return verifier.VerifyBusinessResult(ctx, args, result)
	}
	return nil
}
