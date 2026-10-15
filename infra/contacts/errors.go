package contacts

import (
	"fmt"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"
)

// ErrUnavailable means the Contacts service did not answer, so the same search
// may succeed later. Every failure is reported this way on purpose: a wrongly
// permanent failure would leave a Thread without a contact forever, while a
// retry costs little and never blocks the email.
var ErrUnavailable = kiterrors.New(
	"contacts service is unavailable",
	kiterrors.WithID("contacts.client.unavailable"),
	kiterrors.WithCode(codes.Unavailable),
)

func unavailable(err error) error {
	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}
