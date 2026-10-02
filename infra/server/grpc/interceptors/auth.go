// Package interceptors contains gRPC server middleware.
package interceptors

import (
	"context"
	"strings"

	"google.golang.org/grpc"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/auth"
)

const emailMethodPrefix = "/webitel.email."

// NewUnaryAuthInterceptor authenticates and authorizes Email API methods.
func NewUnaryAuthInterceptor(manager auth.Manager) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		objectClass, licenses, access, ok := methodAccess(info.FullMethod)
		if !ok {
			if strings.HasPrefix(info.FullMethod, emailMethodPrefix) {
				return nil, kiterrors.Forbidden(
					"method is not exposed for authorization",
					kiterrors.WithID("auth.interceptor.unknown_method"),
				)
			}

			return handler(ctx, req)
		}

		session, err := manager.AuthorizeFromContext(ctx, objectClass, access)
		if err != nil {
			return nil, err
		}
		for _, license := range licenses {
			if !session.CheckLicenseAccess(license) {
				return nil, kiterrors.Forbidden(
					"missing required license: "+license,
					kiterrors.WithID("auth.interceptor.license"),
				)
			}
		}
		if !session.CheckOBACAccess(objectClass, access) {
			return nil, kiterrors.Forbidden(
				"missing required permissions: "+objectClass,
				kiterrors.WithID("auth.interceptor.permission"),
			)
		}

		return handler(auth.WithSession(ctx, session), req)
	}
}

func methodAccess(fullMethod string) (string, []string, auth.AccessMode, bool) {
	serviceMethod := strings.TrimPrefix(fullMethod, "/")
	separator := strings.LastIndexByte(serviceMethod, '/')
	if separator < 0 {
		return "", nil, 0, false
	}

	serviceName := serviceMethod[:separator]
	if dot := strings.LastIndexByte(serviceName, '.'); dot >= 0 {
		serviceName = serviceName[dot+1:]
	}
	methodName := serviceMethod[separator+1:]

	service, ok := emailpb.WebitelAPI[serviceName]
	if !ok || service.ObjClass == "" {
		return "", nil, 0, false
	}
	method, ok := service.WebitelMethods[methodName]
	if !ok {
		return "", nil, 0, false
	}

	access, ok := accessMode(method.Access)

	return service.ObjClass, service.AdditionalLicenses, access, ok
}

func accessMode(value int) (auth.AccessMode, bool) {
	switch value {
	case 0:
		return auth.Add, true
	case 1:
		return auth.Read, true
	case 2:
		return auth.Edit, true
	case 3:
		return auth.Delete, true
	default:
		return 0, false
	}
}
