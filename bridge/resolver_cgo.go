//go:build cgo

package main

// systemResolver reports whether this build can use the operating system's
// resolver (on Android, the phone's own DNS settings).
const systemResolver = true
