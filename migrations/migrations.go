// Package migrations contains SQL migrations embedded into the service binary.
package migrations

import "embed"

// EmbedMigrations makes deployment independent of external SQL files.
//
//go:embed *.sql
var EmbedMigrations embed.FS
