package main

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"initd/internal/boot"
	"initd/internal/ipc"
	"initd/internal/logging"
	"initd/internal/supervisor"
)

const initdVersion = "0.0.2"

func main() {
	socketPath, initMode, err := parseArgs(os.Args[1:])
	if err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "%v", err)
		os.Exit(1)
	}

	signals := make(chan os.Signal, 16)

	if initMode {
		signal.Notify(signals, syscall.SIGTERM, syscall.SIGCHLD)
	} else {
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	}

	manager := supervisor.NewManager()

	if os.Getpid() == 1 {
		reaper := supervisor.NewProcessReaper()
		reaper.Start()
		manager.SetReaper(reaper)
	}

	if err := manager.LoadUnits(); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "failed to load units: %v", err)
	}

	go func() {
		for {
			if err := ipc.Serve(socketPath, manager); err != nil {
				logging.KernelPrintf(os.Stderr, "initd", os.Getpid(),
					"ipc server error: %v (retrying)", err)
				time.Sleep(time.Second)
				continue
			}
		}
	}()

if initMode && os.Getpid() == 1 {
	setupConsole()
	remountRootRW()
	boot.ApplyHostname()

	if err := manager.StartEnabledUnits(); err != nil {
		logging.KernelPrintf(os.Stderr, "initd", 1,
			"failed to start enabled units: %v", err)
	}

	spawnLoginFrontend()

	for {
		select {
		case sig := <-signals:
			switch sig {
			case syscall.SIGTERM:
				logging.KernelPrintf(os.Stderr, "initd", 1,
					"SIGTERM ignored by init")
			case syscall.SIGCHLD:
				// reaper handles
			}
		}
	}
}


	// socket-only OR non-PID1
	<-signals
}

/* ---------------- helpers ---------------- */

func setupConsole() {
	for _, p := range []string{"/dev/console", "/dev/tty1"} {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			continue
		}
		_ = syscall.Dup2(int(f.Fd()), 0)
		_ = syscall.Dup2(int(f.Fd()), 1)
		_ = syscall.Dup2(int(f.Fd()), 2)
		_ = f.Close()
		return
	}
}

func readKernelCmdline() string {
	data, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func parseKernelConsole() string {
	cmdline := readKernelCmdline()
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
	if err := syscall.Dup2(int(tty.Fd()), 0); err != nil {
		tty.Close()
		return fmt.Errorf("dup2 stdin failed: %w", err)
	}
	if err := syscall.Dup2(int(tty.Fd()), 1); err != nil {
		tty.Close()
		return fmt.Errorf("dup2 stdout failed: %w", err)
	}
	if err := syscall.Dup2(int(tty.Fd()), 2); err != nil {
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

func remountRootRW() {
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

/* ---------------- arg parsing ---------------- */

func parseArgs(args []string) (string, bool, error) {
	socketPath := "/run/initd.sock"
	initMode := true
	socketProvided := false

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "-h" || arg == "--help" || arg == "-help":
			printHelp()
			os.Exit(0)
		case arg == "-V" || arg == "--version":
			printVersion()
			os.Exit(0)
		case arg == "--init":
			initMode = true
		case arg == "--socket":
			socketProvided = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				socketPath = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--socket="):
			socketProvided = true
			value := strings.TrimPrefix(arg, "--socket=")
			if value != "" {
				socketPath = value
			}
		case arg == "":
			continue
		default:
			return "", false, fmt.Errorf("unknown argument: %s", arg)
		}
	}

	if socketProvided {
		initMode = false
	}

	return socketPath, initMode, nil
}

func printHelp() {
	fmt.Printf(`Usage: initd [OPTIONS...]

Default behavior:
  Running initd with NO arguments defaults to init/supervisor mode (equivalent to --init).

Options:
  --init               Run as init/supervisor (autostart enabled units).
  --socket[=PATH]      Run as a pure daemon/service manager without init/PID1 behaviors.
                       If PATH omitted, defaults to /run/initd.sock.
  -h, --help           Show this help.
  -V, --version        Show version.

Report bugs to: https://github.com/EdwardLab/initd
`)
}

func printVersion() {
	fmt.Printf(
		"initd (initd) %s by EdwardLab (https://github.com/EdwardLab) MIT License\n",
		initdVersion,
	)
}
