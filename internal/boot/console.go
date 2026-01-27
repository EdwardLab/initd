package boot
import (
	"fmt"
	"os"
	"strings"
	"golang.org/x/sys/unix"
	"syscall"
	"initd/internal/logging"
)

func SetupConsole() {
	for _, p := range []string{"/dev/console", "/dev/tty1"} {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		_ = unix.Dup2(int(f.Fd()), 0)
		_ = unix.Dup2(int(f.Fd()), 1)
		_ = unix.Dup2(int(f.Fd()), 2)
		_ = f.Close()
		return
	}
}

func parseKernelConsole() string {
	cmdline := ReadKernelCmdline()
	var console string

	for _, field := range strings.Fields(cmdline) {
		if strings.HasPrefix(field, "console=") {
			console = strings.TrimPrefix(field, "console=")
		}
	}
	return console
}

func detectConsoleTTY() (devPath string, ttyName string) {
	console := parseKernelConsole()
	if console != "" {
		// strip baudrate if present
		name := strings.SplitN(console, ",", 2)[0]
		return "/dev/" + name, name
	}

	// Fallbacks (PC-first, safe defaults)
	if _, err := os.Stat("/dev/tty1"); err == nil {
		return "/dev/tty1", "tty1"
	}
	if _, err := os.Stat("/dev/ttyS0"); err == nil {
		return "/dev/ttyS0", "ttyS0"
	}

	// Absolute last resort
	return "/dev/console", "console"
}

func isSerialTTY(name string) bool {
	return strings.HasPrefix(name, "ttyS") ||
		strings.HasPrefix(name, "ttyAMA") ||
		strings.HasPrefix(name, "ttymxc") ||
		strings.HasPrefix(name, "hvc")
}

func spawnGetty() error {
	dev, name := detectConsoleTTY()

	tty, err := os.OpenFile(dev, os.O_RDWR, 0)
	if err != nil {
		return fmt.Errorf("open %s failed: %w", dev, err)
	}

	// stdin/stdout/stderr
	if err := unix.Dup2(int(tty.Fd()), 0); err != nil {
		tty.Close()
		return fmt.Errorf("dup2 stdin failed: %w", err)
	}
	if err := unix.Dup2(int(tty.Fd()), 1); err != nil {
		tty.Close()
		return fmt.Errorf("dup2 stdout failed: %w", err)
	}
	if err := unix.Dup2(int(tty.Fd()), 2); err != nil {
		tty.Close()
		return fmt.Errorf("dup2 stderr failed: %w", err)
	}

	tty.Close()

	var cmd []string
	if isSerialTTY(name) {
		cmd = []string{
			"/sbin/agetty",
			"-L",
			"115200",
			name,
			"linux",
		}
	} else {
		cmd = []string{
			"/sbin/agetty",
			"--noclear",
			name,
			"linux",
		}
	}

	attr := &syscall.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(),
		Files: []uintptr{0, 1, 2},
		Sys: &syscall.SysProcAttr{
			Setsid:  true,
			Setctty: true,
			Ctty:    0, // stdin
		},
	}

	pid, err := syscall.ForkExec(cmd[0], cmd, attr)
	if err != nil {
		return err
	}

	logging.KernelPrintf(
		os.Stderr,
		"initd",
		1,
		"spawned getty on %s as pid %d",
		name,
		pid,
	)
	return nil
}


func spawnFallbackShell() {
	cmd := []string{"/bin/sh"}

	attr := &syscall.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(),
		Files: []uintptr{0, 1, 2},
		Sys: &syscall.SysProcAttr{
			Setsid: true,
		},
	}

	_, _ = syscall.ForkExec(cmd[0], cmd, attr)
}

func spawnLoginFrontend() {
	if err := spawnGetty(); err != nil {
		logging.KernelPrintf(
			os.Stderr,
			"initd",
			1,
			"getty failed: %v, falling back to shell",
			err,
		)
		spawnFallbackShell()
	}
}

func SpawnVirtualTerminals() {
    for i := 1; i <= 6; i++ {
        go spawnGettyOnTTY(fmt.Sprintf("tty%d", i))
    }
    activateVT(1)
}


func spawnGettyOnTTY(name string) {
    dev := "/dev/" + name

    tty, err := os.OpenFile(dev, os.O_RDWR, 0)
    if err != nil {
        return
    }

    attr := &syscall.ProcAttr{
        Dir: "/",
        Env: os.Environ(),
        Files: []uintptr{
            tty.Fd(), tty.Fd(), tty.Fd(),
        },
        Sys: &syscall.SysProcAttr{
            Setsid:  true,
            Setctty: true,
            Ctty:    0,
        },
    }

    _, err = syscall.ForkExec(
        "/sbin/agetty",
        []string{"agetty", "--noclear", name, "linux"},
        attr,
    )

    tty.Close()
}


func activateVT(n int) {
    f, err := os.OpenFile("/dev/tty0", os.O_RDWR, 0)
    if err != nil {
        return
    }
    defer f.Close()

    _, _, _ = syscall.Syscall(
        syscall.SYS_IOCTL,
        f.Fd(),
        uintptr(0x5606), // VT_ACTIVATE
        uintptr(n),
    )

    _, _, _ = syscall.Syscall(
        syscall.SYS_IOCTL,
        f.Fd(),
        uintptr(0x5607), // VT_WAITACTIVE
        uintptr(n),
    )
}