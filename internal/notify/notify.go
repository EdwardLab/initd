package notify

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

type Server struct {
	Path  string
	Conn  *net.UnixConn
	Ready chan struct{}

	once sync.Once
}

func Start() (*Server, error) {
	path := "/run/initd/notify.sock"

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
	if s.Path != "" {
		_ = os.Remove(s.Path)
	}
}
