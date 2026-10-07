-- +goose Up
CREATE SCHEMA email;

CREATE TABLE email.profile (
    id                            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    domain_id                     bigint NOT NULL,

    name                          text NOT NULL,
    description                   text NOT NULL DEFAULT '',
    enabled                       boolean NOT NULL DEFAULT false,

    email_address                 text NOT NULL,
    sender_name                   text NOT NULL DEFAULT '',
    reply_to                      text NOT NULL DEFAULT '',
    signature                     text NOT NULL DEFAULT '',

    imap_host                     text NOT NULL,
    imap_port                     integer NOT NULL,
    imap_security                 text NOT NULL,
    smtp_host                     text NOT NULL,
    smtp_port                     integer NOT NULL,
    smtp_security                 text NOT NULL,
    username                      text NOT NULL,
    password                      bytea NULL,
    mailbox                       text NOT NULL DEFAULT 'INBOX',
    fetch_interval_seconds        integer NOT NULL DEFAULT 5,

    flow_id                       bigint NULL,

    auth_type                     text NOT NULL DEFAULT 'basic',
    oauth_provider                text NULL,
    oauth_client_id               text NULL,
    oauth_client_secret           bytea NULL,
    oauth_refresh_token           bytea NULL,

    connection_state              text NOT NULL DEFAULT 'idle',
    last_successful_connection_at timestamptz NULL,
    connection_error              text NULL,

    created_at                    timestamptz NOT NULL DEFAULT now(),
    created_by                    bigint NULL,
    updated_at                    timestamptz NOT NULL DEFAULT now(),
    updated_by                    bigint NULL,

    CONSTRAINT profile_name_not_empty
        CHECK (trim(name) <> ''),
    CONSTRAINT profile_email_address_not_empty
        CHECK (trim(email_address) <> ''),
    CONSTRAINT profile_imap_port_valid
        CHECK (imap_port BETWEEN 1 AND 65535),
    CONSTRAINT profile_smtp_port_valid
        CHECK (smtp_port BETWEEN 1 AND 65535),
    CONSTRAINT profile_fetch_interval_valid
        CHECK (fetch_interval_seconds >= 5),
    CONSTRAINT profile_imap_security_valid
        CHECK (imap_security IN ('tls', 'starttls')),
    CONSTRAINT profile_smtp_security_valid
        CHECK (smtp_security IN ('tls', 'starttls')),
    CONSTRAINT profile_auth_type_valid
        CHECK (auth_type IN ('basic', 'oauth2')),
    CONSTRAINT profile_oauth_provider_valid
        CHECK (oauth_provider IN ('google', 'microsoft')),
    CONSTRAINT profile_connection_state_valid
        CHECK (connection_state IN ('idle', 'ready', 'error', 'reauthorization_required'))
);

CREATE INDEX profile_domain_idx ON email.profile (domain_id);

-- +goose Down
DROP TABLE IF EXISTS email.profile;
DROP SCHEMA IF EXISTS email;
