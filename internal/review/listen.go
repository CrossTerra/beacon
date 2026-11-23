package review

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/CrossTerra/beacon/internal/approval"
)

// ListenLoopback binds both literal loopback addresses at the fixed review port, in either origin
// mode. Fail rather than publishing only one listener or falling back to another port.
func ListenLoopback() ([]net.Listener, error) { return listenLoopback(approval.ReviewPort) }

func listenLoopback(port string) ([]net.Listener, error) {
	var listeners []net.Listener
	for _, target := range []struct{ network, ip string }{{"tcp4", "127.0.0.1"}, {"tcp6", "::1"}} {
		address := net.JoinHostPort(target.ip, port)
		l, err := net.Listen(target.network, address)
		if err != nil {
			for _, bound := range listeners {
				bound.Close()
			}
			return nil, fmt.Errorf("listen for review on %s: %w", address, err)
		}
		listeners = append(listeners, l)
	}
	return listeners, nil
}

func (s *Site) Server() *http.Server {
	return &http.Server{Handler: s, ErrorLog: log.New(io.Discard, "", 0), MaxHeaderBytes: 16 << 10,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second,
		IdleTimeout: 30 * time.Second}
}
