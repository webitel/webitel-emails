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
| Exchange `email.events` | email-service, flow_manager, engine | `topic` |
| Queue `flow.email.inbound` | flow_manager | `x-queue-type = quorum`, `x-dead-letter-exchange = email.events.dlx`, `x-delivery-limit` (default 20); bound to `email.events` with `email.message.inbound.v1` |
| Exchange `email.events.dlx` | flow_manager | `fanout` |
| Queue `flow.email.inbound.dlq` | flow_manager | `x-queue-type = quorum`; bound to `email.events.dlx` without a routing key |

`engine` binds the queues of its nodes to the same exchange for the Workspace
events below, one key per user, never to `email.message.inbound.v1`.

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
- When `call_center` reopens a Thread without an active attempt, the Flow has
  `email_route_timeout` (60 seconds by default) to choose a queue with
  `joinQueue`; afterwards `flow_manager` joins the queue of the last finished
  attempt, given to the Flow as `cc_fallback_queue_id`, and a late `joinQueue`
  fails with `email_route.window_closed`. Email Flows should call `joinQueue` as
  early as possible. Such an event starts its Flow again only before the
  deadline, so a Flow interrupted after the join does not repeat its later steps.

## Rollout

Publishing is mandatory, so events wait in the Outbox until their queue exists:

1. apply the `call_center` migration `26.dev/email_service_inbox.sql`;
2. start `flow_manager` with `EMAIL_EVENTS=true`;
3. check that the exchanges, both queues, their bindings and a consumer of
   `flow.email.inbound` exist;
4. only then enable Email Profiles, which starts polling. In production keep
   them disabled until the conditions of the "Inbound email" section of the
   README are met.

Until step 2 the relay logs `unroutable` and retries without spending attempts.
For high availability enable `EMAIL_EVENTS=true` on every `flow_manager`
instance.

## Workspace events, schema 1

These events tell an operator's Workspace, through `engine`, that a Thread
they see has changed. An event is a signal, not data: Workspace reads the
Thread, its Messages and actions again through the read API, which keeps the
only check of access, sanitizing and attachment links. This is the backend
contract of stage 17 (EM-26); its WebSocket form, in the `engine`
documentation, waits for confirmation by Workspace.

| Event type | Routing key | When |
|---|---|---|
| `email.workspace.thread.changed` | `email.workspace.thread.changed.v1.<domain_id>.<user_id>` | An applied projection, a new action or participant, a contact change |
| `email.workspace.message.created` | `email.workspace.message.created.v1.<domain_id>.<user_id>` | A Message of a regular Thread became `ready`; a contact resolved with it comes with this event |
| `email.workspace.message.delivery` | `email.workspace.message.delivery.v1.<domain_id>.<user_id>` | Reserved for the SMTP status of an outbound Message (stage 17.7); not sent yet |

All go to `email.events` (topic) as `application/json`, persistent, with confirms
and the headers `x-event-id`, `x-event-type` and `x-schema-version`, and
**without** `mandatory`: as for calls and chats, the key names the recipient,
and a recipient without an open socket has no bound queue, so the event is
simply not delivered. Adding a field keeps `schema_version`; changing or
removing one bumps it and the version in the routing key. Consumers must ignore
unknown fields.

### Recipients

One event is written per recipient, in the transaction of the change, so the
payload never lists users. The recipients are the current user of the Thread
before and after the change and every user of its participant journal, without
repeats. A Thread without a ready Message, a service Thread and a Thread without
any of these users produce no event. The event gives each recipient their
access after the change; the read API still decides what they see.

### Envelope

Every field is present; a missing value is an explicit `null`.

| Field | Type | Notes |
|---|---|---|
| `event_id` | UUID | UUIDv5, see "Event id"; also the dedup key of the row: a republished row keeps it |
| `event_type` | string | One of the types above |
| `schema_version` | int | `1` |
| `occurred_at` | RFC 3339, UTC | Time of the change |
| `domain_id` | int64 | |
| `user_id` | int64 | The recipient |
| `thread_id` | int64 | |
| `access` | string | The recipient's relation after the change, by the read API rules: `current`, `history` or `none` |
| `workspace_version` | int64 | `email.thread.workspace_version` after the change, for every `access` |
| `projection_version` | int64 or `null` | The applied `call_center` snapshot at that moment; `null` only for a Thread never projected |
| `message_id` | int64 or `null` | Only for `message.created` and `message.delivery` |
| `changes` | array of string | Only for `thread.changed`: some of `projection`, `actions`, `participants`, `contact`; empty for `access = none` |

