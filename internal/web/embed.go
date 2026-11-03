// Package web contains the embedded admin templates and static assets.
package web

import "embed"

// Files contains the admin templates and public assets bundled in the binary.
//
//go:embed templates/*.html assets/*
var Files embed.FS
