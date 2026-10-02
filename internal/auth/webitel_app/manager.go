// Package webitelapp resolves caller sessions through the Webitel auth service.
package webitelapp

import (
	"context"
	"time"

	"golang.org/x/sync/singleflight"
	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	authclient "buf.build/gen/go/webitel/webitel-go/grpc/go/_gogrpc"
	authmodel "buf.build/gen/go/webitel/webitel-go/protocolbuffers/go"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	"github.com/webitel/webitel-emails/internal/auth"
)

const (
	authTokenName      = "X-Webitel-Access"
	requestContextName = "grpc_ctx"
)

// Manager resolves access tokens through go.webitel.app.
type Manager struct {
	client authclient.AuthClient
	group  singleflight.Group
}

var _ auth.Manager = (*Manager)(nil)

// New creates a Webitel auth manager over an existing gRPC connection.
func New(conn *grpc.ClientConn) *Manager {
	return &Manager{client: authclient.NewAuthClient(conn)}
}

// AuthorizeFromContext resolves the access token and builds a caller session.
func (m *Manager) AuthorizeFromContext(
	ctx context.Context,
	objectClass string,
	access auth.AccessMode,
) (*auth.Session, error) {
	info, ok := ctx.Value(requestContextName).(metadata.MD)
	if !ok {
		info, ok = metadata.FromIncomingContext(ctx)
	}
	if !ok {
		return nil, kiterrors.Unauthenticated(
			"authorization metadata is missing",
			kiterrors.WithID("auth.metadata.missing"),
		)
	}

	tokens := info.Get(authTokenName)
	if len(tokens) == 0 || tokens[0] == "" {
		return nil, kiterrors.Unauthenticated(
			"authorization token is missing",
			kiterrors.WithID("auth.token.missing"),
		)
	}

	outgoing := metadata.NewOutgoingContext(ctx, info)
	resolved, err, _ := m.group.Do(tokens[0], func() (any, error) {
		return m.client.UserInfo(outgoing, nil)
	})
	if err != nil {
		return nil, kiterrors.Unauthenticated(
			"unable to resolve user information",
			kiterrors.WithID("auth.user_info"),
			kiterrors.WithCause(err),
		)
	}

	userinfo, ok := resolved.(*authmodel.Userinfo)
	if !ok {
		return nil, kiterrors.Internal(
			"unexpected user information",
			kiterrors.WithID("auth.user_info.type"),
		)
	}

	return sessionFromUserInfo(userinfo, objectClass, access), nil
}

func sessionFromUserInfo(userinfo *authmodel.Userinfo, objectClass string, access auth.AccessMode) *auth.Session {
	session := &auth.Session{
		UserID:   userinfo.GetUserId(),
		DomainID: userinfo.GetDc(),
		Scopes:   make(map[string]auth.Scope, len(userinfo.GetScope())),
		Licenses: make(map[string]bool, len(userinfo.GetLicense())),
	}

	for _, license := range userinfo.GetLicense() {
		session.Licenses[license.GetId()] = license.GetExpiresAt() > time.Now().UnixMilli()
	}
	for _, permission := range userinfo.GetPermissions() {
		switch permission.GetId() {
		case "add":
			session.SuperCreate = true
		case "read":
			session.SuperRead = true
		case "write":
			session.SuperUpdate = true
		case "delete":
			session.SuperDelete = true
		}
	}
	for _, scope := range userinfo.GetScope() {
		session.Scopes[scope.GetClass()] = auth.Scope{
			Access: scope.GetAccess(),
			OBAC:   scope.GetObac(),
		}
	}

	return session
}
