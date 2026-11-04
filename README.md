# webitel-emails

Backend service for Webitel email management.

## Local requirements

- Go 1.26;
- PostgreSQL at `127.0.0.1:5432` with the `webitel` database;
- Consul at `127.0.0.1:8500`;
- RabbitMQ at `127.0.0.1:5672`.

The default local connection values are documented in
[`config/config.example.yml`](config/config.example.yml) and
[`.env.example`](.env.example).

## Run locally

Apply database migrations explicitly:

```shell
go run . migrate --config_file=config/config.example.yml
```

Start the service:

```shell
go run . server --config_file=config/config.example.yml
```

The service listens for gRPC connections at `127.0.0.1:8080`, publishes its
health status through the standard gRPC health service, and registers itself in
Consul. Database migrations are never applied automatically when the server
starts.

## Tests

Unit tests need nothing but Go. Integration tests are excluded from this run:

```shell
go test ./...
```

The integration tests under `test/integration` need PostgreSQL. They start one
shared instance through Docker, or use an existing database when
`EMAILS_TEST_POSTGRES_DSN` is set:

```shell
go test -tags=integration ./test/integration/...

# Or use an existing disposable database instead of Docker:
EMAILS_TEST_POSTGRES_DSN='postgres://webitel:webitel@127.0.0.1:5432/webitel_test?sslmode=disable' \
    go test -tags=integration ./test/integration/...
```

They apply the embedded migrations themselves and empty the email tables between
tests, so point them at a throwaway database. An explicitly requested integration
run fails when neither Docker nor the configured database is available; it never
passes by silently skipping PostgreSQL checks.

## Inbound email

The service reads mailboxes over IMAP, parses each message and stores it as a
Thread with its Messages. Attachments and inline images are uploaded to the
`storage` service and linked to their Message, so an email is stored whole.

A Message that becomes `ready` writes an event to the Transactional Outbox, and
`flow_manager` starts the Flow of its profile from it; see
[docs/events.md](docs/events.md), including the rollout order. The Flow can put
the Thread into an Inbound Email Queue of `call_center` with its `joinQueue`
application. With its Email distribution enabled, `call_center` then assigns the
email to an agent and keeps its lifecycle: taking, postponing, resuming,
returning and completing it, waking it on a new email, and returning it to the
queue when an offer expires, a deadline passes or the agent logs out (see the
`call_center` README). With the snapshot publisher enabled, the Thread's status,
queue, current agent and postponement deadline reach this service as a versioned
projection of `call_center` snapshots.
Operators can read their emails through the read API below; Workspace events
and operator commands are not there yet, so an assigned email still cannot be
handled.

Do not enable polling in production until these parts are released. Production
use will also need compatible `call_center`, `flow_manager` and `engine`
releases, a configured Inbound Email Queue and agents allowed to take emails.

## Read API

`EmailThreads` has five read methods: `ListEmailThreads`, `LocateEmailThread`
(without messages), `ListEmailThreadMessages`, `LocateEmailThreadMessage` and
`GetEmailAttachmentLink`.

- Threads are filtered by `assignment` (`CURRENT`, `HISTORY` or `ALL`, always
  the caller's own), status, profile and contact, and paged by `page`/`size`
  (at most 100).
  Zero means unset: `page` 0 is the first page, `size` 0 is 20, `contact_id` 0
  is any contact, an unset `assignment` is `ALL`; negative values are rejected.
- Messages are paged by an opaque `cursor`. Pages never repeat a message, but
  one completed later with an earlier receive time shows up only when the list
  is read again from the start.
- Only complete (`ready`) messages are returned, and blind copies never.

Who counts as a participant, what history they see and the other product
rules are MVP assumptions (stage 13 of `Emails_Tasks.md`, П1–П9) not yet
confirmed by business analysis.

## Access to Email Threads

The `EmailThreads` API is authorized in two steps. The gRPC interceptor first
checks the caller's role for the `email_thread` object class: read methods need
`read`, and binding or unbinding a contact needs `update`; without it the call
fails with `PermissionDenied` before reaching the service. The class must be
registered in `directory.wbt_class` and granted to operator roles, or every
call is refused. The `SuperRead` permission only lifts that check; it never
opens a Thread the caller may not see.

The service then decides per Thread. A user sees a regular Thread of their
domain that has a complete message when they are its current agent or took it
into work earlier (the participant journal, see
[docs/events.md](docs/events.md)); any other Thread is reported as not found.
Only the current agent after Take (status `in_progress` or `postponed`) may
change its contact; a Thread they see but are not assigned to fails with
`FailedPrecondition` and the stable id `email.thread.not_assigned`. Both checks
follow the latest applied projection of `call_center`, which may lag behind it:
right after an assignment the Thread may still be reported as not found, and
until a newer snapshot is applied a previous agent may still change the
contact.

## Email bodies and attachment links

A stored HTML body is never returned as it is. The read API sanitizes it on
every read with an explicit allowlist of elements, attributes, URL schemes and
CSS properties: scripts, forms, frames, remote images and other external
content are removed, and `remote_content_blocked` tells that something was.

Files are reached only through short-lived download links that `storage`
signs after the service has checked the Thread, the message and the
attachment; the file id never comes from a client, and links are neither
stored nor logged. `storage.link_ttl` sets their life (5 minutes by default, at
most 15). An inline image referenced by `cid:` is linked only when it is a
stored PNG, JPEG, GIF or WebP part of the same message. When `storage` does not
answer within `storage.link_timeout`, messages are still returned with such
images removed and `inline_content_status` set to `UNAVAILABLE`, while
`GetEmailAttachmentLink` fails with `Unavailable`.

Configuration can also be supplied through environment variables. For local
development, copy `.env.example` to `.env`, adjust its values, and export them
before running the commands without `--config_file`:

```shell
set -a
source .env
set +a
go run . migrate
go run . server
```

Configuration priority is: command-line flags, environment variables, the
configuration file, then built-in defaults. TLS certificate verification is
enabled by default; the local examples disable it only for development.

Stop the service with `Ctrl+C`. It first enters the draining state, deregisters
from Consul, and then gracefully stops its dependencies.
