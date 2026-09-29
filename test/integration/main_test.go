//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/webitel/webitel-emails/test/integration/testhelpers"
)

func TestMain(m *testing.M) {
	startCtx, cancelStart := context.WithTimeout(context.Background(), time.Minute)
	err := testhelpers.Start(startCtx)
	cancelStart()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "start integration database: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()

	stopCtx, cancelStop := context.WithTimeout(context.Background(), 30*time.Second)
	if err := testhelpers.Close(stopCtx); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "stop integration database: %v\n", err)
		if code == 0 {
			code = 1
		}
	}
	cancelStop()

	os.Exit(code)
}
