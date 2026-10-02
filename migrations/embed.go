// Package migrations holds the SQL migrations, embedded in the binary and applied by
// `orion migrate`.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
