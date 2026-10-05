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

// Headers duplicate envelope fields, so an event is traceable without decoding.
const (
	HeaderEventID       = "x-event-id"
	HeaderEventType     = "x-event-type"
	HeaderSchemaVersion = "x-schema-version"
)
