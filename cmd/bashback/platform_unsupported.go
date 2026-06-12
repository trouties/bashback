//go:build !unix

// bashback requires a unix platform: Linux or macOS, or Windows via WSL2.
// This file turns a non-unix build into one readable error instead of
// scattered undefined-symbol failures from the unix-only internals.
package main

var _ = bashback_supports_Linux_and_macOS_only__on_Windows_use_WSL2
