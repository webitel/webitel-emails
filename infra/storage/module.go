package storage

import "go.uber.org/fx"

// Module provides the storage client.
var Module = fx.Module("storage", fx.Provide(NewClient))
