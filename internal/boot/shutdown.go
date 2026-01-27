package boot

import (
	"os"
	"time"
	"syscall"

	"initd/internal/logging"
	"initd/internal/supervisor"
)

func Shutdown(mgr *supervisor.Manager, mode string) {
	if os.Getpid() != 1 {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
			"shutdown requested but not running as PID 1")
		return
	}

	logging.KernelPrintf(os.Stderr, "initd", 1,
		"system is going down for %s", mode)

	prepareShutdown()

	stopServices(mgr)

	finalizeShutdown(mode)
}

func prepareShutdown() {
	_ = os.WriteFile("/run/nologin",
		[]byte("System is shutting down\n"), 0644)

	logging.KernelPrintf(os.Stderr, "initd", 1,
		"shutdown: login disabled")
}

func stopServices(mgr *supervisor.Manager) {
	logging.KernelPrintf(os.Stderr, "initd", 1,
		"shutdown: stopping services")

	timeout := time.After(10 * time.Second)

	done := make(chan struct{})
	go func() {
		mgr.StopAllUnits()
		close(done)
	}()

	select {
	case <-done:
		logging.KernelPrintf(os.Stderr, "initd", 1,
			"shutdown: all services stopped")
	case <-timeout:
		logging.KernelPrintf(os.Stderr, "initd", 1,
			"shutdown: timeout, forcing exit")
	}
}
func finalizeShutdown(mode string) {
	syscall.Sync()

	switch mode {
	case "reboot":
		Reboot()
	case "poweroff":
		PowerOff()
	case "halt":
		Halt()
	default:
		logging.KernelPrintf(os.Stderr, "initd", 1,
			"unknown shutdown mode: %s", mode)
	}
}
