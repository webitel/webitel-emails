// Package postgres implements service storage contracts using PostgreSQL.
package postgres

import (
	"context"
	"database/sql"
	stderrors "errors"
	"fmt"
	"strings"
	"time"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/store"
)

// emailProfileColumns is the safe read projection shared by every operation.
// Credentials are excluded; only the presence of a refresh token is exposed.
const emailProfileColumns = `p.id,
    p.domain_id,
    p.name,
    p.description,
    p.enabled,
    p.email_address,
    p.sender_name,
    p.reply_to,
    p.signature,
    p.imap_host,
    p.imap_port,
    p.imap_security,
    p.smtp_host,
    p.smtp_port,
    p.smtp_security,
    p.username,
    p.mailbox,
    p.fetch_interval_seconds,
    p.flow_id,
    p.auth_type,
    p.oauth_provider,
    p.oauth_client_id,
    p.oauth_refresh_token IS NOT NULL AS oauth_connected,
    p.connection_state,
    p.last_successful_connection_at,
    p.connection_error,
    p.created_at,
    p.created_by,
    COALESCE(created_by.name, created_by.username),
    p.updated_at,
    p.updated_by,
    COALESCE(updated_by.name, updated_by.username)`

const emailProfileJoins = `
LEFT JOIN directory.wbt_user created_by ON created_by.id = p.created_by
LEFT JOIN directory.wbt_user updated_by ON updated_by.id = p.updated_by`

const emailProfileSelect = `SELECT ` + emailProfileColumns + `
FROM email.profile p` + emailProfileJoins

const (
	defaultEmailProfilePage = 1
	defaultEmailProfileSize = 10
	maxEmailProfileSize     = 5000
)

var emailProfileSortFields = map[string]string{
	"id":               "p.id",
	"name":             "p.name",
	"email_address":    "p.email_address",
	"enabled":          "p.enabled",
	"connection_state": "p.connection_state",
	"created_at":       "p.created_at",
	"updated_at":       "p.updated_at",
}

type emailProfileStore struct {
	db *sql.DB
}

var _ store.EmailProfileStore = (*emailProfileStore)(nil)

// NewEmailProfileStore creates an Email Profile store over the shared database connection.
func NewEmailProfileStore(db *sql.DB) *emailProfileStore {
	return &emailProfileStore{db: db}
}

// emailProfileRecord is the flat SQL scan target. Nullable database values are
// converted to domain-friendly zero values or pointers by mapEmailProfile.
type emailProfileRecord struct {
	ID       int64
	DomainID int64

	Name        string
	Description string
	Enabled     bool

	EmailAddress string
	SenderName   string
	ReplyTo      string
	Signature    string

	IMAPHost             string
	IMAPPort             int32
	IMAPSecurity         string
	SMTPHost             string
	SMTPPort             int32
	SMTPSecurity         string
	Username             string
	Mailbox              string
	FetchIntervalSeconds int32

	FlowID         sql.NullInt64
	AuthType       string
	OAuthProvider  sql.NullString
	OAuthClientID  sql.NullString
	OAuthConnected bool

	ConnectionState            string
	LastSuccessfulConnectionAt sql.NullTime
	ConnectionError            sql.NullString

	CreatedAt     time.Time
	CreatedByID   sql.NullInt64
	CreatedByName sql.NullString
	UpdatedAt     time.Time
	UpdatedByID   sql.NullInt64
	UpdatedByName sql.NullString
}

type rowScanner interface {
	Scan(dest ...any) error
}

func (s *emailProfileStore) Locate(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	row := s.db.QueryRowContext(
		ctx,
		emailProfileSelect+" WHERE p.domain_id = $1 AND p.id = $2",
		domainID,
		id,
	)

	return scanEmailProfile(row)
}

