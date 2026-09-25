module github.com/webitel/webitel-emails

go 1.26

replace github.com/emersion/go-imap v1.2.1 => github.com/navrotskyj/go-imap v1.2.2-0.20240927130548-8f6fa2edadb3

require (
	buf.build/gen/go/webitel/webitel-go/grpc/go v1.6.2-20260528093848-00aedb783a79.1
	buf.build/gen/go/webitel/webitel-go/protocolbuffers/go v1.36.12-20260528093848-00aedb783a79.1
	github.com/emersion/go-imap v1.2.1
	github.com/emersion/go-message v0.18.2
	github.com/emersion/go-sasl v0.0.0-20200509203442-7bfe0ed36a21
	github.com/jackc/pgx/v5 v5.10.0
	github.com/k3a/html2text v1.2.1
	github.com/mbobakov/grpc-consul-resolver v1.5.3
	github.com/pressly/goose/v3 v3.27.3
	github.com/spf13/pflag v1.0.10
	github.com/urfave/cli/v2 v2.27.7
	github.com/webitel/crypto/cryptobox v0.1.0
	github.com/webitel/crypto/cryptostore v0.1.0
	github.com/webitel/webitel-go-kit/appconfig v0.0.0-20260803090450-fac61ca3df42
	github.com/webitel/webitel-go-kit/cmd/protoc-gen-go-webitel v0.0.0-20240829153325-0ae7f6059b52
	github.com/webitel/webitel-go-kit/infra/discovery v0.0.0-20260826083159-429f167cb300
	github.com/webitel/webitel-go-kit/infra/otel v0.1.0
	github.com/webitel/webitel-go-kit/infra/pubsub/rabbitmq v0.0.0-20260826083159-429f167cb300
	github.com/webitel/webitel-go-kit/pkg/errors v0.1.0
	github.com/webitel/webitel-go-kit/pkg/interceptors v0.1.1
	go.opentelemetry.io/contrib/bridges/otelslog v0.19.0
	go.opentelemetry.io/otel v1.45.0
	go.opentelemetry.io/otel/sdk v1.45.0
	go.uber.org/fx v1.24.0
	golang.org/x/oauth2 v0.36.0
	golang.org/x/sync v0.22.0
	google.golang.org/genproto/googleapis/api v0.0.0-20260414002931-afd174a4e478
	google.golang.org/grpc v1.82.1
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/armon/go-metrics v0.4.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.7 // indirect
	github.com/fatih/color v1.16.0 // indirect
	github.com/fsnotify/fsnotify v1.9.0 // indirect
	github.com/go-logr/logr v1.4.4 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/go-playground/form v3.1.4+incompatible // indirect
	github.com/go-viper/mapstructure/v2 v2.4.0 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.28.0 // indirect
	github.com/hashicorp/consul/api v1.33.2 // indirect
	github.com/hashicorp/errwrap v1.1.0 // indirect
	github.com/hashicorp/go-cleanhttp v0.5.2 // indirect
	github.com/hashicorp/go-hclog v1.5.0 // indirect
	github.com/hashicorp/go-immutable-radix v1.3.1 // indirect
	github.com/hashicorp/go-multierror v1.1.1 // indirect
	github.com/hashicorp/go-rootcerts v1.0.2 // indirect
	github.com/hashicorp/golang-lru v1.0.2 // indirect
	github.com/hashicorp/serf v0.10.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/jpillora/backoff v1.0.0 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.23 // indirect
	github.com/mfridman/interpolate v0.0.2 // indirect
	github.com/mitchellh/go-homedir v1.1.0 // indirect
	github.com/pelletier/go-toml/v2 v2.2.4 // indirect
	github.com/pkg/errors v0.9.1 // indirect
	github.com/rabbitmq/amqp091-go v1.13.0 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/sagikazarmark/locafero v0.11.0 // indirect
	github.com/sethvargo/go-retry v0.4.0 // indirect
	github.com/sourcegraph/conc v0.3.1-0.20240121214520-5f936abd7ae8 // indirect
	github.com/spf13/afero v1.15.0 // indirect
	github.com/spf13/cast v1.10.0 // indirect
	github.com/spf13/viper v1.21.0 // indirect
	github.com/subosito/gotenv v1.6.0 // indirect
	github.com/webitel/crypto/env v0.1.0 // indirect
	github.com/xrash/smetrics v0.0.0-20240521201337-686a1a2994c1 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.19.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.19.0 // indirect
	go.opentelemetry.io/otel/log v0.20.0 // indirect
	go.opentelemetry.io/otel/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/sdk/log v0.19.0 // indirect
	go.opentelemetry.io/otel/sdk/metric v1.45.0 // indirect
	go.opentelemetry.io/otel/trace v1.45.0 // indirect
	go.opentelemetry.io/proto/otlp v1.10.0 // indirect
	go.uber.org/dig v1.19.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	go.uber.org/zap v1.27.0 // indirect
	go.yaml.in/yaml/v3 v3.0.5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp v0.0.0-20260718201538-764159d718ef // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.40.0 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260720211330-0afa2a65878a // indirect
	gopkg.in/natefinch/lumberjack.v2 v2.2.1 // indirect
)
