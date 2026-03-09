//go:build linux && amd64

package main

import (
	_ "embed"
)

//go:embed interpose/libhook-linux-amd64.gz
var embeddedLib []byte
