package storage

import (
	"context"
	stderrors "errors"
	"fmt"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ErrUnavailable means storage itself could not be reached, so the same upload
// may succeed later.
var ErrUnavailable = kiterrors.New(
	"storage service is unavailable",
	kiterrors.WithID("storage.client.unavailable"),
	kiterrors.WithCode(codes.Unavailable),
)

// ErrFileRejected means storage refused this file, by file policy, malware scan
// or invalid metadata. The same bytes are refused on every attempt.
var ErrFileRejected = kiterrors.New(
	"storage rejected the file",
	kiterrors.WithID("storage.client.file_rejected"),
	kiterrors.WithCode(codes.FailedPrecondition),
)

// classifyError sorts an upload failure into a refusal of this file, a temporary
// unavailability of storage, or an error left as it is and retried by the caller.
// Cancellation of the caller's own context is never reclassified.
func classifyError(callerErr error, err error) error {
	if err == nil {
		return nil
	}
	if callerErr != nil {
		return err
	}
	if stderrors.Is(err, context.Canceled) {
		return err
	}

	switch status.Code(err) {
	// Only these two are properties of the file itself: a file policy violation
	// arrives as FailedPrecondition, malware as PermissionDenied. InvalidArgument
	// and NotFound come from storage configuration (a missing or disabled backend
	// profile), so they are retried and quarantine the email only once the
	// attempts run out.
	case codes.FailedPrecondition, codes.PermissionDenied:
		return fmt.Errorf("%w: %w", ErrFileRejected, err)
	case codes.Unavailable, codes.DeadlineExceeded:
		return fmt.Errorf("%w: %w", ErrUnavailable, err)
	default:
		return err
	}
}
