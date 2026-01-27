package boot

import (
	"os"
	"syscall"
	"initd/internal/logging"
)

func RemountRootRW() {
	if os.Getpid() != 1 {
		return
	}

	err := syscall.Mount(
		"",   // source ignored for remount
		"/",  // target
		"",   // fs type ignored
		syscall.MS_REMOUNT,
		"",
	)

	if err != nil {
		logging.KernelPrintf(
			os.Stderr,
			"initd",
			1,
			"rootfs remains read-only (%v)",
			err,
		)
		return
	}

	logging.KernelPrintf(
		os.Stderr,
		"initd",
		1,
		"root filesystem remounted read-write",
	)
}