func (s *emailProfileStore) List(ctx context.Context, domainID int64, filter model.EmailProfileFilter) ([]*model.EmailProfile, bool, error) {
	query := emailProfileSelect + " WHERE p.domain_id = $1"
	args := []any{domainID}

	if search := strings.TrimSpace(filter.Query); search != "" {
		args = append(args, "%"+search+"%")
		placeholder := fmt.Sprintf("$%d", len(args))
		query += " AND (p.name ILIKE " + placeholder +
			" OR p.description ILIKE " + placeholder +
			" OR p.email_address ILIKE " + placeholder + ")"
	}

	query += " ORDER BY " + emailProfileOrderBy(filter.Sort)

	page, size := emailProfilePaging(filter.Page, filter.Size)
	if size > 0 {
		args = append(args, size+1)
		query += fmt.Sprintf(" LIMIT $%d", len(args))

		args = append(args, int64(page-1)*int64(size))
		query += fmt.Sprintf(" OFFSET $%d", len(args))
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	profiles := make([]*model.EmailProfile, 0)
	for rows.Next() {
		profile, err := scanEmailProfile(rows)
		if err != nil {
			return nil, false, err
		}

		profiles = append(profiles, profile)
	}

	if err := rows.Err(); err != nil {
		return nil, false, err
	}

	if size > 0 && len(profiles) > size {
		return profiles[:size], true, nil
	}

	return profiles, false, nil
}

func (s *emailProfileStore) Create(ctx context.Context, domainID, userID int64, profile *model.EmailProfile, password, oauthClientSecret []byte) (*model.EmailProfile, error) {
	mailbox, fetchInterval, authType := emailProfileDefaults(profile)

	const query = `
INSERT INTO email.profile (
    domain_id,
    name,
    description,
    enabled,
    email_address,
    sender_name,
    reply_to,
    signature,
    imap_host,
    imap_port,
    imap_security,
    smtp_host,
    smtp_port,
    smtp_security,
    username,
    password,
    mailbox,
    fetch_interval_seconds,
    flow_id,
    auth_type,
    oauth_provider,
    oauth_client_id,
    oauth_client_secret,
    created_by,
    updated_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25
)
RETURNING *`

	return s.writeReturning(
		ctx,
		query,
		domainID,
		profile.Name,
		profile.Description,
		profile.Enabled,
		profile.EmailAddress,
		profile.SenderName,
		profile.ReplyTo,
		profile.Signature,
		profile.IMAPHost,
		profile.IMAPPort,
		profile.IMAPSecurity,
		profile.SMTPHost,
		profile.SMTPPort,
		profile.SMTPSecurity,
		profile.Username,
		password,
		mailbox,
		fetchInterval,
		nullableInt64(profile.FlowID),
		authType,
		nullIfEmpty(string(profile.OAuthProvider)),
		nullIfEmpty(profile.OAuthClientID),
		oauthClientSecret,
		nullIfZero(userID),
		nullIfZero(userID),
	)
}

func (s *emailProfileStore) Update(
	ctx context.Context,
	domainID, userID, id int64,
	profile *model.EmailProfile,
	password, oauthClientSecret []byte,
) (*model.EmailProfile, error) {
	mailbox, fetchInterval, authType := emailProfileDefaults(profile)

	const query = `
UPDATE email.profile SET
    name = $1,
    description = $2,
    enabled = $3,
    email_address = $4,
    sender_name = $5,
    reply_to = $6,
    signature = $7,
    imap_host = $8,
    imap_port = $9,
    imap_security = $10,
    smtp_host = $11,
    smtp_port = $12,
    smtp_security = $13,
    username = $14,
    password = CASE WHEN $19 = 'basic' THEN COALESCE($15, password) ELSE NULL END,
    mailbox = $16,
    fetch_interval_seconds = $17,
    flow_id = $18,
    auth_type = $19,
    oauth_provider = $20,
    oauth_client_id = $21,
    oauth_client_secret = CASE
        WHEN $19 <> 'oauth2' THEN NULL
        WHEN oauth_provider IS NOT DISTINCT FROM $20
            AND oauth_client_id IS NOT DISTINCT FROM $21
            THEN COALESCE($22, oauth_client_secret)
        ELSE $22
    END,
    oauth_refresh_token = CASE
        WHEN $19 = 'oauth2'
            AND oauth_provider IS NOT DISTINCT FROM $20
            AND oauth_client_id IS NOT DISTINCT FROM $21
            THEN oauth_refresh_token
        ELSE NULL
    END,
    updated_at = now(),
    updated_by = $23
WHERE domain_id = $24 AND id = $25
RETURNING *`

	return s.writeReturning(
		ctx,
		query,
		profile.Name,
		profile.Description,
		profile.Enabled,
		profile.EmailAddress,
		profile.SenderName,
		profile.ReplyTo,
		profile.Signature,
		profile.IMAPHost,
		profile.IMAPPort,
		profile.IMAPSecurity,
		profile.SMTPHost,
		profile.SMTPPort,
		profile.SMTPSecurity,
		profile.Username,
		password,
		mailbox,
		fetchInterval,
		nullableInt64(profile.FlowID),
		authType,
		nullIfEmpty(string(profile.OAuthProvider)),
		nullIfEmpty(profile.OAuthClientID),
		oauthClientSecret,
		nullIfZero(userID),
		domainID,
		id,
	)
}

func (s *emailProfileStore) GetPassword(ctx context.Context, domainID, id int64) ([]byte, error) {
	const query = `SELECT password FROM email.profile WHERE domain_id = $1 AND id = $2`

	var password []byte
	if err := s.db.QueryRowContext(ctx, query, domainID, id).Scan(&password); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, kiterrors.NotFound(
				"email profile does not exist or access is denied",
				kiterrors.WithID("store.email_profile.not_found"),
				kiterrors.WithCause(err),
			)
		}

		return nil, err
	}

	return password, nil
}

