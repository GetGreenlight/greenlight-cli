//go:build linux && arm64

package main

import (
	_ "embed"
)

//go:embed interpose/libhook-linux-arm64.gz
var embeddedLib []byte
