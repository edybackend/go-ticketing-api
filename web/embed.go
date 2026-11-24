// Package web embeds the seat map page (plain HTML and JavaScript, no build step).
package web

import "embed"

// Page holds index.html.
//
//go:embed index.html
var Page embed.FS
