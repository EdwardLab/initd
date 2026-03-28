package notify

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Server struct {
	Path  string
	Conn  *net.UnixConn
	Ready chan struct{}

	once sync.Once
}

func Start() (*Server, error) {
	name := "initd-notify-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	abstractAddr := &net.UnixAddr{Name: "\x00" + name, Net: "unixgram"}
	if conn, err := net.ListenUnixgram("unixgram", abstractAddr); err == nil {
		s := &Server{
			Path:  "@" + name,
			Conn:  conn,
			Ready: make(chan struct{}),
		}
		go s.listen()
		return s, nil
	}

	path := filepath.Join("/run/initd", fmt.Sprintf("notify-%d.sock", time.Now().UnixNano()))
	_ = os.Remove(path)
	_ = os.MkdirAll(filepath.Dir(path), 0o755)

	addr := &net.UnixAddr{Name: path, Net: "unixgram"}
	conn, err := net.ListenUnixgram("unixgram", addr)
	if err != nil {
		return nil, err
	}

	s := &Server{
		Path:  path,
		Conn:  conn,
		Ready: make(chan struct{}),
	}

	go s.listen()
	return s, nil
}

func (s *Server) listen() {
	buf := make([]byte, 4096)
	for {
		n, _, err := s.Conn.ReadFromUnix(buf)
		if err != nil {
			return
		}
		msg := string(buf[:n])
		if strings.Contains(msg, "READY=1") {
			s.once.Do(func() { close(s.Ready) })
			return
		}
	}
}

func (s *Server) Stop() {
	if s.Conn != nil {
		_ = s.Conn.Close()
	}
	if s.Path != "" && !strings.HasPrefix(s.Path, "@") {
		_ = os.Remove(s.Path)
	}
}
