package inbound

import "context"

// MIMEHandler parses raw emails and forwards the result for further handling.
type MIMEHandler struct {
	parser Parser
	next   ParsedMessageHandler
}

func NewMIMEHandler(parser Parser, next ParsedMessageHandler) *MIMEHandler {
	return &MIMEHandler{
		parser: parser,
		next:   next,
	}
}

func (h *MIMEHandler) Handle(ctx context.Context, message *Message) error {
	parsed, err := h.parser.Parse(ctx, message)
	if err != nil {
		return err
	}

	return h.next.Handle(ctx, parsed)
}
