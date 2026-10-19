package event

// Broker topology of the email events. Every object is durable and never
// auto-deleted; both sides declare it with exactly these parameters.
const (
	// Exchange receives every email-service event; only the producer needs it.
	Exchange     = "email.events"
	ExchangeType = "topic"

	// Declared by flow_manager, documented in docs/events.md.
	InboundMessageQueue                = "flow.email.inbound"
	InboundMessageDeadLetterExchange   = "email.events.dlx"
	InboundMessageDeadLetterQueue      = "flow.email.inbound.dlq"
	DefaultInboundMessageDeliveryLimit = 20

	ContentType = "application/json"
)

// Broker topology of the Email attempt snapshots call_center publishes; same
// rules as above, documented in docs/events.md.
const (
	// AttemptExchange is declared by call_center and by this service.
	AttemptExchange     = "cc.email.events"
	AttemptExchangeType = "topic"

	// Declared by this service, the consumer of the snapshots; both queues are quorum.
	ProjectionQueue                  = "email.thread.projection"
	ProjectionQueueType              = "quorum"
	ProjectionDeadLetterExchange     = "cc.email.events.dlx"
	ProjectionDeadLetterExchangeType = "fanout"
	ProjectionDeadLetterQueue        = "email.thread.projection.dlq"
	DefaultProjectionDeliveryLimit   = 20
)

// Headers duplicate envelope fields, so an event is traceable without decoding.
const (
	HeaderEventID       = "x-event-id"
	HeaderEventType     = "x-event-type"
	HeaderSchemaVersion = "x-schema-version"
)
