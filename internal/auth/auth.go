// Package auth defines the authorized caller session used by the service.
package auth

import (
	"context"
	"strings"
)

// AccessMode identifies the operation authorized for an API method.
type AccessMode uint8

const (
	Delete AccessMode = 1 << iota
	Edit
	Read
	Add
)

type contextKey struct{}

// Manager resolves an incoming request into an authorized caller session.
type Manager interface {
	AuthorizeFromContext(context.Context, string, AccessMode) (*Session, error)
}

// Scope contains the caller's access settings for one object class.
type Scope struct {
	Access string
	OBAC   bool
}

// Session contains the authenticated tenant and user identity required by handlers.
type Session struct {
	UserID   int64
	DomainID int64
	Scopes   map[string]Scope
	Licenses map[string]bool

	SuperCreate bool
	SuperRead   bool
	SuperUpdate bool
	SuperDelete bool
}

// WithSession stores the authorized caller session in ctx.
func WithSession(ctx context.Context, session *Session) context.Context {
	return context.WithValue(ctx, contextKey{}, session)
}

// FromContext returns the authorized caller session stored by the interceptor.
func FromContext(ctx context.Context) (*Session, bool) {
	session, ok := ctx.Value(contextKey{}).(*Session)

	return session, ok && session != nil
}

// CheckLicenseAccess reports whether the caller has an active required license.
func (s *Session) CheckLicenseAccess(name string) bool {
	return s != nil && s.Licenses[name]
}

// CheckOBACAccess verifies the requested action against the object's access mask.
func (s *Session) CheckOBACAccess(objectClass string, access AccessMode) bool {
	if s == nil {
		return false
	}

	scope, ok := s.Scopes[objectClass]
	if !ok {
		return false
	}
	if !scope.OBAC {
		return true
	}

	required, bypass := requiredAccess(access, s)

	return bypass || strings.ContainsRune(scope.Access, required)
}

func requiredAccess(access AccessMode, session *Session) (rune, bool) {
	switch access {
	case Add:
		return 'x', session.SuperCreate
	case Read:
		return 'r', session.SuperRead
	case Edit:
		return 'w', session.SuperUpdate
	case Delete:
		return 'd', session.SuperDelete
	default:
		return 0, false
	}
}
