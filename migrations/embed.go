// Package migrations carries the SQL schema migrations embedded into the
// binary, so a release artefact is a single file with nothing that has to be
// deployed next to it.
package migrations

import "embed"

// FS holds every migration in this directory. The broker applies them at
// startup; see db.Migrate.
//
//go:embed *.sql
var FS embed.FS
