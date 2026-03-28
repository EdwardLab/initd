package service

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"initd/internal/notify"

	"initd/internal/logging"
	"initd/internal/parser"

	"github.com/google/shlex"
)

type State string

const (
	StateInactive   State = "inactive"
	StateActivating State = "activating"
	StateActive     State = "active"
	StateStopping   State = "stopping"
	StateFailed     State = "failed"
)

type Runtime struct {
	State               State
	MainPID             int
	ExitCode            int
	LastError           string
	StartedAt           time.Time
	FinishedAt          time.Time
	StartedAtMonotonic  time.Duration
	FinishedAtMonotonic time.Duration
}

type Unit struct {
	mu             sync.Mutex
	Config         *parser.Unit
	Path           string
	Runtime        Runtime
	Cmd            *exec.Cmd
	Logs           *logging.Buffer
	restartHistory []time.Time
	startToken     int
	reaper         ExitReaper
	notifyServer   *notify.Server
}

type ExitReaper interface {
	Register(pid int, handler func(syscall.WaitStatus))
}

type credentialSpec struct {
	uid uint32
	gid uint32
	groups []uint32
	set bool
}

type commandOptions struct {
	rootOnly bool
}

func NewUnit(config *parser.Unit, path string) *Unit {
	return &Unit{
		Config: config,
		Path:   path,
		Logs:   logging.NewBuffer(200),
		Runtime: Runtime{
			State: StateInactive,
		},
	}
}

func (u *Unit) SetReaper(reaper ExitReaper) {
	u.mu.Lock()
	u.reaper = reaper
	u.mu.Unlock()
}

func (u *Unit) Description() string {
	if u.Config.Description != "" {
		return u.Config.Description
	}
	return u.Config.Name
}

func (u *Unit) Snapshot() Runtime {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Runtime
}

func (u *Unit) Start() (int, error) {
	u.mu.Lock()
	if u.Runtime.State == StateActive || u.Runtime.State == StateActivating || u.Runtime.State == StateStopping {
		token := u.startToken
		u.mu.Unlock()
		return token, nil
	}
	u.startToken++
	token := u.startToken
	u.Runtime.State = StateActivating
	u.Runtime.LastError = ""
	u.Runtime.ExitCode = 0
	u.Runtime.FinishedAt = time.Time{}
	u.Runtime.FinishedAtMonotonic = 0
	u.Runtime.StartedAtMonotonic = 0
	u.Runtime.MainPID = 0
	u.mu.Unlock()

	if err := u.checkConditions(); err != nil {
		u.markFailed(err, false)
		return token, err
	}

	if status, err := u.runExecCondition(); err != nil {
		u.markFailed(err, false)
		return token, err
	} else if status == "skip" {
		u.transitionState(StateInactive, "")
		return token, nil
	}

	execStart := strings.TrimSpace(u.Config.Service.ExecStart)
	if execStart == "" {
		err := errors.New("ExecStart is empty")
		u.markFailed(err, false)
		return token, err
	}

	if err := u.ensureManagedDirectories(); err != nil {
		u.markFailed(err, false)
		return token, err
	}

	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		u.markFailed(err, false)
		return token, err
	}

	execStart, ignoreFailure := stripPrefix(execStart)
	expandedStart := expandWithEnv(execStart, envMap)
	args, err := shlex.Split(expandedStart)
	if err != nil {
		u.markFailed(err, ignoreFailure)
		if ignoreFailure {
			return token, nil
		}
		return token, fmt.Errorf("parse ExecStart: %w", err)
	}
	if len(args) == 0 {
		err := errors.New("ExecStart parsed to empty")
		u.markFailed(err, ignoreFailure)
		if ignoreFailure {
			return token, nil
		}
		return token, err
	}

	go u.runStartSequence(token, args, envMap, envList, ignoreFailure)
	return token, nil
}

