package main

import "embed"

//go:embed web/*.html
var webFS embed.FS