`access = none` comes only with `thread.changed`: the recipient lost the Thread,
for example after an offer they did not take. It is an invalidation: it keeps
both versions, so a late one is told from a later assignment, but carries no
Message and no change. No subject, body, address, attachment, reason or comment
is ever sent. A payload stays under 4 KiB, independent of the
`inbound_event.*` limits.

### Event id

`event_id` is the UUIDv5 (SHA-1, Go `uuid.NewSHA1`) of the namespace
`38ad4b08-3ead-470a-ba68-4b72ffd03723` and the UTF-8 string
`<event_type>:<domain_id>:<thread_id>:<workspace_version>:<user_id>`, numbers
in decimal without sign or leading zeros:

| String | `event_id` |
|---|---|
| `email.workspace.thread.changed:1:1024:17:42` | `5b247aa1-9433-53ce-b37d-346cec14bff5` |
| `email.workspace.message.created:1:1024:19:42` | `2c68a74d-7da5-58dd-b6c0-7a9de072241b` |

### Versions

Three versions are never mixed, and each is compared only within one Thread:

- `attempt_version` belongs to one attempt and serves the commands of `engine`;
- `projection_version` orders the `call_center` snapshots of a Thread, across
  attempts; it is not lowered by a stale snapshot;
- `workspace_version` grows by one, under the lock of the Thread, with every
  change of its visible state: a ready Message, an applied projection, an added
  action or participant, a contact, later an SMTP status. A repeated change
  that alters nothing leaves it and writes no event. `0` means no change since
  the release that added it; old changes get no events.

The read API returns `projection_version` and `workspace_version` of every
Thread (`EmailThread` fields 20 and 21, `0` for none). Messages and actions are
read by further calls, so one version does not make several calls atomic.

### Publishing

`workspace_events.enabled` (`WORKSPACE_EVENTS_ENABLED`, `false` by default)
turns on writing new events; rows already in the Outbox are published either
way, and `workspace_version` grows either way.

Workspace events go through a publisher of their own on the same connection:
confirms without `mandatory`, so they never wait for a route. Flow events keep
`mandatory` and their retry without a route. Every broker failure (lost
connection, confirm timeout, nack) and every database failure pause the whole
batch as before. `engine` declares `email.events` with the same parameters and
binds the queues of its nodes; this service declares no queue for it.

## `cc.email.attempt.snapshot`, schema 1

Consumed by this service. `call_center` writes the full Email attempt to its
`cc_email_attempt_outbox` in the transaction of every transition, and a
publisher sends each row here. The Thread keeps a versioned projection of it:
status, queue, current and last agent, `postponed_until` and the current attempt
version. `call_center` stays the owner of these data.

| Property | Value |
|---|---|
| Exchange | `cc.email.events` (topic) |
| Routing key | `cc.email.attempt.snapshot.v1` |
| Content type | `application/json` |

Adding a field keeps `schema_version`; changing or removing one bumps it and the
routing key suffix. Consumers must ignore unknown fields. Messages are
persistent and published with confirms and `mandatory`, with the headers
`x-event-id`, `x-event-type` and `x-schema-version`.

### Topology

| Object | Declared by | Parameters |
|---|---|---|
| Exchange `cc.email.events` | call_center, email-service | `topic` |
| Queue `email.thread.projection` | email-service | `x-queue-type = quorum`, `x-dead-letter-exchange = cc.email.events.dlx`, `x-delivery-limit` (default 20); bound to `cc.email.events` with `cc.email.attempt.snapshot.v1` |
| Exchange `cc.email.events.dlx` | email-service | `fanout` |
| Queue `email.thread.projection.dlq` | email-service | `x-queue-type = quorum`; bound to `cc.email.events.dlx` without a routing key |

Both sides redeclare their objects after a reconnect until it succeeds. The
snapshots reach this service alone; Workspace hears of them through the
Workspace events above.

### Envelope

Every field is present; a missing value is an explicit `null`.