func (u *Unit) runStartSequence(token int, args []string, envMap map[string]string, envList []string, ignoreFailure bool) {
	if !u.isCurrentToken(token) {
		return
	}

	if err := u.runExecStartPre(token, envMap, envList); err != nil {
		u.markFailed(err, false)
		return
	}
	if !u.isCurrentToken(token) {
		return
	}

	serviceType := u.canonicalServiceType()

	cmd, err := u.buildExecCommand(args, commandOptions{})
	if err != nil {
		u.markFailed(err, ignoreFailure)
		return
	}

	// -------------------------------------------------
	// Proper notify socket creation
	// -------------------------------------------------
	if serviceType == "notify" {
		server, err := notify.Start()
		if err != nil {
			u.markFailed(fmt.Errorf("notify socket create failed: %w", err), ignoreFailure)
			return
		}

		envList = append(envList, "NOTIFY_SOCKET="+server.Path)

		u.mu.Lock()
		u.notifyServer = server
		u.mu.Unlock()
	}

	stdoutLogger := &logging.LineLogger{
		Unit:   u.Config.Name,
		PID:    0,
		Level:  logging.LevelInfo,
		Buffer: u.Logs,
		Output: os.Stdout,
	}
	stderrLogger := &logging.LineLogger{
		Unit:   u.Config.Name,
		PID:    0,
		Level:  logging.LevelError,
		Buffer: u.Logs,
		Output: os.Stderr,
	}

	u.configureCommand(cmd, envList, stdoutLogger, stderrLogger)

	if err := cmd.Start(); err != nil {
		u.markFailed(err, ignoreFailure)

		u.mu.Lock()
		if u.notifyServer != nil {
			u.notifyServer.Stop()
			u.notifyServer = nil
		}
		u.mu.Unlock()

		return
	}

	u.mu.Lock()

	if u.startToken != token {
		u.mu.Unlock()
		_ = cmd.Process.Kill()
		return
	}

	u.Cmd = cmd
	u.Runtime.StartedAt = time.Now()
	u.Runtime.StartedAtMonotonic = logging.MonotonicNow()
	stdoutLogger.PID = cmd.Process.Pid
	stderrLogger.PID = cmd.Process.Pid

	if serviceType == "simple" {
		u.Runtime.State = StateActive
		u.Runtime.MainPID = cmd.Process.Pid
	}

	u.mu.Unlock()

	// -------------------------------------------------
	// Register reaper first (avoid race)
	// -------------------------------------------------
	if u.reaper != nil {
		resetActive := serviceType == "simple" || serviceType == "notify"
		pid := cmd.Process.Pid

		u.reaper.Register(pid, func(status syscall.WaitStatus) {
			if serviceType == "oneshot" {
				exitCode := commandExitStatus(status)
				if exitCode == 0 {
					if err := u.runExecStartPost(token, envMap, envList); err != nil {
						u.markFailed(err, ignoreFailure)
						return
					}
				}
			}
			u.handleExitStatusForPID(token, pid, status, ignoreFailure, resetActive)
		})
	}

	// -------------------------------------------------
	// Dispatch wait handlers
	// -------------------------------------------------
	switch serviceType {
	case "oneshot":
		go u.waitOneshot(token, envMap, envList, ignoreFailure)
	case "forking":
		go u.waitForking(token, envMap, envList, ignoreFailure)
	case "notify":
		go u.waitNotify(token, envMap, envList, ignoreFailure)
	default:
		go u.waitSimple(token, envMap, envList, ignoreFailure)
	}
}


func (u *Unit) waitSimple(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	if err := u.runExecStartPost(token, envMap, envList); err != nil {
		u.killMainProcess(syscall.SIGTERM)
		u.markFailed(err, ignoreFailure)
		return
	}
	if u.reaper != nil {
		return
	}
	err := u.Cmd.Wait()
	u.handleExit(token, err, ignoreFailure, true)
}

func (u *Unit) waitOneshot(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	if u.reaper != nil {
		return
	}
	err := u.Cmd.Wait()
	if err != nil {
		u.handleExit(token, err, ignoreFailure, false)
		return
	}
	if err := u.runExecStartPost(token, envMap, envList); err != nil {
		u.markFailed(err, ignoreFailure)
		return
	}
	u.handleExit(token, nil, ignoreFailure, false)
}

func (u *Unit) waitForking(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	startedAt := time.Now()
	startedAtMonotonic := logging.MonotonicNow()
	timeout := u.StartTimeout()
	poll := 100 * time.Millisecond

	// systemd waits for the PIDFile to appear for Type=forking; without cgroups
	// we treat the PIDFile PID as the main process once it shows up.
	pid, err := u.waitForPIDFile(timeout, poll)

	if err != nil {
		u.markFailed(err, ignoreFailure)
		return
	}

	u.mu.Lock()
	if u.startToken != token || u.Runtime.State == StateFailed {
		u.mu.Unlock()
		return
	}
	u.Runtime.State = StateActive
	u.Runtime.MainPID = pid
	u.Runtime.StartedAt = startedAt
	u.Runtime.StartedAtMonotonic = startedAtMonotonic
	u.mu.Unlock()
	if err := u.runExecStartPost(token, envMap, envList); err != nil {
		if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
		u.markFailed(err, ignoreFailure)
		return
	}
	if u.reaper != nil {
		return
	}

	err = u.Cmd.Wait()
	if err != nil {
		u.mu.Lock()
		shouldHandle := u.startToken == token && u.Runtime.MainPID == 0 && u.Runtime.State != StateActive
		u.mu.Unlock()
		if shouldHandle {
			u.handleExit(token, err, ignoreFailure, false)
		}
	}
}

