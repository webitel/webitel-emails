package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

const emailProfileRuntimeColumns = `r.profile_id,
    r.domain_id,
    r.owner_instance_id,
    r.assignment_generation,
    r.next_check_at,
    r.provider_cursor,
    r.last_poll_started_at,
    r.last_poll_finished_at,
    r.last_poll_error,
    r.created_at,
    r.updated_at`

type emailProfileRuntimeStore struct {
	db *sql.DB
}

var _ store.EmailProfileRuntimeStore = (*emailProfileRuntimeStore)(nil)

// NewEmailProfileRuntimeStore creates a runtime store over the shared database connection.
func NewEmailProfileRuntimeStore(db *sql.DB) *emailProfileRuntimeStore {
	return &emailProfileRuntimeStore{db: db}
}

func (s *emailProfileRuntimeStore) Get(ctx context.Context, profileID int64) (*model.EmailProfileRuntime, error) {
	row := s.db.QueryRowContext(
		ctx,
		"SELECT "+emailProfileRuntimeColumns+" FROM email.profile_runtime r WHERE r.profile_id = $1",
		profileID,
	)

	return scanEmailProfileRuntime(row)
}

func (s *emailProfileRuntimeStore) Assign(ctx context.Context, profileID int64, instanceID string) (*model.EmailProfileAssignment, error) {
	if instanceID == "" {
		return nil, kiterrors.InvalidArgument(
			"owner instance id is required",
			kiterrors.WithID("store.email_profile_runtime.assign.owner_required"),
		)
	}

	// Unconditional upsert: RETURNING yields the row even under a concurrent insert.
	const query = `
INSERT INTO email.profile_runtime AS r (
    profile_id,
    domain_id,
    owner_instance_id,
    assignment_generation,
    next_check_at
)
SELECT p.id, p.domain_id, $2, 1, now()
FROM email.profile p
WHERE p.id = $1
ON CONFLICT (profile_id) DO UPDATE SET
    owner_instance_id = EXCLUDED.owner_instance_id,
    assignment_generation = CASE
        WHEN r.owner_instance_id IS DISTINCT FROM EXCLUDED.owner_instance_id
            THEN r.assignment_generation + 1
        ELSE r.assignment_generation
    END,
    next_check_at = CASE
        WHEN r.owner_instance_id IS DISTINCT FROM EXCLUDED.owner_instance_id
            THEN now()
        ELSE r.next_check_at
    END,
    updated_at = CASE
        WHEN r.owner_instance_id IS DISTINCT FROM EXCLUDED.owner_instance_id
            THEN now()
        ELSE r.updated_at
    END
RETURNING r.profile_id, r.domain_id, r.owner_instance_id, r.assignment_generation`

	assignment := new(model.EmailProfileAssignment)
	if err := s.db.QueryRowContext(ctx, query, profileID, instanceID).Scan(
		&assignment.ProfileID,
		&assignment.DomainID,
		&assignment.OwnerInstanceID,
		&assignment.Generation,
	); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, emailProfileRuntimeNotFound(err)
		}

		return nil, err
	}

	return assignment, nil
}

func (s *emailProfileRuntimeStore) Unassign(ctx context.Context, profileID int64) error {
	// The next Assign increments the generation, even for the same instance.
	const query = `
UPDATE email.profile_runtime SET
    owner_instance_id = NULL,
    updated_at = now()
WHERE profile_id = $1 AND owner_instance_id IS NOT NULL`

	_, err := s.db.ExecContext(ctx, query, profileID)

	return err
}

