//go:build !linux

package main

// runSeccompSupervisor is a no-op on non-Linux platforms.
func runSeccompSupervisor(notifFd int, agent string, ir *interposeRelay) {}

// seccompProjectDir is only used on Linux.
var seccompProjectDir string
