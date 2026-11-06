//go:build linux || darwin

package cdj

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// ReusePort is a net.ListenConfig Control that lets a socket share its port
// with others (another Pro DJ Link app, or a socket bound to one address).
// SO_REUSEPORT is 0x200 on macOS and 15 on Linux, whose syscall package does
// not export it; x/sys/unix has both.
func ReusePort(network, address string, c syscall.RawConn) error {
	var err error
	if cerr := c.Control(func(fd uintptr) {
		if err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEADDR, 1); err == nil {
			err = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		}
	}); cerr != nil {
		return cerr
	}
	return err
}