func (u *Unit) Stop(timeout time.Duration) error {
	runStopPost := true
	defer func() {
		u.mu.Lock()
		if u.notifyServer != nil {
			u.notifyServer.Stop()
			u.notifyServer = nil
		}
		u.mu.Unlock()
		if runStopPost {
			_ = u.runExecStopPost()
		}
	}()

	stopCommand := strings.TrimSpace(u.Config.Service.ExecStop)
	if stopCommand != "" {
		if err := u.runStopCommand(stopCommand); err != nil {
			return err
		}
	}

	serviceType := u.canonicalServiceType()
	killProcessGroup := !u.killModeProcess()

	u.mu.Lock()
	if u.Runtime.State == StateInactive {
		runStopPost = false
		u.mu.Unlock()
		return nil
	}
	u.Runtime.State = StateStopping
	pid := u.Runtime.MainPID
	cmd := u.Cmd
	u.mu.Unlock()

	// -----------------------------
	// Special handling for notify
	// -----------------------------
	if serviceType == "notify" {
		if pid != 0 {
			// Respect KillMode=process
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}

		if u.reaper == nil && cmd != nil {
			done := make(chan error, 1)
			go func() {
				done <- cmd.Wait()
			}()

			if timeout > 0 {
				select {
				case <-done:
				case <-time.After(timeout):
					if pid != 0 {
						_ = syscall.Kill(pid, syscall.SIGKILL)
					}
					<-done
				}
			} else {
				<-done
			}
		}

		u.transitionState(StateInactive, "")
		return nil
	}

	// -----------------------------
	// Forking handling
	// -----------------------------
	if serviceType == "forking" {
		if pid == 0 {
			if mainPID, err := u.readPIDFile(); err == nil {
				pid = mainPID
			}
		}
		if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGTERM)
		}
	} else {
		// simple / others
		if pid != 0 {
			if killProcessGroup {
				_ = syscall.Kill(-pid, syscall.SIGTERM)
			} else {
				_ = syscall.Kill(pid, syscall.SIGTERM)
			}
		}
	}

	waitUntilStopped := func() bool {
		if serviceType == "forking" {
			if pid == 0 || !processAlive(pid) {
				u.transitionState(StateInactive, "")
				return true
			}
		} else {
			state := u.Snapshot().State
			if state == StateInactive || state == StateFailed {
				return true
			}
		}
		time.Sleep(200 * time.Millisecond)
		return false
	}

	if timeout <= 0 {
		for {
			if waitUntilStopped() {
				return nil
			}
		}
	}

	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if waitUntilStopped() {
			return nil
		}
	}

	// Timeout escalation
	if serviceType == "forking" {
		if pid != 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	} else {
		if pid != 0 {
			if killProcessGroup {
				_ = syscall.Kill(-pid, syscall.SIGKILL)
			} else {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	}

	u.transitionState(StateFailed, "terminated after timeout")
	return errors.New("stop timeout")
}


func (u *Unit) Restart(timeout time.Duration) error {
	if err := u.Stop(timeout); err != nil {
		return err
	}
	_, err := u.Start()
	return err
}

func (u *Unit) Reload() error {
	u.mu.Lock()
	active := u.Runtime.State == StateActive
	u.mu.Unlock()
	if !active {
		return errors.New("unit is not active")
	}
	if len(u.Config.Service.ExecReload) == 0 {
		return errors.New("ExecReload not set")
	}

	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}
	for _, command := range u.Config.Service.ExecReload {
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runStopCommand(command string) error {
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}

	return u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.Config.Service.PermissionsStartOnly})
}

func (u *Unit) buildEnvironment() (map[string]string, []string, error) {
	envMap := map[string]string{}
	for _, pair := range os.Environ() {
		if key, value, ok := strings.Cut(pair, "="); ok {
			envMap[key] = value
		}
	}

	for _, entry := range u.Config.Service.EnvironmentFile {
		if err := u.loadEnvironmentFile(entry, envMap); err != nil {
			return nil, nil, err
		}
	}

	for _, entry := range u.Config.Service.Environment {
		parts, err := shlex.Split(entry)
		if err != nil {
			return nil, nil, fmt.Errorf("parse Environment: %w", err)
		}
		for _, assignment := range parts {
			if key, value, ok := strings.Cut(assignment, "="); ok {
				envMap[key] = value
			}
		}
	}

	envList := make([]string, 0, len(envMap))
	for key, value := range envMap {
		envList = append(envList, fmt.Sprintf("%s=%s", key, value))
	}
	return envMap, envList, nil
}

func (u *Unit) loadEnvironmentFile(entry string, envMap map[string]string) error {
	paths, err := shlex.Split(entry)
	if err != nil {
		return fmt.Errorf("parse EnvironmentFile: %w", err)
	}
	for _, path := range paths {
		optional := strings.HasPrefix(path, "-")
		if optional {
			path = strings.TrimPrefix(path, "-")
		}
		file, err := os.Open(path)
		if err != nil {
			if optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return err
		}
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			key, value, ok := strings.Cut(line, "=")
			if !ok {
				continue
			}
			value = strings.TrimSpace(value)
			if unquoted, err := strconv.Unquote(value); err == nil {
				value = unquoted
			}
			envMap[strings.TrimSpace(key)] = value
		}
		if err := scanner.Err(); err != nil {
			_ = file.Close()
			return err
		}
		_ = file.Close()
	}
	return nil
}

func (u *Unit) waitForPIDFile(timeout time.Duration, poll time.Duration) (int, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		pid, err := u.readPIDFile()
		if err == nil && pid > 0 && processAlive(pid) {
			return pid, nil
		}
		time.Sleep(poll)
	}
	return 0, errors.New("PIDFile not found or process not running")
}

func (u *Unit) readPIDFile() (int, error) {
	path := strings.TrimSpace(u.Config.Service.PIDFile)
	if path == "" {
		return 0, errors.New("PIDFile not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pidStr := strings.TrimSpace(string(data))
	return strconv.Atoi(pidStr)
}

func (u *Unit) handleExit(token int, err error, ignoreFailure bool, resetActive bool) {
	exitCode := 0
	if err != nil {
		exitCode = 1
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				exitCode = status.ExitStatus()
			}
		}
	}
	u.handleExitCode(token, 0, exitCode, err, ignoreFailure, resetActive)
}