func (s *emailProfileStore) GetOAuthCredentials(ctx context.Context, domainID, id int64) (*store.EmailProfileOAuthCredentials, error) {
	const query = `
SELECT oauth_client_secret, oauth_refresh_token
FROM email.profile
WHERE domain_id = $1 AND id = $2`

	credentials := new(store.EmailProfileOAuthCredentials)
	if err := s.db.QueryRowContext(ctx, query, domainID, id).Scan(
		&credentials.ClientSecret,
		&credentials.RefreshToken,
	); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, kiterrors.NotFound(
				"email profile does not exist or access is denied",
				kiterrors.WithID("store.email_profile.not_found"),
				kiterrors.WithCause(err),
			)
		}

		return nil, err
	}

	return credentials, nil
}

func (s *emailProfileStore) SetOAuthRefreshToken(
	ctx context.Context,
	domainID, id int64,
	refreshToken []byte,
) error {
	// updated_at also invalidates any IMAP session cached against the old token version.
	const query = `
UPDATE email.profile
SET oauth_refresh_token = $1,
    updated_at = now()
WHERE domain_id = $2 AND id = $3
RETURNING id`

	var updatedID int64
	if err := s.db.QueryRowContext(ctx, query, refreshToken, domainID, id).Scan(&updatedID); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return kiterrors.NotFound(
				"email profile does not exist or access is denied",
				kiterrors.WithID("store.email_profile.not_found"),
				kiterrors.WithCause(err),
			)
		}

		return err
	}

	return nil
}

func (s *emailProfileStore) SetOAuthRefreshTokenAndState(ctx context.Context, domainID, id int64, refreshToken []byte, state model.EmailConnectionState) error {
	// updated_at also invalidates any IMAP session cached against the old token version,
	// so a revoked (Disconnect) or replaced authorization stops an already-open session.
	const query = `
UPDATE email.profile SET
    oauth_refresh_token = $1,
    connection_state = $2,
    connection_error = NULL,
    updated_at = now()
WHERE domain_id = $3 AND id = $4
RETURNING id`

	var updatedID int64
	if err := s.db.QueryRowContext(
		ctx, query, refreshToken, state, domainID, id,
	).Scan(&updatedID); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return kiterrors.NotFound(
				"email profile does not exist or access is denied",
				kiterrors.WithID("store.email_profile.not_found"),
				kiterrors.WithCause(err),
			)
		}

		return err
	}

	return nil
}

