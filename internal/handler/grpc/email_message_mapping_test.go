package grpc

import (
	"strings"
	"testing"
	"time"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/model"
)

func TestOutboundMessageToProto(t *testing.T) {
	delivered := time.UnixMilli(1_800_000_000_000)
	got := emailMessageToProto(&model.EmailMessageView{
		Direction: model.EmailMessageDirectionOutbound, Author: &model.Lookup{ID: 70, Name: "Olena"},
		Bcc:            []model.EmailAddress{{Address: "hidden@x.com"}},
		DeliveryStatus: model.EmailDeliveryStatusSent, DeliveredAt: &delivered,
	})
	if got.GetAuthor().GetId() != 70 || got.GetAuthor().GetName() != "Olena" || len(got.GetBcc()) != 1 ||
		got.GetDeliveredAt() != delivered.UnixMilli() || got.GetDeliveryError() != "" {
		t.Fatalf("sent = %+v", got)
	}

	// Every stored failure code has its proto code and a fixed text.
	for code, want := range deliveryErrors {
		got := emailMessageToProto(&model.EmailMessageView{DeliveryStatus: model.EmailDeliveryStatusFailed, DeliveryErrorCode: code})
		if got.GetDeliveryErrorCode() != want.code || got.GetDeliveryErrorCode() == emailpb.EmailDeliveryError_EMAIL_DELIVERY_ERROR_UNSPECIFIED ||
			!strings.HasSuffix(got.GetDeliveryError(), ".") || !strings.EqualFold(emailpb.EmailDeliveryError_name[int32(want.code)], "EMAIL_DELIVERY_ERROR_"+string(code)) {
			t.Errorf("%s = %v %q", code, got.GetDeliveryErrorCode(), got.GetDeliveryError())
		}
	}
	if len(deliveryErrors) != len(emailpb.EmailDeliveryError_name)-1 {
		t.Fatalf("%d texts for %d codes", len(deliveryErrors), len(emailpb.EmailDeliveryError_name)-1)
	}
}
