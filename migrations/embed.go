package migrations

import "embed"

// Files contains versioned SQL migrations shipped with audit-server.
//
//go:embed *.sql
var Files embed.FS
