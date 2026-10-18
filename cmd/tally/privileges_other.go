//go:build !linux

package main

import "log/slog"

// dropRoot only does something in the Linux image. See privileges.go.
func dropRoot(*slog.Logger) error { return nil }
