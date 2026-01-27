package boot

import (
	"os"
	"syscall"

	"initd/internal/logging"
)

func Reboot() {
	if os.Getpid() != 1 {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"reboot requested but not running as PID 1")
		return
	}

	logging.KernelPrintf(os.Stderr, "initd", 1, "rebooting system")

	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
}

func PowerOff() {
	if os.Getpid() != 1 {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"poweroff requested but not running as PID 1")
		return
	}

	logging.KernelPrintf(os.Stderr, "initd", 1, "powering off system")

	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
}

func Halt() {
	if os.Getpid() != 1 {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"halt requested but not running as PID 1")
		return
	}

	logging.KernelPrintf(os.Stderr, "initd", 1, "system halted")

	syscall.Sync()
	_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_HALT)
}
