// Package migrations embeds the SQL migration files into the binaries.
//
// Files are named NNNN_description.up.sql / NNNN_description.down.sql and are
// applied in version order by cmd/migrate.
package migrations

import "embed"

// FS holds every .sql file in this directory.
//
//go:embed *.sql
var FS embed.FS
