package inbound

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/model"
)

// AttachmentGate refuses an email that carries files, because storing them is
// task 6. It is a temporary stage of the pipeline and is removed with that task.
type AttachmentGate struct {
	next ParsedMessageHandler
}

var _ ParsedMessageHandler = (*AttachmentGate)(nil)

// NewAttachmentGate puts the gate in front of the persistence handler.
func NewAttachmentGate(next *PersistenceHandler) *AttachmentGate {
	return &AttachmentGate{next: next}
}

// Handle stops before anything is written. A skipped part counts too: its
// metadata would be lost just as silently as the file itself.
func (g *AttachmentGate) Handle(ctx context.Context, message *model.ParsedEmail) error {
	if message != nil && len(message.Parts) > 0 {
		return kiterrors.New(
			"email carries attachments, which are stored by a later task",
			kiterrors.WithID("inbound.attachment_gate.unsupported"),
		)
	}

	return g.next.Handle(ctx, message)
}
