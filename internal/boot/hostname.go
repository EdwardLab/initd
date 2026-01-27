package boot

import (
	"os"
	"strings"
	"syscall"

	"initd/internal/logging"
)

// ApplyHostname applies system hostname during early boot.
// Must be called in PID 1 before spawning getty/login.
func ApplyHostname() {
	if os.Getpid() != 1 {
		return
	}

	var name string

	// 1. /etc/hostname
	if data, err := os.ReadFile("/etc/hostname"); err == nil {
		name = strings.TrimSpace(string(data))
	}

	// 2. kernel cmdline override: hostname=
	if cmd := ReadKernelCmdline(); cmd != "" {
		for _, field := range strings.Fields(cmd) {
			if strings.HasPrefix(field, "hostname=") {
				name = strings.TrimPrefix(field, "hostname=")
			}
		}
	}

	if name == "" {
		logging.KernelPrintf(os.Stderr, "initd", 1,
			"no hostname specified, skipping")
		return
	}

	if err := syscall.Sethostname([]byte(name)); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", 1,
			"failed to set hostname to %q: %v", name, err)
		return
	}

	logging.KernelPrintf(os.Stderr, "initd", 1,
		"hostname set to %q", name)
}
