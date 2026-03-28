package parser

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Unit struct {
	Name                string
	Type                string 
	Description         string
	After               []string
	Requires            []string
	Wants               []string
	ConditionPathExists []string
	Service             ServiceSection
	Socket              SocketSection
	Install             InstallSection
	Ignored             map[string]string
}


type ServiceSection struct {
	Type                     string
	PermissionsStartOnly     bool
	ExecCondition            []string
	ExecStartPre             []string
	ExecStartPost            []string
	ExecStart                string
	ExecStop                 string
	ExecStopPost             []string
	ExecReload               []string
	Restart                  string
	RestartSec               string
	RestartPreventExitStatus string
	SuccessExitStatus        string
	PIDFile                  string
	WorkingDirectory         string
	RootDirectory            string
	RuntimeDirectory         []string
	RuntimeDirectoryMode     string
	StateDirectory           []string
	CacheDirectory           []string
	LogsDirectory            []string
	ConfigurationDirectory   []string
	KillMode                 string
	TimeoutStartSec          string
	TimeoutStopSec           string
	TimeoutSec               string
	User                     string
	Group                    string
	SupplementaryGroups      []string
	UMask                    string
	LimitNOFILE              string
	Environment              []string
	EnvironmentFile          []string
}

type SocketSection struct {
	ListenStream   []string
	ListenDatagram []string
	SocketMode     string
}

type InstallSection struct {
	WantedBy []string
}

var ignoredKeys = map[string]struct{}{
	"MemoryMax":                {},
	"CPUQuota":                 {},
	"TasksMax":                 {},
	"IOWeight":                 {},
	"DeviceAllow":              {},
	"DeviceDeny":               {},
	"PrivateTmp":               {},
	"ProtectSystem":            {},
	"RestrictNamespaces":       {},
	"CapabilityBoundingSet":    {},
	"AmbientCapabilities":      {},
	"SystemCallFilter":         {},
	"SystemCallArchitectures":  {},
	"ProtectProc":              {},
	"ProcSubset":               {},
	"NoExecPaths":              {},
	"ExecPaths":                {},
	"PrivateDevices":           {},
	"ProtectHome":              {},
	"ReadWritePaths":           {},
	"ReadOnlyPaths":            {},
	"InaccessiblePaths":        {},
	"ReadWriteDirectories":     {},
	"ReadOnlyDirectories":      {},
	"InaccessibleDirectories":  {},
	"NoNewPrivileges":          {},
	"LockPersonality":          {},
	"MemoryDenyWriteExecute":   {},
	"PrivateUsers":             {},
	"ProtectClock":             {},
	"ProtectControlGroups":     {},
	"ProtectHostname":          {},
	"ProtectKernelLogs":        {},
	"ProtectKernelModules":     {},
	"ProtectKernelTunables":    {},
	"RemoveIPC":                {},
	"RestrictAddressFamilies":  {},
	"RestrictRealtime":         {},
	"RestrictSUIDSGID":         {},
	"OOMScoreAdjust":           {},
	"Nice":                     {},
	"IOSchedulingClass":        {},
	"IOSchedulingPriority":     {},
	"CPUSchedulingPolicy":      {},
	"CPUSchedulingPriority":    {},
	"CPUAffinity":              {},
	"LimitNPROC":               {},
	"LimitCORE":                {},
	"LimitMEMLOCK":             {},
	"LimitAS":                  {},
	"LimitRSS":                 {},
	"LimitDATA":                {},
	"LimitSTACK":               {},
	"LimitCPU":                 {},
	"Slice":                    {},
	"Delegate":                 {},
	"TasksAccounting":          {},
	"MemoryAccounting":         {},
	"CPUAccounting":            {},
	"IOAccounting":             {},
	"BlockIOAccounting":        {},
	"DefaultDependencies":      {},
}

