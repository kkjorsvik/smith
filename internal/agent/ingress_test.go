package agent

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/kkjorsvik/smith/internal/types"
)

func TestIngressProxyRouting(t *testing.T) {
	// Backend echoes the Host it received and the X-Forwarded-Proto header.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "host=%s xfp=%s", r.Host, r.Header.Get("X-Forwarded-Proto"))
	}))
	defer backend.Close()

	u, _ := url.Parse(backend.URL)
	host, portStr, _ := net.SplitHostPort(u.Host)
	port, _ := strconv.Atoi(portStr)

	p := newIngressProxy()
	p.setRules([]types.IngressRule{{Host: "git.kkjorsvik.com", ClusterIP: host, Port: port}})

	// Matching host is proxied to the backend; original Host preserved and
	// X-Forwarded-Proto set to https.
	req := httptest.NewRequest(http.MethodGet, "http://git.kkjorsvik.com/", nil)
	req.Host = "git.kkjorsvik.com"
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("matching host: status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "host=git.kkjorsvik.com") {
		t.Fatalf("original Host not preserved: %q", body)
	}
	if !strings.Contains(body, "xfp=https") {
		t.Fatalf("X-Forwarded-Proto not set to https: %q", body)
	}

	// Host:port in the header still matches the rule (port stripped).
	req2 := httptest.NewRequest(http.MethodGet, "http://git.kkjorsvik.com/", nil)
	req2.Host = "git.kkjorsvik.com:443"
	rec2 := httptest.NewRecorder()
	p.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("host:port did not match rule: status %d", rec2.Code)
	}

	// Unknown host -> 404.
	req3 := httptest.NewRequest(http.MethodGet, "http://nope.kkjorsvik.com/", nil)
	req3.Host = "nope.kkjorsvik.com"
	rec3 := httptest.NewRecorder()
	p.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotFound {
		t.Fatalf("unknown host: status %d, want 404", rec3.Code)
	}
}
