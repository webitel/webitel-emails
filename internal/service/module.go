package service

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	contactsinfra "github.com/webitel/webitel-emails/infra/contacts"
	"github.com/webitel/webitel-emails/infra/leader"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/store"
)

// Module provides application services.
var Module = fx.Module(
	"service",
	fx.Provide(
		NewEmailProfileService,
		// Bound here so infra stays unaware of this package.
		func(client *contactsinfra.Client) ContactVerifier { return client },
		func(s store.EmailThreadReadStore) ThreadRightsReader { return s },
		NewEmailThreadService,
		func(cfg *config.Config, client *storageinfra.Client) AttachmentLinker {
			return NewStorageAttachmentLinks(client, cfg.Storage)
		},
		NewEmailThreadReadService,
		func(client *storageinfra.Client) AttachmentUploader { return client },
		NewEmailAttachmentUploadService,
		NewEmailSendService,
		fx.Annotate(
			NewAttachmentUploadCleanup,
			fx.As(new(leader.Task)),
			fx.ResultTags(leader.TaskGroup),
		),
	),
)
