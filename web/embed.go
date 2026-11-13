// Package web holds the embedded single-page UI.
package web

import "embed"

//go:embed index.html logo.svg vendor
var FS embed.FS
