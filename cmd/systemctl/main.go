package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"initd/internal/ipc"

	"golang.org/x/sys/unix"
)

const systemctlVersion = "0.0.2"

func main() {
	if wantsHelp(os.Args[1:]) {
		printHelp()
		return
	}
	if wantsVersion(os.Args[1:]) {
		printVersion()
		return
	}

	flags := flag.NewFlagSet("systemctl", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	socketPath := flags.String("socket", "/run/initd.sock", "path to initd unix socket")
	if err := flags.Parse(os.Args[1:]); err != nil {
		usage()
		os.Exit(1)
	}

	if flags.NArg() < 1 {
		usage()
		os.Exit(1)
	}

	cmd := flags.Arg(0)
	args := flags.Args()[1:]

	client := &ipc.Client{SocketPath: *socketPath}

	switch cmd {
	case "start", "stop", "restart", "status", "is-active", "is-enabled", "enable", "disable":
		if len(args) < 1 {
			fmt.Fprintf(os.Stderr, "%s requires a unit name\n", cmd)
			os.Exit(1)
		}
		handleUnitCommand(client, cmd, args[0])

	case "list-units":
		handleListUnits(client)

	case "list-unit-files":
		handleListUnitFiles(client)

	case "daemon-reload", "reboot", "poweroff", "halt":
		handleSimple(client, cmd)

	default:
		usage()
		os.Exit(1)
	}
}

func handleSimple(client *ipc.Client, action string) {
	resp, err := client.Do(ipc.Request{Action: action})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}
}

func handleUnitCommand(client *ipc.Client, action, unit string) {
	resolvedUnit, err := resolveUnitName(client, unit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		os.Exit(1)
	}
	resp, err := client.Do(ipc.Request{Action: action, Unit: resolvedUnit})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	switch action {
	case "status":
		status := decodeStatus(resp)
		enabled := fetchEnabledState(client, resolvedUnit)
		printStatus(status, enabled)
		exitForState(string(status.State))
	case "is-active":
		state := fmt.Sprintf("%v", resp.Data)
		fmt.Println(state)
		if state == "active" {
			os.Exit(0)
		}
		os.Exit(3)
	case "is-enabled":
		state := fmt.Sprintf("%v", resp.Data)
		fmt.Println(state)
		if state == "enabled" {
			os.Exit(0)
		}
		os.Exit(1)
	}
}

func resolveUnitName(_ *ipc.Client, unit string) (string, error) {
	if strings.HasSuffix(unit, ".service") || strings.Contains(unit, ".") {
		return unit, nil
	}
	return unit + ".service", nil
}

func handleListUnits(client *ipc.Client) {
	resp, err := client.Do(ipc.Request{Action: "list-units"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	var units []ipc.UnitData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &units)

	fmt.Printf("UNIT\tLOAD\tACTIVE\tDESCRIPTION\n")
	for _, unit := range units {
		active := "inactive"
		if unit.State == "active" {
			active = "active"
		}
		fmt.Printf("%s\tloaded\t%s\t%s\n", unit.Name, active, unit.Description)
	}
}

func handleListUnitFiles(client *ipc.Client) {
	resp, err := client.Do(ipc.Request{Action: "list-unit-files"})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	if !resp.Success {
		fmt.Fprintf(os.Stderr, "%s\n", resp.Message)
		os.Exit(1)
	}

	var units []ipc.UnitFileData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &units)

	fmt.Printf("UNIT FILE\tSTATE\n")
	for _, unit := range units {
		fmt.Printf("%s\t%s\n", unit.Name, unit.State)
	}
}

func decodeStatus(resp ipc.Response) ipc.StatusData {
	var status ipc.StatusData
	data, _ := json.Marshal(resp.Data)
	_ = json.Unmarshal(data, &status)
	return status
}

