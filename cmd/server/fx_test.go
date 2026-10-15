package server

import (
	"testing"

	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
)

// The polling scheduler requires a complete inbound pipeline, so a missing
// provider must surface here rather than at the first poll in production.
func TestApplicationGraphIsComplete(t *testing.T) {
	if err := fx.ValidateApp(modules(&config.Config{})); err != nil {
		t.Fatalf("application graph: %v", err)
	}
}
