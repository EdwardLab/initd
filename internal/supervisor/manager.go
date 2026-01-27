package supervisor

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"initd/internal/logging"
	"initd/internal/parser"
	"initd/internal/service"
)

type Manager struct {
	mu          sync.Mutex
	startMu     sync.Mutex
	Units       map[string]*service.Unit
	SearchPaths []string
	UnitOrder   []string
	reaper      service.ExitReaper
}

func NewManager() *Manager {
	return &Manager{
		Units: map[string]*service.Unit{},
		SearchPaths: []string{
			"/etc/systemd/system",
			"/lib/systemd/system",
			"/usr/lib/systemd/system",
		},
	}
}

func (m *Manager) SetReaper(reaper service.ExitReaper) {
	m.mu.Lock()
	m.reaper = reaper
	for _, unit := range m.Units {
		unit.SetReaper(reaper)
	}
	m.mu.Unlock()
}

func (m *Manager) LoadUnits() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	units := map[string]*service.Unit{}
	order := []string{}

	for _, dir := range m.SearchPaths {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".service") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			unitConfig, err := parser.ParseUnit(path)
			if err != nil {
				continue
			}
			unitConfig.Name = entry.Name()
			unit := service.NewUnit(unitConfig, path)
			if m.reaper != nil {
				unit.SetReaper(m.reaper)
			}
			units[entry.Name()] = unit
			order = append(order, entry.Name())
		}
	}

	m.Units = units
	m.UnitOrder = order
	return nil
}

func (m *Manager) FindUnit(name string) (*service.Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	unit, ok := m.Units[name]
	if !ok {
		return nil, fmt.Errorf("unit %s not found", name)
	}
	return unit, nil
}

func (m *Manager) StartUnit(name string) error {
	m.startMu.Lock()
	defer m.startMu.Unlock()

	started := map[string]struct{}{}
	stack := map[string]struct{}{}
	return m.startUnitWithDependencies(name, started, stack)
}

func (m *Manager) startUnitWithDependencies(name string, started map[string]struct{}, stack map[string]struct{}) error {
	if _, ok := started[name]; ok {
		return nil
	}
	if _, ok := stack[name]; ok {
		logKernelWarning(fmt.Sprintf("Dependency cycle detected while starting %s; breaking cycle.", name))
		return nil
	}

	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	stack[name] = struct{}{}

	deps := m.collectDependencies(unit)
	if err := m.startDependencies(unit, deps, started, stack); err != nil {
		unit.MarkFailed(err.Error())
		delete(stack, name)
		return err
	}

	token, err := unit.Start()
	if err != nil {
		delete(stack, name)
		return err
	}
	m.applyRestartPolicy(unit, token)
	started[name] = struct{}{}
	delete(stack, name)
	return nil
}

func (m *Manager) StartEnabledUnits() error {
	units, err := m.EnabledUnits()
	if err != nil {
		return err
	}
	m.startMu.Lock()
	defer m.startMu.Unlock()

	ordered := m.orderUnitsByAfter(units)
	started := map[string]struct{}{}
	for _, unit := range ordered {
		if err := m.startUnitWithDependencies(unit.Config.Name, started, map[string]struct{}{}); err != nil {
			unit.Log(logging.LevelError, fmt.Sprintf("Failed to start enabled unit: %v", err))
		}
	}
	return nil
}

type dependency struct {
	name     string
	required bool
}

func (m *Manager) collectDependencies(unit *service.Unit) []dependency {
	deps := make([]dependency, 0, len(unit.Config.Requires)+len(unit.Config.Wants))
	seen := map[string]struct{}{}
	add := func(name string, required bool) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		deps = append(deps, dependency{name: name, required: required})
	}
	for _, dep := range unit.Config.Requires {
		add(dep, true)
	}
	for _, dep := range unit.Config.Wants {
		add(dep, false)
	}
	return deps
}

