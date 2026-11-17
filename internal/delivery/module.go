package delivery

import (
	"go.uber.org/fx"

	"github.com/webitel/webitel-emails/config"
	"github.com/webitel/webitel-emails/infra/leader"
	mailinfra "github.com/webitel/webitel-emails/infra/mail"
	storageinfra "github.com/webitel/webitel-emails/infra/storage"
	"github.com/webitel/webitel-emails/internal/service"
)

// Module sends accepted outbound emails on every instance.
var Module = fx.Module(
	"delivery",
	fx.Provide(
		func(cfg *config.Config) Sender {
			o := cfg.Outbound
			return mailinfra.NewSMTPSender(mailinfra.SMTPSenderConfig{
				CommandTimeout: o.SMTPCommandTimeout, DataTimeout: o.SMTPDataTimeout,
				FinalReplyTimeout: o.SMTPFinalReplyTimeout, SendTimeout: o.DeliverySendTimeout,
			})
		},
		func(profiles *service.EmailProfileService) ProfileSource { return profiles },
		func(client *storageinfra.Client) FileOpener { return client },
		New,
		fx.Annotate(NewRecovery, fx.As(new(leader.Task)), fx.ResultTags(leader.TaskGroup)),
	),
	fx.Invoke(func(*Worker) {}),
)
