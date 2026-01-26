package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"initd/internal/ipc"
	"initd/internal/supervisor"
)

func main() {
	socketPath := flag.String("socket", "/run/initd.sock", "path to initd unix socket")
	initMode := flag.Bool("init", false, "run as init/supervisor")
	flag.Parse()

	manager := supervisor.NewManager()
	if err := manager.LoadUnits(); err != nil {
		log.Printf("failed to load units: %v", err)
	}

	go func() {
		if err := ipc.Serve(*socketPath, manager); err != nil {
			log.Fatalf("ipc server failed: %v", err)
		}
	}()

	if *initMode {
		if err := manager.StartEnabledUnits(); err != nil {
			log.Printf("failed to start enabled units: %v", err)
		}
		for {
			time.Sleep(1 * time.Second)
		}
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	<-signals

	_ = os.Remove(*socketPath)
}