func (m *Manager) startDependencies(unit *service.Unit, deps []dependency, started map[string]struct{}, stack map[string]struct{}) error {
	if len(deps) == 0 {
		return nil
	}

	depUnits := make([]*service.Unit, 0, len(deps))
	depMeta := make(map[string]dependency, len(deps))
	for _, dep := range deps {
		depUnit, err := m.FindUnit(dep.name)
		if err != nil {
			if dep.required {
				return fmt.Errorf("required unit %s not found", dep.name)
			}
			unit.Log(logging.LevelError, fmt.Sprintf("Wanted unit %s not found", dep.name))
			continue
		}
		depUnits = append(depUnits, depUnit)
		depMeta[depUnit.Config.Name] = dep
	}

	ordered := m.orderUnitsByAfter(depUnits)
	for _, depUnit := range ordered {
		meta := depMeta[depUnit.Config.Name]
		if err := m.startUnitWithDependencies(depUnit.Config.Name, started, stack); err != nil {
			if meta.required {
				return fmt.Errorf("required unit %s failed: %w", depUnit.Config.Name, err)
			}
			unit.Log(logging.LevelError, fmt.Sprintf("Wanted unit %s failed: %v", depUnit.Config.Name, err))
			continue
		}
		if meta.required {
			if err := m.waitForUnitReady(depUnit, 30*time.Second); err != nil {
				return fmt.Errorf("required unit %s failed: %w", depUnit.Config.Name, err)
			}
		}
	}
	return nil
}

func (m *Manager) waitForUnitReady(unit *service.Unit, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		snapshot := unit.Snapshot()
		if snapshot.State == service.StateFailed {
			if snapshot.LastError != "" {
				return errors.New(snapshot.LastError)
			}
			return fmt.Errorf("unit %s failed", unit.Config.Name)
		}
		if snapshot.State != service.StateActivating && snapshot.State != service.StateStopping {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	logKernelWarning(fmt.Sprintf("Timeout waiting for %s to finish activating; continuing.", unit.Config.Name))
	return nil
}

func (m *Manager) StopUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	return unit.Stop(10 * time.Second)
}

func (m *Manager) RestartUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	return unit.Restart(10 * time.Second)
}

func (m *Manager) ListUnits() []*service.Unit {
	m.mu.Lock()
	defer m.mu.Unlock()

	units := make([]*service.Unit, 0, len(m.Units))
	for _, unit := range m.Units {
		units = append(units, unit)
	}
	return units
}

func (m *Manager) Reload() error {
	return m.LoadUnits()
}

func (m *Manager) applyRestartPolicy(unit *service.Unit, token int) {
	restart := strings.ToLower(strings.TrimSpace(unit.Config.Service.Restart))
	if restart == "" || restart == "no" {
		return
	}

	restartSec := 0 * time.Second
	if unit.Config.Service.RestartSec != "" {
		if parsed, err := time.ParseDuration(unit.Config.Service.RestartSec); err == nil {
			restartSec = parsed
		} else if seconds, err := time.ParseDuration(unit.Config.Service.RestartSec + "s"); err == nil {
			restartSec = seconds
		}
	}

	// systemd uses StartLimit* to avoid restart storms; we hardcode a small window.
	startLimitInterval := 10 * time.Second
	startLimitBurst := 5

	preventStatuses := unit.RestartPreventExitStatus()

	go func() {
		for {
			time.Sleep(500 * time.Millisecond)
			if !unit.IsCurrentToken(token) {
				return
			}
			unitState := unit.Snapshot().State
			if unitState == service.StateActive || unitState == service.StateActivating || unitState == service.StateStopping {
				continue
			}
			exitCode := unit.Snapshot().ExitCode
			if _, blocked := preventStatuses[exitCode]; blocked {
				return
			}
			shouldRestart := false
			switch restart {
			case "always":
				shouldRestart = true
			case "on-failure":
				shouldRestart = exitCode != 0
			}
			if !shouldRestart {
				return
			}
			restartCount := unit.RecordRestart(time.Now(), startLimitInterval)
			if restartCount > startLimitBurst {
				unit.MarkFailed("Start request repeated too quickly")
				unit.Log(logging.LevelError, "Start request repeated too quickly.")
				return
			}
			unit.Log(logging.LevelInfo, fmt.Sprintf("Restarting service (attempt %d).", restartCount))
			time.Sleep(restartSec)
			newToken, err := unit.Start()
			if err != nil {
				unit.Log(logging.LevelError, fmt.Sprintf("Restart failed: %v", err))
				return
			}
			token = newToken
		}
	}()
}

