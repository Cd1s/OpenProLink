//go:build !linux && !darwin

package cdj

import "syscall"

// ReusePort does nothing here (Windows has no SO_REUSEPORT): the socket gets
// its port to itself, so quit rekordbox and other Pro DJ Link apps first.
func ReusePort(network, address string, c syscall.RawConn) error { return nil }
