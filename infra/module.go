// Package infra wires external infrastructure used by the service.
package infra

import (
	"go.uber.org/fx"

	authinfra "github.com/webitel/webitel-emails/infra/auth"
	servicediscovery "github.com/webitel/webitel-emails/infra/discovery"
	"github.com/webitel/webitel-emails/infra/logging"
	"github.com/webitel/webitel-emails/infra/postgres"
	"github.com/webitel/webitel-emails/infra/pubsub"
	grpcserver "github.com/webitel/webitel-emails/infra/server/grpc"
	"github.com/webitel/webitel-emails/infra/tls"
)

var Module = fx.Module(
	"infra",
	authinfra.Module,
	logging.Module,
	postgres.Module,
	pubsub.Module,
	tls.Module,
	grpcserver.Module,
	servicediscovery.Module,
)
