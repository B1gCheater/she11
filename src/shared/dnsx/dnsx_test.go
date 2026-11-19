package dnsx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func requireLoopback(t *testing.T, ips ...[]net.IP) {
	t.Helper()
	for _, group := range ips {
		for _, ip := range group {
			if ip.IsLoopback() {
				return
			}
		}
	}
	t.Fatal("system DNS returned no localhost loopback address")
}

func TestSystemDNSIsDefaultWithoutCustomResolvers(t *testing.T) {
	r := &Resolver{Timeout: 2 * time.Second} // fallback intentionally disabled
	v4, v6, err := r.Lookup(context.Background(), "localhost")
	if err != nil {
		t.Fatal(err)
	}
	requireLoopback(t, v4, v6)
}

func TestCustomResolverFailureRequiresExplicitFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	for _, allow := range []bool{false, true} {
		r := &Resolver{DoH: []string{server.URL}, Timeout: 2 * time.Second, AllowSystem: allow}
		v4, v6, err := r.Lookup(context.Background(), "localhost")
		if !allow {
			if err == nil {
				t.Fatal("custom resolver failure unexpectedly fell back to system DNS")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		requireLoopback(t, v4, v6)
	}
}
