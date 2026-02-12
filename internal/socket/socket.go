package socket

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"initd/internal/parser"
)

type Runtime struct {
	mu        sync.Mutex
	Path      string
	Network   string
	Listener  net.Listener
	Packet    net.PacketConn
	Active    bool
}

func Start(unit *parser.Unit) (*Runtime, error) {
	cfg := unit

	if cfg.Socket.ListenStream == "" && cfg.Socket.ListenDatagram == "" {
		return nil, fmt.Errorf("socket unit has no ListenStream or ListenDatagram")
	}

	r := &Runtime{}

	if cfg.Socket.ListenStream != "" {
		if err := r.startStream(cfg.Socket.ListenStream, cfg.Socket.SocketMode); err != nil {
			return nil, err
		}
		return r, nil
	}

	if cfg.Socket.ListenDatagram != "" {
		if err := r.startDatagram(cfg.Socket.ListenDatagram, cfg.Socket.SocketMode); err != nil {
			return nil, err
		}
		return r, nil
	}

	return nil, fmt.Errorf("unsupported socket configuration")
}

func (r *Runtime) startStream(path string, modeStr string) error {
	if err := prepareSocketPath(path); err != nil {
		return err
	}

	l, err := net.Listen("unix", path)
	if err != nil {
		return fmt.Errorf("failed to create unix stream socket: %w", err)
	}

	if err := applyMode(path, modeStr); err != nil {
		l.Close()
		return err
	}

	r.mu.Lock()
	r.Path = path
	r.Network = "unix"
	r.Listener = l
	r.Active = true
	r.mu.Unlock()

	return nil
}

func (r *Runtime) startDatagram(path string, modeStr string) error {
	if err := prepareSocketPath(path); err != nil {
		return err
	}

	addr := &net.UnixAddr{
		Name: path,
		Net:  "unixgram",
	}

	pc, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		return fmt.Errorf("failed to create unix datagram socket: %w", err)
	}

	if err := applyMode(path, modeStr); err != nil {
		pc.Close()
		return err
	}

	r.mu.Lock()
	r.Path = path
	r.Network = "unixgram"
	r.Packet = pc
	r.Active = true
	r.mu.Unlock()

	return nil
}

func (r *Runtime) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.Active {
		return nil
	}

	if r.Listener != nil {
		_ = r.Listener.Close()
		r.Listener = nil
	}

	if r.Packet != nil {
		_ = r.Packet.Close()
		r.Packet = nil
	}

	if r.Path != "" {
		_ = os.Remove(r.Path)
	}

	r.Active = false
	return nil
}

func prepareSocketPath(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	_ = os.Remove(path)
	return nil
}

func applyMode(path string, modeStr string) error {
	mode := os.FileMode(0666)

	if strings.TrimSpace(modeStr) != "" {
		parsed, err := strconv.ParseUint(modeStr, 8, 32)
		if err != nil {
			return fmt.Errorf("invalid SocketMode: %w", err)
		}
		mode = os.FileMode(parsed)
	}

	return os.Chmod(path, mode)
}
