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
	State      State
	MainPID    int
	ExitCode   int
	LastError  string
	StartedAt  time.Time
	FinishedAt time.Time
}

type Unit struct {
	mu             sync.Mutex
	Config         *parser.Unit
	Path           string
	Runtime        Runtime
	Cmd            *exec.Cmd
	Logs           *logging.Buffer
	restartHistory []time.Time
	notifyFallback bool
	startToken     int
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

	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = envList
	cmd.Dir = filepath.Dir(u.Path)

	stdoutLogger := &logging.LineLogger{Unit: u.Config.Name, PID: 0, Level: logging.LevelInfo, Buffer: u.Logs, Output: os.Stdout}
	stderrLogger := &logging.LineLogger{Unit: u.Config.Name, PID: 0, Level: logging.LevelError, Buffer: u.Logs, Output: os.Stderr}
	cmd.Stdout = stdoutLogger
	cmd.Stderr = stderrLogger

	if err := cmd.Start(); err != nil {
		u.markFailed(err, ignoreFailure)
		return
	}

	serviceType := u.canonicalServiceType()

	u.mu.Lock()
	if u.startToken != token {
		u.mu.Unlock()
		_ = cmd.Process.Kill()
		return
	}
	u.Cmd = cmd
	u.Runtime.StartedAt = time.Now()
	stdoutLogger.PID = cmd.Process.Pid
	stderrLogger.PID = cmd.Process.Pid
	switch serviceType {
	case "oneshot":
		u.Runtime.MainPID = cmd.Process.Pid
	case "forking":
		u.Runtime.State = StateActivating
	default:
		u.Runtime.MainPID = cmd.Process.Pid
		u.Runtime.State = StateActive
	}
	u.mu.Unlock()

	switch serviceType {
	case "oneshot":
		go u.waitOneshot(token, ignoreFailure)
	case "forking":
		go u.waitForking(token, ignoreFailure)
	default:
		go u.waitSimple(token, ignoreFailure)
	}
}

func (u *Unit) waitSimple(token int, ignoreFailure bool) {
	err := u.Cmd.Wait()
	u.handleExit(token, err, ignoreFailure, true)
}

func (u *Unit) waitOneshot(token int, ignoreFailure bool) {
	err := u.Cmd.Wait()
	u.handleExit(token, err, ignoreFailure, false)
}

func (u *Unit) waitForking(token int, ignoreFailure bool) {
	startedAt := time.Now()
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
	if u.startToken != token {
		u.mu.Unlock()
		return
	}
	u.Runtime.State = StateActive
	u.Runtime.MainPID = pid
	u.Runtime.StartedAt = startedAt
	u.mu.Unlock()

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
	u.mu.Unlock()

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
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.startToken != token {
		return
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
		if exitErr, ok := err.(*exec.ExitError); ok {
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok {
				u.Runtime.ExitCode = status.ExitStatus()
			}
		}
	} else {
		if u.Runtime.State == StateActive && resetActive {
			u.Runtime.State = StateInactive
		} else if u.Runtime.State != StateActive {
			u.Runtime.State = StateInactive
		}
		u.Runtime.ExitCode = 0
	}
	u.Runtime.FinishedAt = time.Now()

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
		Timestamp: time.Now(),
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
	case "forking", "oneshot", "simple", "idle", "exec", "dbus", "notify", "notify-reload":
		if serviceType == "dbus" {
			u.Log(logging.LevelInfo, fmt.Sprintf("Service type %q treats readiness as simple; notification ignored.", serviceType))
		}
		if serviceType == "notify" || serviceType == "notify-reload" {
			u.logNotifyFallback()
			return "simple"
		}
		return serviceType
	case "":
		return "simple"
	default:
		u.Log(logging.LevelError, fmt.Sprintf("Unsupported service type %q; treating as simple.", serviceType))
		return "simple"
	}
}

func (u *Unit) logNotifyFallback() {
	u.mu.Lock()
	if u.notifyFallback {
		u.mu.Unlock()
		return
	}
	u.notifyFallback = true
	u.mu.Unlock()
	u.Log(logging.LevelInfo, "Service type notify not supported; treating as simple")
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
