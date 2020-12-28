package proxy

import (
	"github.com/rueian/zenvoy/pkg/logger"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

type suite struct {
	store    *store
	server   *Server
	serverLn net.Listener
	targetLn net.Listener
}

func (s *suite) Close() {
	s.serverLn.Close()
	s.targetLn.Close()
}

func setup(t *testing.T) *suite {
	store := NewStore()
	server := NewServer(&logger.Std{}, store)

	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	go server.Serve(ln1)
	go http.Serve(ln2, http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	return &suite{
		store:    store,
		server:   server,
		serverLn: ln1,
		targetLn: ln2,
	}
}

func TestDirectClose(t *testing.T) {
	suite := setup(t)
	defer suite.Close()

	resp, err := http.Get("http://" + suite.serverLn.Addr().String())
	if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "connection reset by peer") {
		t.Error(resp, err)
	}
}

func TestDirectRedirect(t *testing.T) {
	suite := setup(t)
	defer suite.Close()

	suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
		suite.serverLn.Addr().String(),
		suite.targetLn.Addr().String(),
	})

	resp, err := http.Get("http://" + suite.serverLn.Addr().String())
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Error(resp, err)
	}
}

func TestPendingRedirect(t *testing.T) {
	suite := setup(t)
	defer suite.Close()

	suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
		suite.serverLn.Addr().String(),
	})
	go func() {
		time.Sleep(time.Second / 2)
		suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
			suite.serverLn.Addr().String(),
			suite.targetLn.Addr().String(),
		})
	}()

	resp, err := http.Get("http://" + suite.serverLn.Addr().String())
	if err != nil || resp.StatusCode != http.StatusNoContent {
		t.Error(resp, err)
	}
}

func TestPendingClose(t *testing.T) {
	suite := setup(t)
	defer suite.Close()

	suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
		suite.serverLn.Addr().String(),
	})
	go func() {
		time.Sleep(time.Second / 2)
		suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{})
	}()

	resp, err := http.Get("http://" + suite.serverLn.Addr().String())
	if !strings.Contains(err.Error(), "EOF") && !strings.Contains(err.Error(), "connection reset by peer") {
		t.Error(resp, err)
	}
}

func TestDynamicRedirect(t *testing.T) {
	suite := setup(t)
	defer suite.Close()

	suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
		suite.serverLn.Addr().String(),
	})

	stop := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
				suite.serverLn.Addr().String(),
			})
			time.Sleep(time.Second / 10)
			suite.store.SetIntendedEndpoints(port(suite.serverLn.Addr()), []string{
				suite.serverLn.Addr().String(),
				suite.targetLn.Addr().String(),
			})
			time.Sleep(time.Second / 10)
		}
		close(stop)
	}()

loop:
	for i := 0; ; i++ {
		select {
		case <-stop:
			if i == 0 {
				t.Fatal("no request succeeded")
			}
			break loop
		default:
			resp, err := http.Get("http://" + suite.serverLn.Addr().String())
			if err != nil || resp.StatusCode != http.StatusNoContent {
				t.Error(resp, err)
			}
		}
	}
}