| Field | Type | Notes |
|---|---|---|
| `event_id` | UUID | UUIDv5 of the event type and `projection_version`: a republished row keeps it |
| `event_type` | string | `cc.email.attempt.snapshot` |
| `schema_version` | int | `1` |
| `occurred_at` | RFC 3339, UTC | Time of the transition |
| `domain_id` | int64 | |
| `thread_id` | int64 | `email.thread.id` |
| `projection_version` | int64 | `sequence` of the outbox row; orders the snapshots of a Thread, gaps are normal |
| `transition` | string | Transition of the attempt, or `inbound_reopened` |
| `source` | string | `engine`, `flow` or `system` |
| `causation_id` | string or `null` | `email_event_id` of the email that caused it, for tracing |
| `attempt_id` | int64 or `null` | |
| `attempt_version` | int64 or `null` | Version of the attempt for operator commands; starts at 0 for every attempt |
| `attempt_state` | string or `null` | `waiting`, `offering`, `active`, `postponed`, `processing` or `leaving` |
| `result` | string or `null` | Only for `leaving`: `processed` or `processing_timeout` |
| `queue_id` | int64 or `null` | |
| `agent_id` | int64 or `null` | `cc_agent.id` |
| `agent_user_id` | int64 or `null` | User of the agent, set together with `agent_id` |
| `postponed_until` | RFC 3339 or `null` | Only for `postponed` |
| `actor_user_id` | int64 or `null` | User who sent the command; only for `source = engine` |
| `comment` | string or `null` | Plain text of the operator: the reason of a return or the comment of a transfer, up to 500 characters |
| `transfer_to_queue_id` | int64 or `null` | Target queue; only for `transferred` |
| `transfer_to_user_id` | int64 or `null` | User of the target agent; only for `transferred`, never with `transfer_to_queue_id` |

No email content, addresses or attachments are sent; `comment` is written by the
operator, not taken from the email.

### Validation

A snapshot is rejected to the dead letter queue, never mapped to a guessed
status, when any of these fails:

- `event_id` is set and equals a non-empty `x-event-id` header; type and schema
  are known; `domain_id`, `thread_id` and `projection_version` are positive;
  `transition`, `attempt_state`, `result` and `source` are known values;
- `agent_id` and `agent_user_id` are both set or both `null`;
- `inbound_reopened`: every attempt field, `result`, `agent_*` and
  `postponed_until` are `null`; `queue_id` may be `null`;
- any other transition: positive `attempt_id` and `queue_id`, non-negative
  `attempt_version`, `result` only for `leaving`, `postponed_until` exactly for
  `postponed`, no agent for `waiting`;
- `actor_user_id` only for `source = engine` and positive; `comment` only for
  `returned` or `transferred` of `engine`, 1 to 500 characters;
- `transferred` is exactly `leaving` with `result = transfer` and has exactly one
  positive `transfer_to_queue_id` or `transfer_to_user_id`, which no other
  transition has; `transfer_created` is `waiting` or `offering`.

A new value is unknown until this service maps it, so a release that maps it is
deployed before `call_center` sends it, as for the Transfer values above.

### Mapping to the Thread

A snapshot applies only to an existing `regular` Thread of the same domain and
only when its `projection_version` is greater than the stored one; a repeated or
older snapshot leaves the projection as it is, but may still update
`last_handled_*` and the participant journal (see below). Delivery order does
not matter.

| Snapshot | Thread status | Current agent | `postponed_until` | `completed_at` |
|---|---|---|---|---|
| `waiting` | `new` | `null` | `null` | `null` |
| `offering` | `distributed` | agent | `null` | `null` |
| `active` | `in_progress` | agent | `null` | `null` |
| `postponed` | `postponed` | agent | snapshot value | `null` |
| `processing` | `in_progress` | agent | `null` | `null` |
| `leaving` with `processed` or `processing_timeout` | `processed` | `null` | `null` | `occurred_at` |
| `leaving` with `transfer` | `new` | `null` | `null` | `null` |
| `inbound_reopened` | `new` | `null` | `null` | `null` |

`queue_id` is taken from an attempt snapshot and kept after the attempt ends. The
current attempt and its version are cleared by `leaving` and by the marker.

`inbound_reopened` is written by `call_center` when a new inbound email finds
no active attempt of the Thread. It opens the Thread as `new` without an
attempt, so an email that no queue receives stays visible. It updates
`queue_id` only when it carries one. Its `projection_version` is above the final
snapshot of the previous attempt and below the `created` snapshot of the next
one, so a late final snapshot cannot close the Thread again.

