// Package web holds the pages and assets the server sends to the browser. No build step: what
// is here is what ships.
package web

import "embed"

//go:embed index.html login.html
var Pages embed.FS

//go:embed static
var Static embed.FS
