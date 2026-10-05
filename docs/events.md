# email-service events

email-service writes every event to the `email.outbox` table in the same
transaction as the change it describes; a leader-only relay publishes it to
RabbitMQ. Delivery is at-least-once: consumers deduplicate by `event_id`.

## `email.message.inbound`, schema 1

Starts Flow for a new inbound Message. Emitted once per Message, when it becomes
`ready`, only for a `regular` Message of a `regular` Thread: auto-replies,
bounces and service Threads produce no event (technical design 2.1.17).

| Property | Value |
|---|---|
| Exchange | `email.events` (topic) |
| Routing key | `email.message.inbound.v1` |
| Content type | `application/json` |

Adding a field keeps `schema_version`; changing or removing one bumps it and the
routing key suffix. Consumers must ignore unknown fields.

Messages are persistent and published with confirms and `mandatory`: a message
that matches no queue fails the publish and stays in the Outbox. Headers repeat
envelope fields for tracing: `x-event-id`, `x-event-type`, `x-schema-version`.

### Topology

Every object is durable and never auto-deleted. A redeclare with other
parameters fails the channel, so both sides use exactly these values.

| Object | Declared by | Parameters |
|---|---|---|
| Exchange `email.events` | email-service, flow_manager | `topic` |
| Queue `flow.email.inbound` | flow_manager | `x-queue-type = quorum`, `x-dead-letter-exchange = email.events.dlx`, `x-delivery-limit` (default 20); bound to `email.events` with `email.message.inbound.v1` |
| Exchange `email.events.dlx` | flow_manager | `fanout` |
| Queue `flow.email.inbound.dlq` | flow_manager | `x-queue-type = quorum`; bound to `email.events.dlx` without a routing key |

### Envelope

| Field | Type | Notes |
|---|---|---|
| `event_id` | UUID | Deduplication key of the consumer |
| `event_type` | string | `email.message.inbound` |
| `schema_version` | int | `1` |
| `occurred_at` | RFC 3339, UTC | When the Message became `ready` |
| `domain_id` | int64 | |
| `profile_id` | int64 | Email Profile |
| `flow_id` | int64 or `null` | Flow of the profile at that moment; `null` means no user Flow |
| `thread_id` | int64 | `email.thread.id` |
| `message_id` | int64 | `email.message.id` |
| `snapshot` | object | See below |

### Snapshot

| Field | Type | Notes |
|---|---|---|
| `internet_message_id` | string | RFC 5322 `Message-ID` |
| `in_reply_to` | string | |
| `subject` | string | |
| `message_kind` | string | Always `regular` in schema 1 |
| `thread_kind` | string | Always `regular` in schema 1 |
| `contact_id` | int64 or `null` | Resolved contact of the Thread |
| `received_at` | RFC 3339, UTC | IMAP receive time |
| `from`, `sender`, `reply_to`, `to`, `cc` | array of `{address, name}` | Header order; Bcc is never sent |
| `body_text` | string | Plain text; HTML is never sent |
| `attachments` | array | Stored attachment parts in MIME order, in the shape of flow_manager `model.File`: `id` (storage file id), `name`, `size`, `mime`, `view_name` (`null`), `channel` (`mail`). Inline parts are not listed |
| `body_text_truncated` | bool | `body_text` was cut |
| `from_truncated`, `sender_truncated`, `reply_to_truncated`, `to_truncated`, `cc_truncated` | bool | The list or one of its addresses or names was cut |
| `attachments_truncated` | bool | The list or one of its file names or MIME types was cut |

### Limits

The snapshot is bounded by the `inbound_event.*` configuration so that any email
fits `max_payload_size`; startup fails when the limits could exceed it. The
limits themselves are capped: body 8 MiB, field 64 KiB, 10 000 items per list
and a 16 MiB payload, the RabbitMQ 4 default `max_message_size`.

- `body_text` keeps at most `max_body_text_size` bytes, each list at most
  `max_addresses_per_type` or `max_attachments` items;
- the subject, identifiers and file names keep at most `max_field_size` bytes,
  addresses 320 bytes, display names 256 bytes and MIME types 255 bytes; a cut
  subject or identifier is not flagged;
- text is valid UTF-8 and is cut on a character boundary; control characters
  are removed, and header values have line breaks replaced with spaces.

## Consumer: flow_manager

`flow_manager` stores each event in its Inbox, the `flow.email_event` table, and
acknowledges the delivery only after the row is committed. Inbox workers then
start the Flow of `flow_id`; an event without `flow_id` is accepted with no Flow.

- Deduplication by `event_id` lasts as long as the accepted row is kept
  (`email_events_retention`, 30 days by default). Replaying an event from the
  dead letter queue after that starts its Flow again.
- The dead letter queue receives events that no retry can store: an invalid or
  unsupported envelope, an `x-event-id` header that differs from `event_id`, a
  PostgreSQL data error, and deliveries past `x-delivery-limit`. Database
  outages are retried and never dead-lettered.
- A Flow may still start twice if `flow_manager` stops between starting it and
  recording the event as accepted. Flows receive `email_event_id`, and steps
  with side effects, such as joining a queue, must be idempotent by it.

## Rollout

Publishing is mandatory, so events wait in the Outbox until their queue exists:

1. apply the `call_center` migration `26.dev/email_service_inbox.sql`;
2. start `flow_manager` with `EMAIL_EVENTS=true`;
3. check that the exchanges, both queues, their bindings and a consumer of
   `flow.email.inbound` exist;
4. only then enable the relay and polling of email-service.

Until step 2 the relay logs `unroutable` and retries without spending attempts.
For high availability enable `EMAIL_EVENTS=true` on every `flow_manager`
instance.
