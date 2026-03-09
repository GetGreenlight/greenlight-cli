//go:build darwin && arm64

package main

import (
	_ "embed"
)

//go:embed interpose/libhook-darwin-arm64.gz
var embeddedLib []byte