func (s *emailProfileStore) SetConnectionResult(ctx context.Context, domainID, id int64, state model.EmailConnectionState, connectionError string, successful bool) error {
	const query = `
UPDATE email.profile SET
    connection_state = $1,
    last_successful_connection_at = CASE
        WHEN $2 THEN now()
        ELSE last_successful_connection_at
    END,
    connection_error = NULLIF($3, '')
WHERE domain_id = $4 AND id = $5
RETURNING id`

	var updatedID int64
	if err := s.db.QueryRowContext(
		ctx,
		query,
		state,
		successful,
		connectionError,
		domainID,
		id,
	).Scan(&updatedID); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return kiterrors.NotFound(
				"email profile does not exist or access is denied",
				kiterrors.WithID("store.email_profile.not_found"),
				kiterrors.WithCause(err),
			)
		}

		return err
	}

	return nil
}

func (s *emailProfileStore) Delete(ctx context.Context, domainID, id int64) (*model.EmailProfile, error) {
	const query = `
DELETE FROM email.profile
WHERE domain_id = $1 AND id = $2
RETURNING *`

	return s.writeReturning(ctx, query, domainID, id)
}

// writeReturning wraps a write in a CTE and reads the changed row through the
// same safe projection and scanner used by Locate and List.
func (s *emailProfileStore) writeReturning(ctx context.Context, query string, args ...any) (*model.EmailProfile, error) {
	readBackQuery := "WITH p AS (" + query + ") SELECT " + emailProfileColumns +
		" FROM p" + emailProfileJoins

	return scanEmailProfile(s.db.QueryRowContext(ctx, readBackQuery, args...))
}

func emailProfileDefaults(profile *model.EmailProfile) (string, int32, model.EmailAuthType) {
	mailbox := profile.Mailbox
	if mailbox == "" {
		mailbox = "INBOX"
	}

	fetchInterval := profile.FetchIntervalSeconds
	if fetchInterval == 0 {
		fetchInterval = 5
	}

	authType := profile.AuthType
	if authType == "" {
		authType = model.EmailAuthTypeBasic
	}

	return mailbox, fetchInterval, authType
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}

	return *value
}

func nullIfEmpty(value string) any {
	if value == "" {
		return nil
	}

	return value
}

func nullIfZero(value int64) any {
	if value == 0 {
		return nil
	}

	return value
}

// emailProfilePaging applies API defaults and caps one database request.
// A negative size deliberately disables pagination.
func emailProfilePaging(page, size int32) (int, int) {
	resolvedPage := int(page)
	if resolvedPage <= 0 {
		resolvedPage = defaultEmailProfilePage
	}

	resolvedSize := int(size)
	switch {
	case resolvedSize < 0:
		return defaultEmailProfilePage, -1
	case resolvedSize == 0:
		resolvedSize = defaultEmailProfileSize
	case resolvedSize > maxEmailProfileSize:
		resolvedSize = maxEmailProfileSize
	}

	return resolvedPage, resolvedSize
}

// emailProfileOrderBy accepts only known fields, preventing user-provided SQL
// identifiers from reaching ORDER BY. ID is appended for stable pagination.
func emailProfileOrderBy(sort string) string {
	criteria := strings.Split(sort, ",")
	orderBy := make([]string, 0, len(criteria)+1)
	hasID := false

	for _, criterion := range criteria {
		criterion = strings.TrimSpace(criterion)
		if criterion == "" {
			continue
		}

		direction := "ASC"
		switch criterion[0] {
		case '+':
			criterion = criterion[1:]
		case '-':
			criterion = criterion[1:]
			direction = "DESC"
		}

		field := strings.TrimSpace(criterion)
		column, ok := emailProfileSortFields[field]
		if !ok {
			continue
		}

		orderBy = append(orderBy, column+" "+direction)
		hasID = hasID || field == "id"
	}

	if len(orderBy) == 0 {
		return "p.name ASC, p.id ASC"
	}

	if !hasID {
		orderBy = append(orderBy, "p.id ASC")
	}

	return strings.Join(orderBy, ", ")
}