func (s *emailProfileRuntimeStore) ListOwnership(ctx context.Context) ([]*model.EmailProfileOwnership, error) {
	const query = `
SELECT p.id, p.enabled, COALESCE(r.owner_instance_id, '')
FROM email.profile p
LEFT JOIN email.profile_runtime r ON r.domain_id = p.domain_id AND r.profile_id = p.id
WHERE p.enabled OR r.owner_instance_id IS NOT NULL
ORDER BY p.id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	profiles := make([]*model.EmailProfileOwnership, 0)
	for rows.Next() {
		profile := new(model.EmailProfileOwnership)
		if err := rows.Scan(&profile.ProfileID, &profile.Enabled, &profile.OwnerInstanceID); err != nil {
			return nil, err
		}

		profiles = append(profiles, profile)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return profiles, nil
}

func (s *emailProfileRuntimeStore) ListDue(ctx context.Context, instanceID string, limit int) ([]*model.EmailProfileRuntime, error) {
	query := `
SELECT ` + emailProfileRuntimeColumns + `
FROM email.profile_runtime r
JOIN email.profile p ON p.domain_id = r.domain_id AND p.id = r.profile_id
WHERE r.owner_instance_id = $1
    AND p.enabled
    AND p.connection_state <> 'reauthorization_required'
    AND (r.next_check_at IS NULL OR r.next_check_at <= now())
ORDER BY r.next_check_at ASC NULLS FIRST, r.profile_id ASC`

	args := []any{instanceID}
	if limit > 0 {
		args = append(args, limit)
		query += " LIMIT $2"
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	runtimes := make([]*model.EmailProfileRuntime, 0)
	for rows.Next() {
		runtime, err := scanEmailProfileRuntime(rows)
		if err != nil {
			return nil, err
		}

		runtimes = append(runtimes, runtime)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return runtimes, nil
}

func (s *emailProfileRuntimeStore) StartPoll(ctx context.Context, assignment model.EmailProfileAssignment) error {
	const query = `
UPDATE email.profile_runtime SET
    last_poll_started_at = now(),
    updated_at = now()
WHERE profile_id = $1
    AND owner_instance_id = $2
    AND assignment_generation = $3`

	result, err := s.db.ExecContext(
		ctx,
		query,
		assignment.ProfileID,
		assignment.OwnerInstanceID,
		assignment.Generation,
	)
	if err != nil {
		return err
	}

	return requireFencedWrite(result)
}

func (s *emailProfileRuntimeStore) CompletePoll(
	ctx context.Context,
	assignment model.EmailProfileAssignment,
	result model.EmailProfilePollResult,
) error {
	cursor, err := encodeProviderCursor(result.ProviderCursor)
	if err != nil {
		return err
	}

	// A stale assignment updates no runtime row, so the profile is not touched.
	const query = `
WITH runtime AS (
    UPDATE email.profile_runtime r SET
        provider_cursor = COALESCE($4::jsonb, r.provider_cursor),
        next_check_at = COALESCE(
            $5::timestamptz,
            now() + make_interval(secs => p.fetch_interval_seconds)
        ),
        last_poll_finished_at = now(),
        last_poll_error = NULLIF($6, ''),
        updated_at = now()
    FROM email.profile p
    WHERE r.profile_id = $1
        AND r.owner_instance_id = $2
        AND r.assignment_generation = $3
        AND p.domain_id = r.domain_id
        AND p.id = r.profile_id
    RETURNING r.profile_id, r.domain_id
), profile AS (
    UPDATE email.profile p SET
        connection_state = $7,
        last_successful_connection_at = CASE
            WHEN $8 THEN now()
            ELSE p.last_successful_connection_at
        END,
        connection_error = NULLIF($9, '')
    FROM runtime
    WHERE $7 <> ''
        AND p.domain_id = runtime.domain_id
        AND p.id = runtime.profile_id
    RETURNING p.id
)
SELECT count(*) FROM runtime`

	var updated int
	if err := s.db.QueryRowContext(
		ctx,
		query,
		assignment.ProfileID,
		assignment.OwnerInstanceID,
		assignment.Generation,
		cursor,
		nullableTime(result.NextCheckAt),
		result.Error,
		string(result.ConnectionState),
		result.Connected,
		result.ConnectionError,
	).Scan(&updated); err != nil {
		return err
	}

	if updated == 0 {
		return store.ErrStaleEmailProfileAssignment
	}

	return nil
}

// requireFencedWrite reports a fenced write that matched no row as stale.
func requireFencedWrite(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}

	if affected == 0 {
		return store.ErrStaleEmailProfileAssignment
	}

	return nil
}

// encodeProviderCursor returns NULL for an empty cursor.
func encodeProviderCursor(cursor *model.ProviderCursor) (sql.NullString, error) {
	if cursor.IsEmpty() {
		return sql.NullString{}, nil
	}

	data, err := json.Marshal(cursor)
	if err != nil {
		return sql.NullString{}, fmt.Errorf("encode provider cursor: %w", err)
	}

	return sql.NullString{String: string(data), Valid: true}, nil
}

func decodeProviderCursor(data []byte) (*model.ProviderCursor, error) {
	if len(data) == 0 {
		return nil, nil
	}

	cursor := new(model.ProviderCursor)
	if err := json.Unmarshal(data, cursor); err != nil {
		return nil, fmt.Errorf("decode provider cursor: %w", err)
	}

	return cursor, nil
}

func scanEmailProfileRuntime(row rowScanner) (*model.EmailProfileRuntime, error) {
	var (
		runtime            model.EmailProfileRuntime
		ownerInstanceID    sql.NullString
		nextCheckAt        sql.NullTime
		providerCursor     []byte
		lastPollStartedAt  sql.NullTime
		lastPollFinishedAt sql.NullTime
		lastPollError      sql.NullString
	)

	if err := row.Scan(
		&runtime.ProfileID,
		&runtime.DomainID,
		&ownerInstanceID,
		&runtime.AssignmentGeneration,
		&nextCheckAt,
		&providerCursor,
		&lastPollStartedAt,
		&lastPollFinishedAt,
		&lastPollError,
		&runtime.CreatedAt,
		&runtime.UpdatedAt,
	); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, emailProfileRuntimeNotFound(err)
		}

		return nil, err
	}

	cursor, err := decodeProviderCursor(providerCursor)
	if err != nil {
		return nil, err
	}

	runtime.OwnerInstanceID = ownerInstanceID.String
	runtime.NextCheckAt = nullTimeValue(nextCheckAt)
	runtime.ProviderCursor = cursor
	runtime.LastPollStartedAt = nullTimeValue(lastPollStartedAt)
	runtime.LastPollFinishedAt = nullTimeValue(lastPollFinishedAt)
	runtime.LastPollError = lastPollError.String

	return &runtime, nil
}

func emailProfileRuntimeNotFound(cause error) error {
	return kiterrors.NotFound(
		"email profile runtime does not exist",
		kiterrors.WithID("store.email_profile_runtime.not_found"),
		kiterrors.WithCause(cause),
	)
}

func nullableTime(value *time.Time) any {
	if value == nil {
		return nil
	}

	return *value
}
