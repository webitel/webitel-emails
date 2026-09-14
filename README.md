# webitel-emails

Backend service for Webitel email management.

## Local requirements

- Go 1.26;
- PostgreSQL at `127.0.0.1:5432` with the `webitel` database;
- Consul at `127.0.0.1:8500`;
- RabbitMQ at `127.0.0.1:5672`.

The default local connection values are documented in
[`config/config.example.yml`](config/config.example.yml) and
[`.env.example`](.env.example).

## Run locally

Apply database migrations explicitly:

```shell
go run . migrate --config_file=config/config.example.yml
```

Start the service:

```shell
go run . server --config_file=config/config.example.yml
```

The service listens for gRPC connections at `127.0.0.1:8080`, publishes its
health status through the standard gRPC health service, and registers itself in
Consul. Database migrations are never applied automatically when the server
starts.

Configuration can also be supplied through environment variables. For local
development, copy `.env.example` to `.env`, adjust its values, and export them
before running the commands without `--config_file`:

```shell
set -a
source .env
set +a
go run . migrate
go run . server
```

Configuration priority is: command-line flags, environment variables, the
configuration file, then built-in defaults. TLS certificate verification is
enabled by default; the local examples disable it only for development.

Stop the service with `Ctrl+C`. It first enters the draining state, deregisters
from Consul, and then gracefully stops its dependencies.
