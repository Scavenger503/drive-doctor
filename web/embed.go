// Package web holds the embedded front end.
package web

import "embed"

//go:embed index.html favicon.svg samples
var Files embed.FS
