//go:build !unix

package main

import "os/exec"

// setProcessGroup does nothing here; termux-call-log only exists on Android.
func setProcessGroup(*exec.Cmd) {}
