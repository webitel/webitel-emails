package store

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	"github.com/webitel/webitel-emails/internal/model"
)

// ErrStaleEmailProfileAssignment means the caller no longer owns the profile and must stop serving it.
var ErrStaleEmailProfileAssignment = kiterrors.New(
	"email profile assignment is stale",
	kiterrors.WithID("store.email_profile_runtime.stale_assignment"),
	kiterrors.WithCode(codes.FailedPrecondition),
)

// EmailProfileRuntimeStore persists Email Profile ownership, schedule and sync position.
type EmailProfileRuntimeStore interface {
	// Get returns the runtime state of a profile.
	Get(ctx context.Context, profileID int64) (*model.EmailProfileRuntime, error)
	// Assign sets the owner; the generation changes only with the owner.
	Assign(ctx context.Context, profileID int64, instanceID string) (*model.EmailProfileAssignment, error)
	// Unassign removes the owner of a profile.
	Unassign(ctx context.Context, profileID int64) error
	// ListOwnership returns enabled or still assigned profiles with their owners.
	ListOwnership(ctx context.Context) ([]*model.EmailProfileOwnership, error)
	// ListDue returns enabled profiles of instanceID that are due for a check.
	// A non-positive limit returns all of them.
	ListDue(ctx context.Context, instanceID string, limit int) ([]*model.EmailProfileRuntime, error)
	// StartPoll records the start of a poll.
	StartPoll(ctx context.Context, assignment model.EmailProfileAssignment) error
	// CompletePoll atomically stores the poll result and connection state.
	CompletePoll(ctx context.Context, assignment model.EmailProfileAssignment, result model.EmailProfilePollResult) error
}
