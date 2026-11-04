package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"time"
)

type mockAuthServerRegistry struct {
	mu       sync.RWMutex
	handlers map[string]http.Handler
	seq      int64
}

var testAuthServers = &mockAuthServerRegistry{
	handlers: make(map[string]http.Handler),
}

func (r *mockAuthServerRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	r.mu.RLock()
	key := req.URL.Scheme + "://" + req.URL.Host
	h, ok := r.handlers[key]
	if !ok {
		h, ok = r.handlers[req.URL.Host]
	}
	r.mu.RUnlock()

	if !ok {
		return http.DefaultTransport.RoundTrip(req)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	resp := rec.Result()
	resp.Request = req
	return resp, nil
}

type inMemoryAuthTestServer struct {
	URL string
}

func newAuthTestServer(handler http.Handler) *inMemoryAuthTestServer {
	id := atomic.AddInt64(&testAuthServers.seq, 1)
	u := fmt.Sprintf("http://auth-node-%d.local", id)
	testAuthServers.mu.Lock()
	testAuthServers.handlers[u] = handler
	testAuthServers.mu.Unlock()
	return &inMemoryAuthTestServer{URL: u}
}

func (s *inMemoryAuthTestServer) Close() {
	testAuthServers.mu.Lock()
	delete(testAuthServers.handlers, s.URL)
	testAuthServers.mu.Unlock()
}

func init() {
	newAuthHTTPClient = func(timeout time.Duration) *http.Client {
		return &http.Client{
			Timeout:   timeout,
			Transport: testAuthServers,
		}
	}
}
