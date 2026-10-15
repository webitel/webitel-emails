package migrate

import (
	"testing"

	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
)

func TestApplicationGraphIsComplete(t *testing.T) {
	if err := fx.ValidateApp(modules(&config.Config{})); err != nil {
		t.Fatalf("application graph: %v", err)
	}
}