func (u *Unit) handleExitStatus(token int, status syscall.WaitStatus, ignoreFailure bool, resetActive bool) {
	u.handleExitStatusForPID(token, 0, status, ignoreFailure, resetActive)
}

func (u *Unit) handleExitStatusForPID(token int, watchedPID int, status syscall.WaitStatus, ignoreFailure bool, resetActive bool) {
	exitCode := 0
	var err error
	switch {
	case status.Exited():
		exitCode = status.ExitStatus()
		if exitCode != 0 {
			err = fmt.Errorf("exit status %d", exitCode)
		}
	case status.Signaled():
		exitCode = 128 + int(status.Signal())
		err = fmt.Errorf("terminated by signal %s", status.Signal())
	default:
		err = fmt.Errorf("process exited")
		exitCode = 1
	}
	u.handleExitCode(token, watchedPID, exitCode, err, ignoreFailure, resetActive)
}

func (u *Unit) handleExitCode(token int, watchedPID int, exitCode int, err error, ignoreFailure bool, resetActive bool) {
	serviceType := u.canonicalServiceType()
	if serviceType == "notify" && watchedPID != 0 {
		adoptTimeout := u.StartTimeout()
		if adoptTimeout <= 0 {
			adoptTimeout = 30 * time.Second
		}
		if adoptedPID := u.waitForNotifyMainPID(watchedPID, adoptTimeout, 50*time.Millisecond); adoptedPID != 0 && adoptedPID != watchedPID {
			u.mu.Lock()
			if u.startToken == token {
				if u.notifyServer != nil {
					u.notifyServer.Stop()
					u.notifyServer = nil
				}
				u.Runtime.State = StateActive
				u.Runtime.MainPID = adoptedPID
				u.Runtime.ExitCode = 0
				u.Runtime.LastError = ""
			}
			u.mu.Unlock()
			return
		}
	}

	u.mu.Lock()
	defer u.mu.Unlock()

	if u.startToken != token {
		return
	}

	if serviceType == "notify" && watchedPID != 0 {
		if adoptedPID := u.adoptedNotifyPIDWithCurrent(watchedPID, u.Runtime.MainPID); adoptedPID != 0 && adoptedPID != watchedPID {
			if u.notifyServer != nil {
				u.notifyServer.Stop()
				u.notifyServer = nil
			}
			u.Runtime.State = StateActive
			u.Runtime.MainPID = adoptedPID
			u.Runtime.ExitCode = 0
			u.Runtime.LastError = ""
			return
		}
		if currentPID := u.Runtime.MainPID; currentPID != 0 && currentPID != watchedPID && processAlive(currentPID) {
			if u.notifyServer != nil {
				u.notifyServer.Stop()
				u.notifyServer = nil
			}
			u.Runtime.State = StateActive
			u.Runtime.ExitCode = 0
			u.Runtime.LastError = ""
			return
		}
	}

	// Respect RestartPreventExitStatus
	if prevent := u.RestartPreventExitStatus(); prevent != nil {
		if _, blocked := prevent[exitCode]; blocked {
			return
		}
	}
	if success := u.SuccessExitStatus(); success != nil {
		if _, ok := success[exitCode]; ok {
			err = nil
			exitCode = 0
		}
	}

	// Cleanup notify socket
	if u.notifyServer != nil {
		u.notifyServer.Stop()
		u.notifyServer = nil
	}

	if u.Runtime.State == StateActive && resetActive {
		u.Runtime.MainPID = 0
	}

	if err != nil {
		if u.Runtime.State == StateActive && resetActive {
			u.Runtime.State = StateInactive
		} else if u.Runtime.State != StateActive {
			u.Runtime.State = StateFailed
		}
		u.Runtime.LastError = err.Error()
		u.Runtime.ExitCode = exitCode
	} else {
		if u.Runtime.State != StateActive {
			u.Runtime.State = StateInactive
		}
		u.Runtime.ExitCode = exitCode
	}

	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()

	if ignoreFailure {
		if u.Runtime.State != StateActive {
			u.Runtime.State = StateInactive
			u.Runtime.LastError = ""
		}
	}
}


func (u *Unit) Log(level logging.Level, message string) {
	u.mu.Lock()
	pid := u.Runtime.MainPID
	u.mu.Unlock()
	u.Logs.Add(logging.Entry{
		Timestamp: logging.MonotonicNow(),
		Unit:      u.Config.Name,
		PID:       pid,
		Level:     level,
		Message:   message,
	})
}

func (u *Unit) RecordRestart(now time.Time, interval time.Duration) int {
	u.mu.Lock()
	defer u.mu.Unlock()
	cutoff := now.Add(-interval)
	pruned := u.restartHistory[:0]
	for _, stamp := range u.restartHistory {
		if stamp.After(cutoff) {
			pruned = append(pruned, stamp)
		}
	}
	u.restartHistory = append(pruned, now)
	return len(u.restartHistory)
}

