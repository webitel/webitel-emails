package outbound

import (
	"strings"

	"github.com/emersion/go-message/mail"
	"github.com/google/uuid"
	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"
)

const (
	// maxMessageIDBytes keeps "References: <id>" within the 998-byte header line.
	maxMessageIDBytes = 900
	maxReferences     = 20
	agentNameField    = "${agent_name}"
)

// Sender is the profile identity of an outbound email.
type Sender struct {
	Name    string
	Address string
	ReplyTo string
}

// CheckSender validates profile values, which may come from old settings.
func CheckSender(s Sender) error {
	_, addressOK := checkAddress(s.Address)
	replyToOK := s.ReplyTo == ""
	if !replyToOK {
		_, replyToOK = checkAddress(s.ReplyTo)
	}
	if !addressOK || !replyToOK || !checkName(s.Name) {
		return kiterrors.New("email profile cannot send email",
			kiterrors.WithID("email.profile.send_unavailable"), kiterrors.WithCode(codes.FailedPrecondition))
	}

	return nil
}

// ReplyHeaders returns In-Reply-To and References for a reply to the parent.
// Invalid ancestors are dropped; beyond the limit the first and the last 19 remain.
func ReplyHeaders(parentID string, parentReferences []string) (string, []string, error) {
	if !validMessageID(parentID) {
		return "", nil, kiterrors.New("parent email has no valid Message-ID",
			kiterrors.WithID("email.thread.reply_parent_not_found"), kiterrors.WithCode(codes.FailedPrecondition))
	}

	seen := map[string]bool{parentID: true}
	refs := make([]string, 0, len(parentReferences)+1)
	for _, id := range parentReferences {
		if !seen[id] && validMessageID(id) {
			seen[id] = true
			refs = append(refs, id)
		}
	}
	refs = append(refs, parentID)
	if len(refs) > maxReferences {
		refs = append(refs[:1], refs[len(refs)-maxReferences+1:]...)
	}

	return parentID, refs, nil
}

// validMessageID accepts an ID that the header parser reads back unchanged.
func validMessageID(id string) bool {
	if id == "" || len(id) > maxMessageIDBytes {
		return false
	}
	var h mail.Header
	h.Set("Message-Id", "<"+id+">")
	parsed, err := h.MessageID()

	return err == nil && parsed == id
}

// NewMessageID returns an ID without angle brackets in the domain of the profile address.
func NewMessageID(address string) string {
	domain := address[strings.LastIndexByte(address, '@')+1:]

	return uuid.NewString() + "@" + strings.ToLower(domain)
}

// Signature fills the plain-text signature template.
func Signature(template, agentName string) string {
	return strings.ReplaceAll(template, agentNameField, agentName)
}
