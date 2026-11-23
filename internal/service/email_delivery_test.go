package service

import (
	"testing"
	"time"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
)

func TestMarkManualRetry(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	reader := &auth.Session{UserID: 70, Scopes: map[string]auth.Scope{"email_thread": {Access: "rw", OBAC: true}}}
	for _, tt := range []struct {
		name   string
		change func(*model.EmailMessageView, *config.OutboundConfig)
		want   bool
	}{
		{"inbound", func(m *model.EmailMessageView, _ *config.OutboundConfig) { m.Author = nil }, false},
		{"unknown result", func(m *model.EmailMessageView, _ *config.OutboundConfig) {
			m.DeliveryErrorCode = model.EmailDeliveryErrorUnknownResult
		}, false},
		{"rejected", func(m *model.EmailMessageView, _ *config.OutboundConfig) {
			m.DeliveryErrorCode = model.EmailDeliveryErrorRejected
		}, true},
		{"series used up", func(m *model.EmailMessageView, _ *config.OutboundConfig) { m.DeliveryManualRetries = 1 }, false},
		{"no manual series", func(_ *model.EmailMessageView, c *config.OutboundConfig) { c.DeliveryManualRetries = 0 }, false},
		{"window passed", func(m *model.EmailMessageView, _ *config.OutboundConfig) { m.ReceivedAt = now.Add(-ManualRetryWindow) }, false},
	} {
		cfg := config.OutboundConfig{Enabled: true, DeliveryManualRetries: 1}
		m := &model.EmailMessageView{Author: &model.Lookup{ID: 70}, DeliveryStatus: model.EmailDeliveryStatusFailed,
			DeliveryErrorCode: model.EmailDeliveryErrorTemporaryFailure, ReceivedAt: now.Add(-time.Hour), ReplyAllowed: true}
		tt.change(m, &cfg)
		s := &EmailDeliveryService{cfg: cfg, now: func() time.Time { return now }}
		s.MarkManualRetry(reader, m)
		if m.ManualRetryAvailable != tt.want {
			t.Errorf("%s: %v, want %v", tt.name, m.ManualRetryAvailable, tt.want)
		}
	}
}
