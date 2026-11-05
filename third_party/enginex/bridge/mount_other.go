//go:build !linux

package bridge

import "errors"

// Only the deck (Linux) mounts overlays; this lets tools that embed the bridge,
// such as the OpenCDJ-3000 simulator, build for macOS.
func overlayMount(target, opts string) error { return errors.ErrUnsupported }