func printStatus(status ipc.StatusData, enabled string) {
	unitBase := strings.TrimSuffix(status.Name, ".service")

	fmt.Printf("● %s - %s\n", status.Name, status.Description)
	fmt.Printf("   Loaded: loaded (%s; %s)\n", status.Name, enabled)

	activeLine := string(status.State)
	if status.State == "active" {
		activeLine = "active (running)"
	}

	if !status.StartedAt.IsZero() {
		startedAt := status.StartedAt.Local()
		monotonicSince := formatSince(status.StartedAtMonotonic)
		fmt.Printf(
			"   Active: %s since %s; %s ago\n",
			activeLine,
			startedAt.Format("Mon, 02 Jan 2006 15:04:05 MST"),
			monotonicSince,
		)
	} else {
		fmt.Printf("   Active: %s\n", activeLine)
	}

	if status.MainPID > 0 {
		fmt.Printf(" Main PID: %d\n", status.MainPID)
	}

	if status.LastError != "" {
		fmt.Printf("   Error: %s\n", status.LastError)
	}

	if len(status.Logs) > 0 {
		fmt.Println("\nLogs:")
		for _, raw := range status.Logs {
			line := strings.TrimSpace(raw)

			// strip kernel-style monotonic prefix: [1234.567890]
			if strings.HasPrefix(line, "[") {
				if idx := strings.Index(line, "]"); idx > 0 {
					line = strings.TrimSpace(line[idx+1:])
				}
			}

			// map unit[0] → systemd[1]
			if strings.HasPrefix(line, status.Name+"[0]:") {
				line = "systemd[1]:" + strings.TrimPrefix(line, status.Name+"[0]:")
			}

			// unit.service[pid] → unit[pid]
			line = strings.ReplaceAll(line, status.Name+"[", unitBase+"[")

			fmt.Printf(" %s\n", line)
		}
	}
}


func monotonicNow() time.Duration {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		return 0
	}
	return time.Duration(ts.Sec)*time.Second + time.Duration(ts.Nsec)
}

func formatSince(start time.Duration) string {
	if start <= 0 {
		return "0s"
	}
	now := monotonicNow()
	if now <= start {
		return "0s"
	}
	delta := now - start
	if delta < 0 {
		return "0s"
	}
	delta = delta.Round(time.Second)
	seconds := int(delta.Seconds())
	hours := seconds / 3600
	minutes := (seconds % 3600) / 60
	secs := seconds % 60
	switch {
	case hours > 0:
		return fmt.Sprintf("%dh %dm %ds", hours, minutes, secs)
	case minutes > 0:
		return fmt.Sprintf("%dm %ds", minutes, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}

func fetchEnabledState(client *ipc.Client, unit string) string {
	resp, err := client.Do(ipc.Request{Action: "is-enabled", Unit: unit})
	if err != nil || !resp.Success {
		return "disabled"
	}
	return fmt.Sprintf("%v", resp.Data)
}

func exitForState(state string) {
	switch state {
	case "active":
		os.Exit(0)
	case "failed":
		os.Exit(1)
	default:
		os.Exit(3)
	}
}

func usage() {
	fmt.Println("Usage: systemctl [OPTIONS...] {COMMAND} [UNIT...]")
}

func wantsHelp(args []string) bool {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			return true
		}
	}
	if len(args) > 0 && args[0] == "help" {
		return true
	}
	return false
}

func wantsVersion(args []string) bool {
	for _, arg := range args {
		if arg == "-V" || arg == "--version" {
			return true
		}
	}
	return false
}

func printHelp() {
	fmt.Println("systemctl [OPTIONS...] COMMAND [UNIT...]")
	fmt.Println()
	fmt.Println("Query or send control commands to the initd system manager.")
	fmt.Println()
	fmt.Println("Options:")
	fmt.Println("  --socket=PATH        Path to initd control socket")
	fmt.Println("  -h, --help           Show this help")
	fmt.Println("  -V, --version        Show version")
	fmt.Println()
	fmt.Println("Unit Commands:")
	fmt.Println("  start UNIT...        Start (activate) one or more units")
	fmt.Println("  stop UNIT...         Stop (deactivate) one or more units")
	fmt.Println("  restart UNIT...      Restart one or more units")
	fmt.Println("  status UNIT...       Show runtime status of one or more units")
	fmt.Println("  is-active UNIT...    Check whether units are active")
	fmt.Println("  is-enabled UNIT...   Check whether unit files are enabled")
	fmt.Println("  enable UNIT...       Enable one or more unit files")
	fmt.Println("  disable UNIT...      Disable one or more unit files")
	fmt.Println("  list-units           List loaded units")
	fmt.Println("  list-unit-files      List installed unit files")
	fmt.Println("  daemon-reload        Reload unit files")
	fmt.Println("System Commands:")
	fmt.Println("  reboot               Reboot the system")
	fmt.Println("  poweroff             Power off the system")
	fmt.Println("  halt                 Halt the system")
	fmt.Println()
	fmt.Println("Report bugs to: https://github.com/EdwardLab/initd")
}

func printVersion() {
	fmt.Printf("systemctl (initd) %s by EdwardLab (https://github.com/EdwardLab) MIT License\n", systemctlVersion)
}
