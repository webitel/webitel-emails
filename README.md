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

`EmailThreads` has six read methods: `ListEmailThreads`, `LocateEmailThread`
(without messages), `ListEmailThreadMessages`, `LocateEmailThreadMessage`,
`GetEmailAttachmentLink` and `ListEmailThreadActions`, the action journal of a
Thread (see [docs/events.md](docs/events.md)).

- Threads are filtered by `assignment` (`CURRENT`, `HISTORY` or `ALL`, always
  the caller's own), status, profile and contact, and paged by `page`/`size`
  (at most 100).
  Zero means unset: `page` 0 is the first page, `size` 0 is 20, `contact_id` 0
  is any contact, an unset `assignment` is `ALL`; negative values are rejected.
- Messages are paged by an opaque `cursor`. Pages never repeat a message, but
  one completed later with an earlier receive time shows up only when the list
  is read again from the start.
- Only complete (`ready`) messages are returned. Blind copies are returned only
  to the author of an outbound email; an inbound email never shows them.
- An outbound email also carries its `author`, `delivery_status` (`PENDING`
  right after Send), `delivery_error_code` with a fixed short `delivery_error`
  text, and `delivered_at`. `LocateEmailThread` fills `reply_signature`: the
  profile signature with `${agent_name}` set to the caller's name.
- A bounce on an outbound email with Bcc, and an auto-reply from an address
  that was only in its Bcc, are shown to the author of that email alone: they
  may name a blind copy. The email is found as threading links it (the bounced
  Message-ID, In-Reply-To, then References from the last). Other users do not
  get the message, its preview or its parts; the Thread itself still changes
  for them (`last_message_at`, version, `message.created`).

Who counts as a participant, what history they see and the other product
rules are MVP assumptions (stage 13 of `Emails_Tasks.md`, П1–П9) not yet
confirmed by business analysis.

## Access to Email Threads

The `EmailThreads` API is authorized in two steps. The gRPC interceptor first
checks the caller's role for the `email_thread` object class: read methods need
`read`; binding or unbinding a contact and `SendEmailReply` need `update`;
`CreateEmailThread` and `ListEmailSenderProfiles` need `create`;
`UploadEmailAttachment` needs `read`, and the service adds `create` or `update`.
Without it the call
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
`FailedPrecondition`; its internal stable id `email.thread.not_assigned` does not
reach clients for now (see [Error responses](#error-responses)). Both checks
follow the latest applied projection of `call_center`, which may lag behind it:
right after an assignment the Thread may still be reported as not found, and
until a newer snapshot is applied a previous agent may still change the
contact.

## Error responses

The service creates errors with a gRPC code and a stable id such as
`email.thread.not_assigned`, but the shared `UnaryServerErrorInterceptor` of
`webitel-go-kit` replaces the id with a generic `api.process.*` one and puts its
own HTTP code into the response body, which Gateway returns as is. Over HTTP a
client can tell only 401, 403, 404, 400 and 500 apart: `FailedPrecondition`, a
business refusal, comes as 500; `InvalidArgument`, `AlreadyExists` and `Aborted`
come as 400. gRPC clients keep the gRPC code, but the id in the status message is
the generic one there too; the reason is in the logs.

This limitation is accepted for now (F2, `Emails_Tasks.md` 13.3); stable ids and
correct HTTP codes remain the target. Until it is fixed, a client does not rely
on stable ids or parse the message text. After a refused command or Send it
reloads the Thread and shows a generic message; repeating a Send with the same
`request_id` is safe and returns the same email or a conflict.

The delivery result of an outbound email is not such an error: it is a field of
`EmailMessage` (`delivery_error_code`), read through the read API whatever F2.

## Outbound email

Operators reply in a Thread (`SendEmailReply`) and start new Threads
(`CreateEmailThread`); both queue the email, and SMTP delivery runs in the
background. While `outbound.enabled` is off, which is the default, these methods,
`ListEmailSenderProfiles` and `UploadEmailAttachment` fail with
`email.outbound.disabled`; emails already accepted are still delivered.

- **Who may send.** A reply needs the current agent after Take, within the
  attempt and version the client saw, or, with `attempt_id` 0, a participant of a
  `processed` Thread without an agent. Sending never changes the attempt. A new
  email may come from any profile of the domain with `allow_send` (see below).
  Rights follow the latest applied projection, which may lag behind `call_center`.
- **Profiles.** `allow_send` lets operators start new emails from a profile and
  lists it in `ListEmailSenderProfiles`; a reply always goes from the profile of
  its Thread. Both need SMTP settings and, for OAuth2, a connected profile not
  waiting for reauthorization. `enabled` only turns IMAP polling on: a profile
  with `enabled` off and `allow_send` on sends, but replies to it are never read.
- **Reply-To.** A client's answer continues a Thread only when it reaches a
  mailbox of the same profile. A `Reply-To` pointing elsewhere makes answers
  arrive in another profile as a new Thread, or not at all.
- **Contact of a new Thread.** Workspace finds the contact by the To addresses
  and sends the operator's choice: `contact_id` (`LINK`, checked to belong to the
  domain) or none (`NONE`). Later inbound email does not change it.
- **Content.** HTML is sanitized by the read policy; plain text is taken from
  the request or derived from HTML. Attachments are uploaded first and attached
  once by their reference, before it expires. The plain-text signature is
  inserted by Workspace; the service fills `${agent_name}`.
- **Limits** (`outbound.*`): recipients, size of each body, number and total
  size of attachments, size of the encoded email; above them the request is
  refused as a whole.
- **Delivery.** A worker on every instance claims due emails within its own
  and per-profile limits: `sent` once the server accepted the email (not that it
  reached the mailbox), or `failed` with a code (`auth`, `recipient_rejected`,
  `message_too_large`, `connection`, `profile_unavailable`, `rejected`,
  `temporary_failure`, `interrupted`, `unknown_result`). The email is not copied
  to the Sent folder of the mailbox. Logs carry codes only: no addresses,
  credentials or SMTP replies.
- **Automatic retries.** An attempt the server certainly did not accept and
  whose cause may pass (a `4xx` reply, a broken or timed out connection, an
  unavailable dependency, a stop of the worker before the commit) is queued
  again after `outbound.delivery_retry_delays` (`1m,5m,15m`: up to 4 attempts in
  a series); when they run out, the email is `failed` with the last code. A
  certificate, an unknown host, other `5xx` replies and `unknown_result` are
  final at once: `unknown_result` means the server may have the email, so it is
  never sent again. An attempt whose instance stopped is recovered by the leader
  after its lease (`outbound.delivery_lease`): before the commit it counts as
  `interrupted`, after it as `unknown_result`. An empty list in the YAML file
  turns automatic retries off (an empty environment variable keeps the default).
- **Retry and Cancel.** `RetryEmailDelivery` queues a `failed` email again as
  the same email, with the same Message-ID, and starts a new series; it is open
  to its author with the right to reply (as for `SendEmailReply`), for the codes
  `temporary_failure`, `connection`, `interrupted`, `auth`,
  `profile_unavailable` and `rejected`, within 24 hours of acceptance and
  `outbound.delivery_manual_retries` series (1 by default, 0–3), and only with
  `outbound.enabled`. `CancelEmailDelivery` stops an email of its author that is
  queued, waits for a retry or is being sent, until the server may have it;
  `cancelled` is final, and it works with sending disabled and while another
  agent handles the Thread. Both need `email_thread` read and update and the
  `delivery_version` the client read: an older one changes nothing and fails
  with `Aborted`. The read API gives `delivery_version`, `delivery_retry_at`
  (while a scheduled retry waits; once past, the attempt may already run) and
  `manual_retry_available`; the reason of a failure comes only with `failed`.
- **Idempotency.** `request_id` identifies the intent of its author: the same
  request returns the stored email with its current status, another one with
  the same id fails with `email.message.request_conflict`.
- **Internal stable ids** of the service. While F2 holds they do not reach
  clients over gRPC or HTTP (see [Error responses](#error-responses)); clients
  tell errors apart by the gRPC or HTTP code only:
  `email.outbound.disabled`, `email.message.request_id_invalid`,
  `email.message.request_conflict`, `email.message.no_recipient`,
  `email.message.invalid_recipient`, `email.message.too_many_recipients`,
  `email.message.invalid_subject`, `email.message.body_too_large`,
  `email.message.empty`, `email.message.too_many_attachments`,
  `email.message.attachments_too_large`, `email.message.too_large`,
  `email.message.invalid_contact_mode`, `email.thread.reply_not_allowed`,
  `email.thread.attempt_version_conflict`, `email.thread.reply_parent_not_found`,
  `email.thread.contact_not_found`, `email.profile.id_required`,
  `email.profile.send_unavailable`, `email.attachment.invalid`,
  `email.attachment.too_large`, `email.attachment.upload_busy`,
  `email.attachment.rejected`, `email.attachment.upload_failed`,
  `email.storage.unavailable`, `email.attachment.reference_invalid`,
  `email.attachment.reference_unavailable`; also the existing
  `email.thread.not_found`, `store.email_profile.not_found` and
  `contacts.client.unavailable`. `RetryEmailDelivery` and `CancelEmailDelivery`
  (stage 16) add
  `email.message.delivery_version_invalid`, `email.message.delivery_version_conflict`,
  `email.message.retry_not_allowed`, `email.message.retry_expired`,
  `email.message.retry_limit_reached`, `email.message.delivery_committed`,
  `email.message.cancel_not_allowed` and `email.message.not_author`, and reuse
  `email.outbound.disabled`, `email.thread.reply_not_allowed` and
  `email.thread.attempt_version_conflict`.

For Admin: `allow_send` and `Reply-To` are profile settings with the effect
above; turning `allow_send` off stops new emails, not replies.

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
