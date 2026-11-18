// Package templates holds the text/template files the compiler renders into
// system config files. One template per kind of output file.
package templates

import "embed"

//go:embed *.tmpl
var FS embed.FS
