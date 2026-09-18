package grpc

import (
	"context"

	kiterrors "github.com/webitel/webitel-go-kit/pkg/errors"

	emailpb "github.com/webitel/webitel-emails/api/email"
	"github.com/webitel/webitel-emails/internal/auth"
	"github.com/webitel/webitel-emails/internal/model"
	"github.com/webitel/webitel-emails/internal/service"
)

// EmailProfilesServer exposes Email Profile use cases over gRPC.
type EmailProfilesServer struct {
	emailpb.UnimplementedEmailProfilesServer

	service *service.EmailProfileService
}

var _ emailpb.EmailProfilesServer = (*EmailProfilesServer)(nil)

// NewEmailProfilesServer creates an Email Profile gRPC handler.
func NewEmailProfilesServer(profileService *service.EmailProfileService) *EmailProfilesServer {
	return &EmailProfilesServer{service: profileService}
}

// ListEmailProfiles returns one page of profiles from the caller's domain.
func (s *EmailProfilesServer) ListEmailProfiles(
	ctx context.Context,
	req *emailpb.ListEmailProfilesRequest,
) (*emailpb.EmailProfileList, error) {
	session, err := emailProfileSession(ctx)
	if err != nil {
		return nil, err
	}

	profiles, next, err := s.service.List(ctx, session.DomainID, model.EmailProfileFilter{
		Page:   req.GetPage(),
		Size:   req.GetSize(),
		Query:  req.GetQ(),
		Sort:   req.GetSort(),
		Fields: req.GetFields(),
	})
	if err != nil {
		return nil, err
	}

	items := make([]*emailpb.EmailProfile, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, emailProfileToProto(profile))
	}

	return &emailpb.EmailProfileList{Items: items, Next: next}, nil
}

// LocateEmailProfile returns a profile from the caller's domain.
func (s *EmailProfilesServer) LocateEmailProfile(
	ctx context.Context,
	req *emailpb.LocateEmailProfileRequest,
) (*emailpb.EmailProfile, error) {
	session, err := emailProfileSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateEmailProfileID(req.GetId()); err != nil {
		return nil, err
	}

	profile, err := s.service.Locate(ctx, session.DomainID, req.GetId())
	if err != nil {
		return nil, err
	}

	return emailProfileToProto(profile), nil
}

// CreateEmailProfile creates a profile in the caller's domain.
func (s *EmailProfilesServer) CreateEmailProfile(
	ctx context.Context,
	req *emailpb.CreateEmailProfileRequest,
) (*emailpb.EmailProfile, error) {
	session, err := emailProfileSession(ctx)
	if err != nil {
		return nil, err
	}

	profile, err := s.service.Create(
		ctx,
		session.DomainID,
		session.UserID,
		emailProfileFromProto(req.GetInput()),
		req.GetInput().GetPassword(),
	)
	if err != nil {
		return nil, err
	}

	return emailProfileToProto(profile), nil
}

// UpdateEmailProfile replaces writable profile settings in the caller's domain.
func (s *EmailProfilesServer) UpdateEmailProfile(
	ctx context.Context,
	req *emailpb.UpdateEmailProfileRequest,
) (*emailpb.EmailProfile, error) {
	session, err := emailProfileSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateEmailProfileID(req.GetId()); err != nil {
		return nil, err
	}

	profile, err := s.service.Update(
		ctx,
		session.DomainID,
		session.UserID,
		req.GetId(),
		emailProfileFromProto(req.GetInput()),
		req.GetInput().GetPassword(),
	)
	if err != nil {
		return nil, err
	}

	return emailProfileToProto(profile), nil
}

// DeleteEmailProfile removes a profile from the caller's domain.
func (s *EmailProfilesServer) DeleteEmailProfile(
	ctx context.Context,
	req *emailpb.DeleteEmailProfileRequest,
) (*emailpb.EmailProfile, error) {
	session, err := emailProfileSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateEmailProfileID(req.GetId()); err != nil {
		return nil, err
	}

	profile, err := s.service.Delete(ctx, session.DomainID, req.GetId())
	if err != nil {
		return nil, err
	}

	return emailProfileToProto(profile), nil
}

// TestEmailProfile validates the saved IMAP and SMTP connection settings.
func (s *EmailProfilesServer) TestEmailProfile(
	ctx context.Context,
	req *emailpb.TestEmailProfileRequest,
) (*emailpb.TestEmailProfileResponse, error) {
	session, err := emailProfileSession(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateEmailProfileID(req.GetId()); err != nil {
		return nil, err
	}

	result, err := s.service.Test(ctx, session.DomainID, req.GetId())
	if err != nil {
		return nil, err
	}

	return &emailpb.TestEmailProfileResponse{
		Imap: connectionTestResultToProto(result.IMAP),
		Smtp: connectionTestResultToProto(result.SMTP),
	}, nil
}

func emailProfileSession(ctx context.Context) (*auth.Session, error) {
	session, ok := auth.FromContext(ctx)
	if !ok {
		return nil, kiterrors.Unauthenticated(
			"unauthorized",
			kiterrors.WithID("email.profile.auth.session_missing"),
		)
	}

	return session, nil
}

func validateEmailProfileID(id int64) error {
	if id <= 0 {
		return kiterrors.InvalidArgument(
			"email profile id is required",
			kiterrors.WithID("email.profile.id_required"),
		)
	}

	return nil
}