func (u *Unit) MarkFailed(reason string) {
	u.mu.Lock()
	current := u.Runtime.LastError
	u.mu.Unlock()
	if current != "" && current != reason {
		u.transitionState(StateFailed, reason+": "+current)
		return
	}
	u.transitionState(StateFailed, reason)
}

func expandWithEnv(input string, envMap map[string]string) string {
	return os.Expand(input, func(key string) string {
		if value, ok := envMap[key]; ok {
			return value
		}
		return ""
	})
}

func stripPrefix(command string) (string, bool) {
	ignoreFailure := false
	if strings.HasPrefix(command, "-") {
		ignoreFailure = true
		command = strings.TrimSpace(strings.TrimPrefix(command, "-"))
	}
	return command, ignoreFailure
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil
}

func (u *Unit) canonicalServiceType() string {
	serviceType := strings.ToLower(strings.TrimSpace(u.Config.Service.Type))

	switch serviceType {
	case "", "simple":
		return "simple"

	case "forking", "oneshot", "idle", "exec":
		return serviceType

	case "notify", "notify-reload":
		return "notify"

	case "dbus":
		u.Log(logging.LevelInfo, "DBus type treated as simple")
		return "simple"

	default:
		u.Log(logging.LevelError, fmt.Sprintf("Unsupported service type %q; treating as simple", serviceType))
		return "simple"
	}
}

func (u *Unit) transitionState(next State, reason string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.Runtime.State == StateActive && next == StateInactive {
		u.Runtime.MainPID = 0
	}
	u.Runtime.State = next
	if reason != "" {
		u.Runtime.LastError = reason
	}
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
}

func (u *Unit) markFailed(err error, ignoreFailure bool) {
	if ignoreFailure {
		u.transitionState(StateInactive, "")
		return
	}
	u.mu.Lock()
	u.Runtime.State = StateFailed
	u.Runtime.LastError = err.Error()
	u.Runtime.ExitCode = 1
	u.Runtime.MainPID = 0
	u.Runtime.FinishedAt = time.Now()
	u.Runtime.FinishedAtMonotonic = logging.MonotonicNow()
	u.mu.Unlock()
}

func (u *Unit) ensureRuntimeDirectory() error {
	return u.ensureNamedDirectories("/run", u.Config.Service.RuntimeDirectory, u.Config.Service.RuntimeDirectoryMode)
}

func (u *Unit) ensureManagedDirectories() error {
	if err := u.ensureRuntimeDirectory(); err != nil {
		return err
	}
	if err := u.ensureNamedDirectories("/var/lib", u.Config.Service.StateDirectory, "0755"); err != nil {
		return err
	}
	if err := u.ensureNamedDirectories("/var/cache", u.Config.Service.CacheDirectory, "0755"); err != nil {
		return err
	}
	if err := u.ensureNamedDirectories("/var/log", u.Config.Service.LogsDirectory, "0755"); err != nil {
		return err
	}
	if err := u.ensureNamedDirectories("/etc", u.Config.Service.ConfigurationDirectory, "0755"); err != nil {
		return err
	}
	return nil
}

func (u *Unit) ensureNamedDirectories(base string, names []string, modeStr string) error {
	if len(names) == 0 {
		return nil
	}
	mode, err := parseFileMode(modeStr, 0o755)
	if err != nil {
		return err
	}
	creds, err := u.resolveCredentialsForStart(false)
	if err != nil {
		return err
	}
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		path := filepath.Join(base, name)
		if err := os.MkdirAll(path, mode); err != nil {
			return fmt.Errorf("create directory %s: %w", path, err)
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
		if creds.set {
			if err := os.Chown(path, int(creds.uid), int(creds.gid)); err != nil {
				return fmt.Errorf("chown directory %s: %w", path, err)
			}
		}
	}
	return nil
}

func (u *Unit) runExecStartPre(token int, envMap map[string]string, envList []string) error {
	for _, command := range u.Config.Service.ExecStartPre {
		if !u.isCurrentToken(token) {
			return nil
		}
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.Config.Service.PermissionsStartOnly}); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runExecCondition() (string, error) {
	if len(u.Config.Service.ExecCondition) == 0 {
		return "continue", nil
	}
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return "", err
	}
	for _, command := range u.Config.Service.ExecCondition {
		if strings.TrimSpace(command) == "" {
			continue
		}
		status, err := u.runCommandStatus(command, envMap, envList, commandOptions{rootOnly: u.Config.Service.PermissionsStartOnly})
		if err != nil {
			return "", err
		}
		switch {
		case status == 0:
			continue
		case status >= 1 && status <= 254:
			return "skip", nil
		default:
			return "", fmt.Errorf("ExecCondition failed with status %d", status)
		}
	}
	return "continue", nil
}

func (u *Unit) runExecStartPost(token int, envMap map[string]string, envList []string) error {
	for _, command := range u.Config.Service.ExecStartPost {
		if !u.isCurrentToken(token) {
			return nil
		}
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.Config.Service.PermissionsStartOnly}); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runExecStopPost() error {
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}
	for _, command := range u.Config.Service.ExecStopPost {
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList, commandOptions{rootOnly: u.Config.Service.PermissionsStartOnly}); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runCommand(command string, envMap map[string]string, envList []string, opts commandOptions) error {
	status, err := u.runCommandStatus(command, envMap, envList, opts)
	command, ignoreFailure := stripPrefix(command)
	if err != nil {
		if ignoreFailure {
			return nil
		}
		return err
	}
	if status != 0 {
		if ignoreFailure {
			return nil
		}
		return fmt.Errorf("exit status %d", status)
	}
	return nil
}

