package biz

import (
	"fmt"

	"github.com/go-kratos/kratos/v2/errors"
)

// Error reasons. Kratos renders them over HTTP as
// {"code":<status>,"reason":<reason>,"message":...} and over gRPC as the
// matching status code with the reason in ErrorInfo.
const (
	ReasonForbidden          = "FORBIDDEN"
	ReasonNotFound           = "NOT_FOUND"
	ReasonConflict           = "CONFLICT"
	ReasonInvalid            = "INVALID_ARGUMENT"
	ReasonLimit              = "LIMIT_EXCEEDED"
	ReasonIdempotencyReused  = "IDEMPOTENCY_KEY_REUSED"
	ReasonIdempotencyMissing = "IDEMPOTENCY_KEY_REQUIRED"
	ReasonDuplicate          = "DU04"
	ReasonUnauthenticated    = "UNAUTHENTICATED"
)

// ErrForbidden is a caller whose role or organization may not do this.
func ErrForbidden(format string, a ...any) error {
	return errors.Forbidden(ReasonForbidden, fmt.Sprintf(format, a...))
}

// ErrNotFound is a resource the caller cannot see. Another organization's
// payment is not found, not forbidden, so ids do not leak.
func ErrNotFound(format string, a ...any) error {
	return errors.NotFound(ReasonNotFound, fmt.Sprintf(format, a...))
}

// ErrConflict is a resource not in a state that allows this.
func ErrConflict(format string, a ...any) error {
	return errors.Conflict(ReasonConflict, fmt.Sprintf(format, a...))
}

// ErrInvalid is a malformed or unacceptable request (400).
func ErrInvalid(format string, a ...any) error {
	return errors.BadRequest(ReasonInvalid, fmt.Sprintf(format, a...))
}

// ErrLimit is a request over an entitlement limit (422).
func ErrLimit(format string, a ...any) error {
	return errors.New(422, ReasonLimit, fmt.Sprintf(format, a...))
}

// ErrIdempotencyReused is a key used before for a different request (422, as
// the IETF Idempotency-Key draft specifies).
func ErrIdempotencyReused(key string) error {
	return errors.New(422, ReasonIdempotencyReused, fmt.Sprintf("idempotency key %q was used for a different request", key))
}

// ErrIdempotencyMissing is a create without an Idempotency-Key.
func ErrIdempotencyMissing() error {
	return errors.BadRequest(ReasonIdempotencyMissing, "send an Idempotency-Key header (at most 64 characters) so a retry cannot pay twice")
}

// ErrDuplicate is an end-to-end id the organization already used (409 DU04).
func ErrDuplicate(e2e, existing string) error {
	return errors.Conflict(ReasonDuplicate, fmt.Sprintf("DU04 duplicate: endToEndId %q is already used by payment %s", e2e, existing))
}

// ErrUnauthenticated is a missing or unverifiable bearer token.
func ErrUnauthenticated(msg string) error {
	return errors.Unauthorized(ReasonUnauthenticated, msg)
}
