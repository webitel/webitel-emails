// Package auth provides the connection to the Webitel authorization service.
package auth

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/infra/webitelapp"
	internalauth "github.com/webitel/webitel-emails/internal/auth"
	webitelappclient "github.com/webitel/webitel-emails/internal/auth/webitel_app"
)

// Module provides the authorized caller session manager.
var Module = fx.Module("auth", fx.Provide(NewManager))

// NewManager builds the session manager on the shared go.webitel.app connection.
func NewManager(conn webitelapp.Conn) internalauth.Manager {
	return webitelappclient.New(conn.ClientConn)
}