func (m *Manager) EnableUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	if len(unit.Config.Install.WantedBy) == 0 {
		return errors.New("WantedBy not set")
	}
	for _, target := range unit.Config.Install.WantedBy {
		wantsDir := filepath.Join("/etc/systemd/system", fmt.Sprintf("%s.wants", target))
		if err := os.MkdirAll(wantsDir, 0o755); err != nil {
			return err
		}
		linkPath := filepath.Join(wantsDir, name)
		_ = os.Remove(linkPath)
		if err := os.Symlink(unit.Path, linkPath); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) DisableUnit(name string) error {
	unit, err := m.FindUnit(name)
	if err != nil {
		return err
	}
	for _, target := range unit.Config.Install.WantedBy {
		wantsDir := filepath.Join("/etc/systemd/system", fmt.Sprintf("%s.wants", target))
		linkPath := filepath.Join(wantsDir, name)
		_ = os.Remove(linkPath)
	}
	return nil
}

func (m *Manager) IsEnabled(name string) (bool, error) {
	m.mu.Lock()
	_, ok := m.Units[name]
	m.mu.Unlock()
	if !ok {
		return false, fmt.Errorf("unit %s not found", name)
	}
	enabled, err := m.enabledUnitNames()
	if err != nil {
		return false, err
	}
	_, ok = enabled[name]
	return ok, nil
}

func (m *Manager) ListUnitFiles() ([]*service.Unit, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	units := make([]*service.Unit, 0, len(m.Units))
	for _, unit := range m.Units {
		units = append(units, unit)
	}
	return units, nil
}

func (m *Manager) EnabledUnits() ([]*service.Unit, error) {
	enabled, err := m.enabledUnitNames()
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	ordered := make([]*service.Unit, 0, len(enabled))
	for _, name := range m.UnitOrder {
		if _, ok := enabled[name]; !ok {
			continue
		}
		if unit, ok := m.Units[name]; ok {
			ordered = append(ordered, unit)
		}
	}
	return ordered, nil
}

func (m *Manager) enabledUnitNames() (map[string]struct{}, error) {
	root := "/etc/systemd/system"
	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	enabled := map[string]struct{}{}
	for _, entry := range dirs {
		if !entry.IsDir() || !strings.HasSuffix(entry.Name(), ".wants") {
			continue
		}
		wantsDir := filepath.Join(root, entry.Name())
		wantsEntries, err := os.ReadDir(wantsDir)
		if err != nil {
			continue
		}
		for _, want := range wantsEntries {
			name := want.Name()
			if !strings.HasSuffix(name, ".service") {
				continue
			}
			if want.Type()&os.ModeSymlink == 0 {
				continue
			}
			enabled[name] = struct{}{}
		}
	}
	return enabled, nil
}

func (m *Manager) orderUnitsByAfter(units []*service.Unit) []*service.Unit {
	if len(units) <= 1 {
		return units
	}

	orderIndex := map[string]int{}
	nameToUnit := map[string]*service.Unit{}
	for idx, unit := range units {
		orderIndex[unit.Config.Name] = idx
		nameToUnit[unit.Config.Name] = unit
	}

	adj := map[string][]string{}
	indegree := map[string]int{}
	for _, unit := range units {
		indegree[unit.Config.Name] = 0
	}

	for _, unit := range units {
		for _, dep := range unit.Config.After {
			if strings.HasSuffix(dep, ".target") || !strings.HasSuffix(dep, ".service") {
				continue
			}
			if _, ok := nameToUnit[dep]; !ok || dep == unit.Config.Name {
				continue
			}
			adj[dep] = append(adj[dep], unit.Config.Name)
			indegree[unit.Config.Name]++
		}
	}

	queue := make([]string, 0, len(units))
	for _, unit := range units {
		if indegree[unit.Config.Name] == 0 {
			queue = append(queue, unit.Config.Name)
		}
	}

	sorted := make([]*service.Unit, 0, len(units))
	for len(queue) > 0 {
		name := queue[0]
		queue = queue[1:]
		sorted = append(sorted, nameToUnit[name])

		neighbors := adj[name]
		if len(neighbors) > 1 {
			sort.SliceStable(neighbors, func(i, j int) bool {
				return orderIndex[neighbors[i]] < orderIndex[neighbors[j]]
			})
		}
		for _, next := range neighbors {
			indegree[next]--
			if indegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}

	if len(sorted) != len(units) {
		// Best-effort ordering: on cycles fall back to file order.
		logKernelWarning("After= dependency cycle detected; falling back to file order.")
		return units
	}
	return sorted
}

func logKernelWarning(message string) {
	logging.KernelPrintf(os.Stderr, "initd", os.Getpid(), "%s", message)
}
