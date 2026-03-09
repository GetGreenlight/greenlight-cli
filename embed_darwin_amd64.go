//go:build darwin && amd64

package main

import (
	_ "embed"
)

//go:embed interpose/libhook-darwin-amd64.gz
var embeddedLib []byte
