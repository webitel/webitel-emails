package grpc_test

import (
	"context"
	"testing"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"
	"google.golang.org/grpc/codes"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/auth"
	handler "github.com/webitel/webitel-emails/internal/handler/grpc"
	"github.com/webitel/webitel-emails/internal/service"
)

// The service refuses a missing input before it touches any dependency.
func TestUpdateEmailProfileWithoutInput(t *testing.T) {
	server := handler.NewEmailProfilesServer(&service.EmailProfileService{})
	ctx := auth.WithSession(context.Background(), &auth.Session{UserID: 70, DomainID: 1})

	_, err := server.UpdateEmailProfile(ctx, &emailpb.UpdateEmailProfileRequest{Id: 5})
	if kiterrors.Code(err) != codes.InvalidArgument || kiterrors.ID(err) != "email.profile.input_required" {
		t.Fatalf("err = %v (id %q)", err, kiterrors.ID(err))
	}
}