func (u *Unit) runCommandStatus(command string, envMap map[string]string, envList []string, opts commandOptions) (int, error) {
	command, ignoreFailure := stripPrefix(command)
	expanded := expandWithEnv(command, envMap)
	args, err := shlex.Split(expanded)
	if err != nil {
		if ignoreFailure {
			return 0, nil
		}
		return 0, fmt.Errorf("parse command: %w", err)
	}
	if len(args) == 0 {
		return 0, nil
	}
	cmd, err := u.buildExecCommand(args, opts)
	if err != nil {
		if ignoreFailure {
			return 0, nil
		}
		return 0, err
	}
	stdoutLogger := &logging.LineLogger{Unit: u.Config.Name, PID: 0, Level: logging.LevelInfo, Buffer: u.Logs, Output: os.Stdout}
	stderrLogger := &logging.LineLogger{Unit: u.Config.Name, PID: 0, Level: logging.LevelError, Buffer: u.Logs, Output: os.Stderr}
	u.configureCommand(cmd, envList, stdoutLogger, stderrLogger)
	if err := cmd.Start(); err != nil {
		if ignoreFailure {
			return 0, nil
		}
		return 0, err
	}
	stdoutLogger.PID = cmd.Process.Pid
	stderrLogger.PID = cmd.Process.Pid
	if u.reaper != nil {
		done := make(chan syscall.WaitStatus, 1)
		u.reaper.Register(cmd.Process.Pid, func(status syscall.WaitStatus) {
			done <- status
		})
		status := <-done
		return commandExitStatus(status), nil
	}
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				return commandExitStatus(status), nil
			}
		}
		if ignoreFailure {
			return 0, nil
		}
		return 0, err
	}
	return 0, nil
}

func (u *Unit) buildExecCommand(args []string, opts commandOptions) (*exec.Cmd, error) {
	if len(args) == 0 {
		return nil, errors.New("command parsed to empty")
	}
	umask := strings.TrimSpace(u.Config.Service.UMask)
	limitNOFILE := strings.TrimSpace(u.Config.Service.LimitNOFILE)
	var cmd *exec.Cmd
	if umask != "" || limitNOFILE != "" {
		setup := make([]string, 0, 2)
		if umask != "" {
			setup = append(setup, fmt.Sprintf("umask %s", umask))
		}
		if limitNOFILE != "" {
			setup = append(setup, fmt.Sprintf("ulimit -n %s >/dev/null 2>&1 || true", limitNOFILE))
		}
		setup = append(setup, `exec "$@"`)
		shellArgs := []string{"-c", strings.Join(setup, "; "), "_"}
		shellArgs = append(shellArgs, args...)
		cmd = exec.Command("/bin/sh", shellArgs...)
	} else {
		cmd = exec.Command(args[0], args[1:]...)
	}

	creds, err := u.resolveCredentialsForStart(opts.rootOnly)
	if err != nil {
		return nil, err
	}
	sysProcAttr := &syscall.SysProcAttr{Setpgid: true}
	if creds.set {
		sysProcAttr.Credential = &syscall.Credential{Uid: creds.uid, Gid: creds.gid, Groups: creds.groups}
	}
	if rootDir := strings.TrimSpace(u.Config.Service.RootDirectory); rootDir != "" {
		sysProcAttr.Chroot = rootDir
	}
	cmd.SysProcAttr = sysProcAttr
	cmd.Dir = u.workingDirectory()
	return cmd, nil
}

func (u *Unit) configureCommand(cmd *exec.Cmd, envList []string, stdoutLogger, stderrLogger *logging.LineLogger) {
	cmd.Env = mergeEnvList(envList, map[string]string{
		"MAINPID": strconv.Itoa(u.mainPID()),
	})
	cmd.Stdout = stdoutLogger
	cmd.Stderr = stderrLogger
}

func (u *Unit) checkConditions() error {
	for _, condition := range u.Config.ConditionPathExists {
		condition = strings.TrimSpace(condition)
		if condition == "" {
			continue
		}
		negated := strings.HasPrefix(condition, "!")
		path := strings.TrimPrefix(condition, "!")
		_, err := os.Stat(path)
		exists := err == nil
		if negated {
			exists = !exists
		}
		if !exists {
			return fmt.Errorf("ConditionPathExists=%s failed", condition)
		}
	}
	return nil
}

func (u *Unit) killModeProcess() bool {
	return strings.EqualFold(strings.TrimSpace(u.Config.Service.KillMode), "process")
}

func (u *Unit) killMainProcess(sig syscall.Signal) {
	u.mu.Lock()
	cmd := u.Cmd
	killProcessGroup := !u.killModeProcess()
	u.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return
	}

	pid := cmd.Process.Pid

	if killProcessGroup {
		// Kill entire process group
		_ = syscall.Kill(-pid, sig)
	} else {
		// Kill only main process
		_ = syscall.Kill(pid, sig)
	}
}