A Transfer ends the attempt with `transfer` and creates the next one in the same
`call_center` transaction (`transfer_created`, mapped by its state like any
attempt). The transferred attempt does not complete the Thread; its agent, the
sender, becomes the last handled agent and stays a participant. The recipient of
a direct transfer gets an offer and becomes a participant only after Take.

The last agent who finished an attempt is kept with its own version: a `leaving`
snapshot sets it when its `projection_version` is above `last_handled_version`,
even if the projection already applied a newer snapshot. The marker and a new
`waiting` attempt keep it.

A snapshot that proves its agent took the Thread into work (`active`,
`postponed`, `processing`, or `leaving` with an agent) adds the agent's user to
the participant journal `email.thread_participant`, in the same transaction and
whether or not the projection applied the snapshot. The journal is a set: rows
are only added, their versions and times only widen, so the order and repetition
of deliveries do not matter. `waiting`, `offering` and the marker never add a
row, so an offer that was not taken gives no history. Only a participant or the
current agent of a Thread may read it through the read API; `last_handled_*` is
history, not access. Authors of outbound emails will join the journal with
outbound email support.

### Action journal

Every snapshot and marker of a regular Thread of the domain adds one row to
`email.thread_action`, keyed by `projection_version`, in the transaction of the
projection and whether or not the projection applied it; a repeated or late
delivery adds nothing. The journal is the Email history of the Thread, apart
from any system audit: it keeps the transition, attempt state and result, queue,
agent, the author of an engine command, the reason of a return or the comment of
a transfer as plain text, and the target of a transfer. It starts with the
release that has it; earlier transitions are not restored.

`ListEmailThreadActions` returns it oldest first, paged by an opaque cursor, to
the users who see the Thread through the read API. Names come from
`directory.wbt_user` of the same domain. With `thread_action.retention`
(`THREAD_ACTION_RETENTION`, `0` by default: keep everything) the leader deletes
older actions once an hour in batches; agree the value before production.

Other consumers of this event, such as delivery to Workspace, must expect
`inbound_reopened` without an attempt.

### Consumer: email-service

Every instance consumes `email.thread.projection`, apart from leadership and
mailbox assignment, with the prefetch of `thread_projection.prefetch`. It
declares the topology above before each subscription and again after a lost
connection or channel, with a growing pause.

- A snapshot, the last handled agent and the participant are written in one
  transaction, and the delivery is acknowledged only after it commits.
- A repeated or older snapshot is acknowledged without changing the projection;
  `last_handled_*` and the participant journal are still updated by their own
  rules.
- The dead letter queue receives snapshots that no retry can apply: an invalid
  or unknown envelope, a Thread that does not exist in that domain, a service
  Thread, values the database refuses, and deliveries past `x-delivery-limit`
  (`thread_projection.delivery_limit`, which cannot change once the queue exists).
- A database outage keeps the delivery and retries it until it succeeds, the
  service stops or the channel closes; it is never dead-lettered.
- The applied, stale and rejected counts, the recorded participants and actions and the lag
  (`now - occurred_at`) are logged every `thread_projection.report_interval`;
  payloads are never logged.

### Changing the email-service version

All instances share `email.thread.projection`, and a version without the
participant or action journal acknowledges snapshots without recording them;
those snapshots never come again. A version without the Transfer values sends
them to the dead letter queue. So old and new instances must never consume
at the same time, both on upgrade and on rollback:

1. Stop the snapshot publisher on every `call_center` node
   (`EMAIL_ATTEMPT_PUBLISHER=false`, read at start, so the nodes restart).
   Snapshots wait in its outbox.
2. Wait until no node publishes and the queue holds `ready = 0` and
   `unacked = 0`. On rollback the new version drains it.
3. Replace every instance.
4. Start the publisher again only once the new version runs everywhere; keep it
   stopped while an old version runs.

Before read access is granted after the journal arrives, run the check of
[participant-backfill.md](participant-backfill.md). A rollback keeps the
journal table; dropping it needs a separate decision and a backup, since the
backfill cannot rebuild it completely.

### Rollout

Deploy email-service with its consumer first. Then apply the `call_center`
migrations `email_status_sync.sql` and `email_route.sql`, deploy every
`call_center` node with the snapshot publisher off, and only then `flow_manager`.
Enable the publisher last, as the `call_center` README says; until then
snapshots wait in its outbox. Wake-ups recorded before the migration get no
fallback queue.
