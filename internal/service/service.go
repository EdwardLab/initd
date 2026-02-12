package service

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

	execStart := strings.TrimSpace(u.Config.Service.ExecStart)
	if execStart == "" {
		err := errors.New("ExecStart is empty")
		u.markFailed(err, false)
		return token, err
	}

	if err := u.ensureRuntimeDirectory(); err != nil {
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

	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Dir = filepath.Dir(u.Path)

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

	cmd.Env = envList

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

	cmd.Stdout = stdoutLogger
	cmd.Stderr = stderrLogger

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
		resetActive := serviceType == "simple"
		pid := cmd.Process.Pid

		u.reaper.Register(pid, func(status syscall.WaitStatus) {
			u.handleExitStatus(token, status, ignoreFailure, resetActive)
		})
	}

	// -------------------------------------------------
	// Dispatch wait handlers
	// -------------------------------------------------
	switch serviceType {
	case "oneshot":
		go u.waitOneshot(token, ignoreFailure)
	case "forking":
		go u.waitForking(token, ignoreFailure)
	case "notify":
		go u.waitNotify(token, ignoreFailure)
	default:
		go u.waitSimple(token, ignoreFailure)
	}
}


func (u *Unit) waitSimple(token int, ignoreFailure bool) {
	if u.reaper != nil {
		return
	}
	err := u.Cmd.Wait()
	u.handleExit(token, err, ignoreFailure, true)
}

func (u *Unit) waitOneshot(token int, ignoreFailure bool) {
	if u.reaper != nil {
		return
	}
	err := u.Cmd.Wait()
	u.handleExit(token, err, ignoreFailure, false)
}

func (u *Unit) waitForking(token int, ignoreFailure bool) {
	startedAt := time.Now()
	startedAtMonotonic := logging.MonotonicNow()
	timeout := 2 * time.Second
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
	defer func() {
		u.mu.Lock()
		if u.notifyServer != nil {
			u.notifyServer.Stop()
			u.notifyServer = nil
		}
		u.mu.Unlock()
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

			select {
			case <-done:
				// Clean exit
			case <-time.After(timeout):
				// Escalate
				if pid != 0 {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
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

	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		if serviceType == "forking" {
			if pid == 0 || !processAlive(pid) {
				u.transitionState(StateInactive, "")
				return nil
			}
		} else {
			state := u.Snapshot().State
			if state == StateInactive || state == StateFailed {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
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

func (u *Unit) runStopCommand(command string) error {
	envMap, envList, err := u.buildEnvironment()
	if err != nil {
		return err
	}

	return u.runCommand(command, envMap, envList)
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
	u.handleExitCode(token, exitCode, err, ignoreFailure, resetActive)
}

func (u *Unit) handleExitStatus(token int, status syscall.WaitStatus, ignoreFailure bool, resetActive bool) {
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
	u.handleExitCode(token, exitCode, err, ignoreFailure, resetActive)
}

func (u *Unit) handleExitCode(token int, exitCode int, err error, ignoreFailure bool, resetActive bool) {
	u.mu.Lock()
	defer u.mu.Unlock()

	if u.startToken != token {
		return
	}

	// Respect RestartPreventExitStatus
	if prevent := u.RestartPreventExitStatus(); prevent != nil {
		if _, blocked := prevent[exitCode]; blocked {
			return
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
	runtimeDir := strings.TrimSpace(u.Config.Service.RuntimeDirectory)
	if runtimeDir == "" {
		return nil
	}
	mode := os.FileMode(0o755)
	if modeStr := strings.TrimSpace(u.Config.Service.RuntimeDirectoryMode); modeStr != "" {
		parsed, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return fmt.Errorf("RuntimeDirectoryMode parse: %w", err)
		}
		mode = os.FileMode(parsed)
	}
	path := filepath.Join("/run", runtimeDir)
	if err := os.MkdirAll(path, mode); err != nil {
		return fmt.Errorf("RuntimeDirectory create: %w", err)
	}
	return os.Chmod(path, mode)
}

func (u *Unit) runExecStartPre(token int, envMap map[string]string, envList []string) error {
	for _, command := range u.Config.Service.ExecStartPre {
		if !u.isCurrentToken(token) {
			return nil
		}
		if strings.TrimSpace(command) == "" {
			continue
		}
		if err := u.runCommand(command, envMap, envList); err != nil {
			return err
		}
	}
	return nil
}

func (u *Unit) runCommand(command string, envMap map[string]string, envList []string) error {
	command, ignoreFailure := stripPrefix(command)
	expanded := expandWithEnv(command, envMap)
	args, err := shlex.Split(expanded)
	if err != nil {
		if ignoreFailure {
			return nil
		}
		return fmt.Errorf("parse command: %w", err)
	}
	if len(args) == 0 {
		return nil
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = envList
	cmd.Dir = filepath.Dir(u.Path)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdoutLogger := &logging.LineLogger{Unit: u.Config.Name, PID: 0, Level: logging.LevelInfo, Buffer: u.Logs, Output: os.Stdout}
	stderrLogger := &logging.LineLogger{Unit: u.Config.Name, PID: 0, Level: logging.LevelError, Buffer: u.Logs, Output: os.Stderr}
	cmd.Stdout = stdoutLogger
	cmd.Stderr = stderrLogger
	if err := cmd.Start(); err != nil {
		if ignoreFailure {
			return nil
		}
		return err
	}
	stdoutLogger.PID = cmd.Process.Pid
	stderrLogger.PID = cmd.Process.Pid
	if u.reaper != nil {
		done := make(chan syscall.WaitStatus, 1)
		u.reaper.Register(cmd.Process.Pid, func(status syscall.WaitStatus) {
			done <- status
		})
		status := <-done
		if err := waitStatusError(status); err != nil {
			if ignoreFailure {
				return nil
			}
			return err
		}
		return nil
	}
	if err := cmd.Wait(); err != nil {
		if ignoreFailure {
			return nil
		}
		return err
	}
	return nil
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


func (u *Unit) isCurrentToken(token int) bool {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.startToken == token
}

func (u *Unit) IsCurrentToken(token int) bool {
	return u.isCurrentToken(token)
}

func (u *Unit) RestartPreventExitStatus() map[int]struct{} {
	raw := strings.Fields(u.Config.Service.RestartPreventExitStatus)
	if len(raw) == 0 {
		return nil
	}
	result := make(map[int]struct{}, len(raw))
	for _, entry := range raw {
		if value, err := strconv.Atoi(entry); err == nil {
			result[value] = struct{}{}
		}
	}
	return result
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

func (u *Unit) waitNotify(token int, ignoreFailure bool) {
	u.mu.Lock()
	server := u.notifyServer
	cmd := u.Cmd
	u.mu.Unlock()

	if server == nil {
		u.markFailed(fmt.Errorf("notify server missing"), ignoreFailure)
		return
	}

	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	select {

	case <-server.Ready:
		u.mu.Lock()
		if u.startToken == token {
			u.Runtime.State = StateActive
			if cmd != nil && cmd.Process != nil {
				u.Runtime.MainPID = cmd.Process.Pid
			}
		}
		u.mu.Unlock()

		return

	case <-timer.C:
		u.mu.Lock()
		if u.startToken != token {
			u.mu.Unlock()
			return
		}
		cmd = u.Cmd
		u.mu.Unlock()

		if cmd != nil && cmd.Process != nil {
			u.killMainProcess(syscall.SIGTERM)
		}

		u.markFailed(fmt.Errorf("notify timeout"), ignoreFailure)
		return
	}
}