func scanEmailProfile(row rowScanner) (*model.EmailProfile, error) {
	record := new(emailProfileRecord)
	if err := row.Scan(
		&record.ID,
		&record.DomainID,
		&record.Name,
		&record.Description,
		&record.Enabled,
		&record.EmailAddress,
		&record.SenderName,
		&record.ReplyTo,
		&record.Signature,
		&record.IMAPHost,
		&record.IMAPPort,
		&record.IMAPSecurity,
		&record.SMTPHost,
		&record.SMTPPort,
		&record.SMTPSecurity,
		&record.Username,
		&record.Mailbox,
		&record.FetchIntervalSeconds,
		&record.FlowID,
		&record.AuthType,
		&record.OAuthProvider,
		&record.OAuthClientID,
		&record.OAuthConnected,
		&record.ConnectionState,
		&record.LastSuccessfulConnectionAt,
		&record.ConnectionError,
		&record.CreatedAt,
		&record.CreatedByID,
		&record.CreatedByName,
		&record.UpdatedAt,
		&record.UpdatedByID,
		&record.UpdatedByName,
	); err != nil {
		if stderrors.Is(err, sql.ErrNoRows) {
			return nil, kiterrors.NotFound(
				"email profile does not exist or access is denied",
				kiterrors.WithID("store.email_profile.not_found"),
				kiterrors.WithCause(err),
			)
		}

		return nil, err
	}

	return mapEmailProfile(record), nil
}

func mapEmailProfile(record *emailProfileRecord) *model.EmailProfile {
	profile := &model.EmailProfile{
		ID:                         record.ID,
		DomainID:                   record.DomainID,
		Name:                       record.Name,
		Description:                record.Description,
		Enabled:                    record.Enabled,
		EmailAddress:               record.EmailAddress,
		SenderName:                 record.SenderName,
		ReplyTo:                    record.ReplyTo,
		Signature:                  record.Signature,
		IMAPHost:                   record.IMAPHost,
		IMAPPort:                   record.IMAPPort,
		IMAPSecurity:               model.EmailConnectionSecurity(record.IMAPSecurity),
		SMTPHost:                   record.SMTPHost,
		SMTPPort:                   record.SMTPPort,
		SMTPSecurity:               model.EmailConnectionSecurity(record.SMTPSecurity),
		Username:                   record.Username,
		Mailbox:                    record.Mailbox,
		FetchIntervalSeconds:       record.FetchIntervalSeconds,
		AuthType:                   model.EmailAuthType(record.AuthType),
		OAuthProvider:              model.EmailOAuthProvider(record.OAuthProvider.String),
		OAuthClientID:              record.OAuthClientID.String,
		OAuthConnected:             record.OAuthConnected,
		ConnectionState:            model.EmailConnectionState(record.ConnectionState),
		LastSuccessfulConnectionAt: nullTimeValue(record.LastSuccessfulConnectionAt),
		ConnectionError:            record.ConnectionError.String,
		CreatedAt:                  record.CreatedAt,
		CreatedBy:                  lookupValue(record.CreatedByID, record.CreatedByName),
		UpdatedAt:                  record.UpdatedAt,
		UpdatedBy:                  lookupValue(record.UpdatedByID, record.UpdatedByName),
	}

	if record.FlowID.Valid {
		profile.FlowID = &record.FlowID.Int64
	}

	return profile
}

func lookupValue(id sql.NullInt64, name sql.NullString) *model.Lookup {
	if !id.Valid {
		return nil
	}

	return &model.Lookup{ID: id.Int64, Name: name.String}
}

func nullTimeValue(value sql.NullTime) *time.Time {
	if !value.Valid {
		return nil
	}

	return &value.Time
}
