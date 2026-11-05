package bridge

import "syscall"

func overlayMount(target, opts string) error {
	return syscall.Mount("overlay", target, "overlay", 0, opts)
}