func (u *Unit) mainPID() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.Runtime.MainPID
}

func (u *Unit) isCurrentToken(token int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.startToken == token
}

func (u *Unit) IsCurrentToken(token int) bool {
	return u.isCurrentToken(token)
}

func (u *Unit) RestartPreventExitStatus() map[int]struct{} {
	return parseExitStatusSet(u.Config.Service.RestartPreventExitStatus)
}

func (u *Unit) StopTimeout() time.Duration {
	raw := strings.TrimSpace(u.Config.Service.TimeoutStopSec)
	if raw == "" {
		raw = strings.TrimSpace(u.Config.Service.TimeoutSec)
	}
	return parseSystemdDuration(raw, 10*time.Second)
}

func (u *Unit) StartTimeout() time.Duration {
	raw := strings.TrimSpace(u.Config.Service.TimeoutStartSec)
	if raw == "" {
		raw = strings.TrimSpace(u.Config.Service.TimeoutSec)
	}
	return parseSystemdDuration(raw, 30*time.Second)
}

func waitStatusError(status syscall.WaitStatus) error {
	switch {
	case status.Exited():
		if status.ExitStatus() != 0 {
			return fmt.Errorf("exit status %d", status.ExitStatus())
		}
		return nil
	case status.Signaled():
		return fmt.Errorf("terminated by signal %s", status.Signal())
	default:
		return fmt.Errorf("process exited")
	}
}

func commandExitStatus(status syscall.WaitStatus) int {
	switch {
	case status.Exited():
		return status.ExitStatus()
	case status.Signaled():
		return 128 + int(status.Signal())
	default:
		return 1
	}
}

func (u *Unit) livePIDFilePID() int {
	pid, err := u.readPIDFile()
	if err != nil || pid <= 0 || !processAlive(pid) {
		return 0
	}
	return pid
}

func (u *Unit) waitForLivePIDFile(timeout time.Duration, poll time.Duration) int {
	if strings.TrimSpace(u.Config.Service.PIDFile) == "" {
		return 0
	}
	deadline := time.Now().Add(timeout)
	for {
		if pid := u.livePIDFilePID(); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(poll)
	}
}

func (u *Unit) processGroupMemberPID(pgid int, exclude int) int {
	if pgid <= 0 {
		return 0
	}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid <= 0 || pid == exclude {
			continue
		}
		memberPGID, err := syscall.Getpgid(pid)
		if err != nil || memberPGID != pgid || !processAlive(pid) {
			continue
		}
		return pid
	}
	return 0
}

func (u *Unit) adoptedNotifyPIDWithCurrent(watchedPID int, currentPID int) int {
	if pid := u.livePIDFilePID(); pid != 0 && pid != watchedPID {
		return pid
	}
	if currentPID != 0 && currentPID != watchedPID && processAlive(currentPID) {
		return currentPID
	}
	if memberPID := u.processGroupMemberPID(watchedPID, watchedPID); memberPID != 0 {
		return memberPID
	}
	return 0
}

func (u *Unit) adoptedNotifyPID(watchedPID int) int {
	return u.adoptedNotifyPIDWithCurrent(watchedPID, u.mainPID())
}

func (u *Unit) waitForNotifyMainPID(watchedPID int, timeout time.Duration, poll time.Duration) int {
	deadline := time.Now().Add(timeout)
	for {
		if pid := u.adoptedNotifyPID(watchedPID); pid != 0 {
			return pid
		}
		if time.Now().After(deadline) {
			return 0
		}
		time.Sleep(poll)
	}
}

func (u *Unit) notifyMainPID(cmd *exec.Cmd) int {
	if pid := u.waitForLivePIDFile(500*time.Millisecond, 25*time.Millisecond); pid != 0 {
		return pid
	}
	if cmd != nil && cmd.Process != nil {
		if pid := u.processGroupMemberPID(cmd.Process.Pid, cmd.Process.Pid); pid != 0 {
			return pid
		}
	}
	if cmd != nil && cmd.Process != nil {
		return cmd.Process.Pid
	}
	return 0
}

func (u *Unit) waitNotify(token int, envMap map[string]string, envList []string, ignoreFailure bool) {
	u.mu.Lock()
	server := u.notifyServer
	cmd := u.Cmd
	u.mu.Unlock()

	if server == nil {
		u.markFailed(fmt.Errorf("notify server missing"), ignoreFailure)
		return
	}

	timer := time.NewTimer(u.StartTimeout())
	defer timer.Stop()

	select {

	case <-server.Ready:
		pid := u.notifyMainPID(cmd)
		u.mu.Lock()
		if u.startToken == token {
			u.Runtime.State = StateActive
			if pid != 0 {
				u.Runtime.MainPID = pid
			}
		}
		u.mu.Unlock()
		if err := u.runExecStartPost(token, envMap, envList); err != nil {
			u.killMainProcess(syscall.SIGTERM)
			u.markFailed(err, ignoreFailure)
			return
		}

		return

	case <-timer.C:
		u.mu.Lock()
		if u.startToken != token {
			u.mu.Unlock()
			return
		}
		cmd = u.Cmd
		pid := u.Runtime.MainPID
		if pid == 0 {
			pid = u.notifyMainPID(cmd)
		}
		u.mu.Unlock()

		if pid != 0 && processAlive(pid) {
			u.mu.Lock()
			if u.startToken == token {
				u.Runtime.State = StateActive
				u.Runtime.MainPID = pid
			}
			u.mu.Unlock()
			u.Log(logging.LevelInfo, "Type=notify readiness timed out; falling back to active because process is still running")
			return
		}

		if cmd != nil && cmd.Process != nil {
			u.killMainProcess(syscall.SIGTERM)
		}

		u.markFailed(fmt.Errorf("notify timeout"), ignoreFailure)
		return
	}
}

