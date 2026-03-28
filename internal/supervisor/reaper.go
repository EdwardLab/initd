package supervisor

import (
	"os"
	"os/signal"
	"sync"
	"syscall"
)

type ProcessReaper struct {
	mu       sync.Mutex
	handlers map[int]func(syscall.WaitStatus)
}

func NewProcessReaper() *ProcessReaper {
	return &ProcessReaper{
		handlers: make(map[int]func(syscall.WaitStatus)),
	}
}

func (r *ProcessReaper) Register(pid int, handler func(syscall.WaitStatus)) {
	if pid <= 0 || handler == nil {
		return
	}
	r.mu.Lock()
	r.handlers[pid] = handler
	r.mu.Unlock()

	var status syscall.WaitStatus
	if reaped, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); err == nil && reaped == pid {
		r.handleExit(pid, status)
	}
}

func (r *ProcessReaper) Start() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGCHLD)
	go func() {
		for range ch {
			for {
				var status syscall.WaitStatus
				pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
				if pid <= 0 {
					if err == syscall.EINTR {
						continue
					}
					break
				}
				r.handleExit(pid, status)
			}
		}
	}()
}

func (r *ProcessReaper) handleExit(pid int, status syscall.WaitStatus) {
	r.mu.Lock()
	handler := r.handlers[pid]
	delete(r.handlers, pid)
	r.mu.Unlock()
	if handler != nil {
		go handler(status)
	}
}
