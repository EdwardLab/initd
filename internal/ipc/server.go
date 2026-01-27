package ipc

import (
	"encoding/json"
	"net"
	"os"
	"time"

	"initd/internal/boot"
	"initd/internal/logging"
	"initd/internal/service"
	"initd/internal/supervisor"
)

type Request struct {
	Action string `json:"action"`
	Unit   string `json:"unit,omitempty"`
}

type Response struct {
	Success bool        `json:"success"`
	Message string      `json:"message,omitempty"`
	Data    interface{} `json:"data,omitempty"`
}

type StatusData struct {
	Name                string        `json:"name"`
	Description         string        `json:"description"`
	State               service.State `json:"state"`
	MainPID             int           `json:"main_pid"`
	StartedAt           time.Time     `json:"started_at"`
	FinishedAt          time.Time     `json:"finished_at"`
	StartedAtMonotonic  time.Duration `json:"started_at_monotonic"`
	FinishedAtMonotonic time.Duration `json:"finished_at_monotonic"`
	LastError           string        `json:"last_error"`
	Logs                []string      `json:"logs"`
}

type UnitData struct {
	Name        string        `json:"name"`
	Description string        `json:"description"`
	State       service.State `json:"state"`
}

type UnitFileData struct {
	Name  string `json:"name"`
	State string `json:"state"`
	Path  string `json:"path"`
}

func Serve(socketPath string, manager *supervisor.Manager) error {
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return err
	}
	defer listener.Close()

	for {
		conn, err := listener.Accept()
		if err != nil {
			continue
		}
		go handleConn(conn, manager)
	}
}

func handleConn(conn net.Conn, manager *supervisor.Manager) {
	defer conn.Close()
	decoder := json.NewDecoder(conn)
	encoder := json.NewEncoder(conn)

	var req Request
	if err := decoder.Decode(&req); err != nil {
		_ = encoder.Encode(Response{Success: false, Message: err.Error()})
		return
	}

	response := dispatch(req, manager)
	_ = encoder.Encode(response)
}

func dispatch(req Request, manager *supervisor.Manager) Response {
	switch req.Action {
	case "start":
		if err := manager.StartUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "stop":
		if err := manager.StopUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "restart":
		if err := manager.RestartUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "status":
		unit, err := manager.FindUnit(req.Unit)
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		snapshot := unit.Snapshot()
		logs := unit.Logs.Entries()
		logLines := make([]string, 0, len(logs))
		for _, entry := range logs {
			logLines = append(logLines, logging.FormatEntry(entry))
		}
		return Response{Success: true, Data: StatusData{
			Name:                unit.Config.Name,
			Description:         unit.Description(),
			State:               snapshot.State,
			MainPID:             snapshot.MainPID,
			StartedAt:           snapshot.StartedAt,
			FinishedAt:          snapshot.FinishedAt,
			StartedAtMonotonic:  snapshot.StartedAtMonotonic,
			FinishedAtMonotonic: snapshot.FinishedAtMonotonic,
			LastError:           snapshot.LastError,
			Logs:                logLines,
		}}
	case "is-active":
		unit, err := manager.FindUnit(req.Unit)
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		state := unit.Snapshot().State
		return Response{Success: true, Data: state}
	case "list-units":
		units := manager.ListUnits()
		data := make([]UnitData, 0, len(units))
		for _, unit := range units {
			snapshot := unit.Snapshot()
			data = append(data, UnitData{Name: unit.Config.Name, Description: unit.Description(), State: snapshot.State})
		}
		return Response{Success: true, Data: data}
	case "list-unit-files":
		units, err := manager.ListUnitFiles()
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		data := make([]UnitFileData, 0, len(units))
		for _, unit := range units {
			enabled, _ := manager.IsEnabled(unit.Config.Name)
			state := "disabled"
			if enabled {
				state = "enabled"
			}
			data = append(data, UnitFileData{Name: unit.Config.Name, State: state, Path: unit.Path})
		}
		return Response{Success: true, Data: data}
	case "enable":
		if err := manager.EnableUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "disable":
		if err := manager.DisableUnit(req.Unit); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "is-enabled":
		enabled, err := manager.IsEnabled(req.Unit)
		if err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		if enabled {
			return Response{Success: true, Data: "enabled"}
		}
		return Response{Success: true, Data: "disabled"}
	case "daemon-reload":
		if err := manager.Reload(); err != nil {
			return Response{Success: false, Message: err.Error()}
		}
		return Response{Success: true}
	case "reboot", "poweroff", "halt":
		go func() {
			boot.Shutdown(manager, req.Action)
		}()
		return Response{Success: true}
	default:
		return Response{Success: false, Message: "unknown action"}
	}
}
