//go:build !linux

package main

import "os/exec"

// setPdeathsig does nothing off Linux, which the benchmark needs anyway.
func setPdeathsig(*exec.Cmd) {}