func mergeEnvList(envList []string, extra map[string]string) []string {
	envMap := make(map[string]string, len(envList)+len(extra))
	for _, pair := range envList {
		if key, value, ok := strings.Cut(pair, "="); ok {
			envMap[key] = value
		}
	}
	for key, value := range extra {
		if key == "" {
			continue
		}
		envMap[key] = value
	}
	merged := make([]string, 0, len(envMap))
	for key, value := range envMap {
		merged = append(merged, fmt.Sprintf("%s=%s", key, value))
	}
	return merged
}

func (u *Unit) resolveCredentialsForStart(rootOnly bool) (credentialSpec, error) {
	if rootOnly {
		return credentialSpec{}, nil
	}
	userName := strings.TrimSpace(u.Config.Service.User)
	groupName := strings.TrimSpace(u.Config.Service.Group)
	if userName == "" && groupName == "" {
		return credentialSpec{}, nil
	}

	var (
		uid uint32
		gid uint32
		err error
	)

	if userName != "" {
		uid, gid, err = lookupUser(userName)
		if err != nil {
			return credentialSpec{}, err
		}
	}

	if groupName != "" {
		gid, err = lookupGroup(groupName)
		if err != nil {
			return credentialSpec{}, err
		}
	} else if userName == "" {
		gid = uint32(os.Getgid())
	}

	if userName == "" {
		uid = uint32(os.Getuid())
	}

	groups, err := lookupSupplementaryGroups(u.Config.Service.SupplementaryGroups)
	if err != nil {
		return credentialSpec{}, err
	}

	return credentialSpec{uid: uid, gid: gid, groups: groups, set: true}, nil
}

func (u *Unit) workingDirectory() string {
	if dir := strings.TrimSpace(u.Config.Service.WorkingDirectory); dir != "" {
		return dir
	}
	return filepath.Dir(u.Path)
}

func (u *Unit) SuccessExitStatus() map[int]struct{} {
	return parseExitStatusSet(u.Config.Service.SuccessExitStatus)
}

func lookupUser(name string) (uint32, uint32, error) {
	if uid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(uid), uint32(os.Getgid()), nil
	}
	info, err := user.Lookup(name)
	if err != nil {
		return 0, 0, fmt.Errorf("lookup user %q: %w", name, err)
	}
	uid, err := strconv.ParseUint(info.Uid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse uid for %q: %w", name, err)
	}
	gid, err := strconv.ParseUint(info.Gid, 10, 32)
	if err != nil {
		return 0, 0, fmt.Errorf("parse gid for %q: %w", name, err)
	}
	return uint32(uid), uint32(gid), nil
}

func lookupGroup(name string) (uint32, error) {
	if gid, err := strconv.ParseUint(name, 10, 32); err == nil {
		return uint32(gid), nil
	}
	info, err := user.LookupGroup(name)
	if err != nil {
		return 0, fmt.Errorf("lookup group %q: %w", name, err)
	}
	gid, err := strconv.ParseUint(info.Gid, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("parse gid for %q: %w", name, err)
	}
	return uint32(gid), nil
}

func lookupSupplementaryGroups(entries []string) ([]uint32, error) {
	if len(entries) == 0 {
		return nil, nil
	}
	groups := make([]uint32, 0, len(entries))
	for _, entry := range entries {
		gid, err := lookupGroup(entry)
		if err != nil {
			return nil, err
		}
		groups = append(groups, gid)
	}
	return groups, nil
}

func parseExitStatusSet(raw string) map[int]struct{} {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return nil
	}
	result := make(map[int]struct{}, len(fields))
	for _, entry := range fields {
		if value, err := strconv.Atoi(entry); err == nil {
			result[value] = struct{}{}
		}
	}
	return result
}

func parseSystemdDuration(raw string, defaultValue time.Duration) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue
	}
	if raw == "0" || strings.EqualFold(raw, "infinity") {
		return 0
	}
	if parsed, err := time.ParseDuration(raw); err == nil {
		return parsed
	}
	if seconds, err := time.ParseDuration(raw + "s"); err == nil {
		return seconds
	}
	return defaultValue
}

func parseFileMode(raw string, defaultValue os.FileMode) (os.FileMode, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultValue, nil
	}
	parsed, err := strconv.ParseUint(raw, 8, 32)
	if err != nil {
		return 0, fmt.Errorf("parse file mode %q: %w", raw, err)
	}
	return os.FileMode(parsed), nil
}
