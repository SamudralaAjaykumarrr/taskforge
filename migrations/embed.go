// Package migrations embeds the SQL migration files in this directory so
// internal/migrate can apply them without relying on files being present
// on disk at runtime (a single compiled binary is self-contained).
package migrations

import "embed"

//go:embed *.up.sql *.down.sql
var Files embed.FS