func ParseUnit(path string) (*Unit, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	unit := &Unit{
		Name:    filepath.Base(path),
		Ignored: map[string]string{},
	}

	switch {
	case strings.HasSuffix(unit.Name, ".socket"):
		unit.Type = "socket"
	case strings.HasSuffix(unit.Name, ".service"):
		unit.Type = "service"
	default:
		unit.Type = "unknown"
	}


	scanner := bufio.NewScanner(file)
	section := ""

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid line in %s: %s", path, line)
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])

		if _, ok := ignoredKeys[key]; ok {
			unit.Ignored[key] = value
			continue
		}

		switch section {
		case "Unit":
			switch key {
			case "Description":
				unit.Description = value
			case "After":
				unit.After = splitList(value)
			case "Requires":
				unit.Requires = splitList(value)
			case "Wants":
				unit.Wants = splitList(value)
			case "ConditionPathExists":
				unit.ConditionPathExists = append(unit.ConditionPathExists, value)
			default:
				unit.Ignored["Unit."+key] = value
			}
		case "Service":
			switch key {
			case "Type":
				unit.Service.Type = value
			case "PermissionsStartOnly":
				unit.Service.PermissionsStartOnly = strings.EqualFold(value, "yes") || strings.EqualFold(value, "true")
			case "ExecCondition":
				unit.Service.ExecCondition = append(unit.Service.ExecCondition, value)
			case "ExecStartPre":
				unit.Service.ExecStartPre = append(unit.Service.ExecStartPre, value)
			case "ExecStartPost":
				unit.Service.ExecStartPost = append(unit.Service.ExecStartPost, value)
			case "ExecStart":
				unit.Service.ExecStart = value
			case "ExecStop":
				unit.Service.ExecStop = value
			case "ExecStopPost":
				unit.Service.ExecStopPost = append(unit.Service.ExecStopPost, value)
			case "ExecReload":
				unit.Service.ExecReload = append(unit.Service.ExecReload, value)
			case "Restart":
				unit.Service.Restart = value
			case "RestartSec":
				unit.Service.RestartSec = value
			case "RestartPreventExitStatus":
				unit.Service.RestartPreventExitStatus = value
			case "SuccessExitStatus":
				unit.Service.SuccessExitStatus = value
			case "PIDFile":
				unit.Service.PIDFile = value
			case "WorkingDirectory":
				unit.Service.WorkingDirectory = value
			case "RootDirectory":
				unit.Service.RootDirectory = value
			case "RuntimeDirectory":
				unit.Service.RuntimeDirectory = splitList(value)
			case "RuntimeDirectoryMode":
				unit.Service.RuntimeDirectoryMode = value
			case "StateDirectory":
				unit.Service.StateDirectory = splitList(value)
			case "CacheDirectory":
				unit.Service.CacheDirectory = splitList(value)
			case "LogsDirectory":
				unit.Service.LogsDirectory = splitList(value)
			case "ConfigurationDirectory":
				unit.Service.ConfigurationDirectory = splitList(value)
			case "KillMode":
				unit.Service.KillMode = value
			case "TimeoutStartSec":
				unit.Service.TimeoutStartSec = value
			case "TimeoutStopSec":
				unit.Service.TimeoutStopSec = value
			case "TimeoutSec":
				unit.Service.TimeoutSec = value
			case "User":
				unit.Service.User = value
			case "Group":
				unit.Service.Group = value
			case "SupplementaryGroups":
				unit.Service.SupplementaryGroups = splitList(value)
			case "UMask":
				unit.Service.UMask = value
			case "LimitNOFILE":
				unit.Service.LimitNOFILE = value
			default:
				unit.Ignored["Service."+key] = value
			case "Environment":
				unit.Service.Environment = append(unit.Service.Environment, value)
			case "EnvironmentFile":
				unit.Service.EnvironmentFile = append(unit.Service.EnvironmentFile, value)
			}
		case "Socket":
			switch key {
			case "ListenStream":
				unit.Socket.ListenStream = append(unit.Socket.ListenStream, value)
			case "ListenDatagram":
				unit.Socket.ListenDatagram = append(unit.Socket.ListenDatagram, value)
			case "SocketMode":
				unit.Socket.SocketMode = value
			default:
				unit.Ignored["Socket."+key] = value
			}

		case "Install":
			switch key {
			case "WantedBy":
				unit.Install.WantedBy = splitList(value)
			default:
				unit.Ignored["Install."+key] = value
			}
		default:
			unit.Ignored[section+"."+key] = value
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}
	
	if unit.Type == "socket" {
		if len(unit.Socket.ListenStream) == 0 && len(unit.Socket.ListenDatagram) == 0 {
			return nil, fmt.Errorf("socket unit missing ListenStream/ListenDatagram")
		}
	}


	return unit, nil
}

func splitList(value string) []string {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return nil
	}
	return fields
}
