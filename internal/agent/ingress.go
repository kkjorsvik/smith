package agent

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/kkjorsvik/smith/internal/types"
)

// ingressProxy terminates TLS with the wildcard cert and reverse-proxies
// requests by Host to the target service's ClusterIP. Cert and routes are
// swapped atomically as the agent pulls updates from the control plane.
type ingressProxy struct {
	mu     sync.RWMutex
	cert   *tls.Certificate
	routes map[string]*httputil.ReverseProxy // host -> backend proxy
}

func newIngressProxy() *ingressProxy {
	return &ingressProxy{routes: make(map[string]*httputil.ReverseProxy)}
}

func (p *ingressProxy) setCert(cert tls.Certificate) {
	p.mu.Lock()
	p.cert = &cert
	p.mu.Unlock()
}

// getCertificate feeds the TLS server so the cert can be swapped on renewal
// without restarting the listener.
func (p *ingressProxy) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cert == nil {
		return nil, fmt.Errorf("no ingress cert")
	}
	return p.cert, nil
}

// setRules rebuilds the host -> backend map from the resolved ingress rules.
func (p *ingressProxy) setRules(rules []types.IngressRule) {
	m := make(map[string]*httputil.ReverseProxy, len(rules))
	for _, r := range rules {
		target := &url.URL{Scheme: "http", Host: fmt.Sprintf("%s:%d", r.ClusterIP, r.Port)}
		m[strings.ToLower(r.Host)] = backendProxy(target)
	}
	p.mu.Lock()
	p.routes = m
	p.mu.Unlock()
}

// backendProxy returns a reverse proxy to target. It preserves the original
// Host (so vhost-aware apps like forgejo see their own domain) and sets the
// forwarded headers real apps need to generate correct HTTPS URLs. The backend
// speaks plain HTTP (TLS terminates here); WebSocket upgrades pass through.
func backendProxy(target *url.URL) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.Host = pr.In.Host
			pr.SetXForwarded()
			pr.Out.Header.Set("X-Forwarded-Proto", "https")
		},
	}
}

func (p *ingressProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := strings.ToLower(r.Host)
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	p.mu.RLock()
	rp := p.routes[host]
	p.mu.RUnlock()
	if rp == nil {
		http.Error(w, "no ingress for "+host, http.StatusNotFound)
		return
	}
	rp.ServeHTTP(w, r)
}

// startIngress fetches the wildcard cert and initial rules, then starts the
// :443 proxy and :80 redirect plus the ingress-sync loop. Best-effort: if the
// cert isn't available (control plane hasn't provisioned it), ingress is
// skipped and the agent runs without it.
func (a *Agent) startIngress() {
	cert, err := a.fetchIngressCert()
	if err != nil {
		log.Printf("agent: ingress cert unavailable, ingress disabled: %v", err)
		return
	}

	a.ingress = newIngressProxy()
	a.ingress.setCert(cert)
	if rules, err := a.fetchIngresses(); err != nil {
		log.Printf("agent: initial ingress rules: %v", err)
	} else {
		a.ingress.setRules(rules)
	}

	a.serveIngress()
	go a.ingressSyncLoop()
	log.Printf("agent: ingress proxy listening on :443 (:80 redirect)")
}

// serveIngress runs the HTTPS proxy on :443 and an HTTP :80 -> :443 redirect.
func (a *Agent) serveIngress() {
	https := &http.Server{
		Addr:    ":443",
		Handler: a.ingress,
		TLSConfig: &tls.Config{
			GetCertificate: a.ingress.getCertificate,
			MinVersion:     tls.VersionTLS12,
		},
	}
	go func() {
		if err := https.ListenAndServeTLS("", ""); err != nil {
			log.Printf("agent: ingress https server: %v", err)
		}
	}()

	redirect := &http.Server{
		Addr: ":80",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "https://"+r.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
		}),
	}
	go func() {
		if err := redirect.ListenAndServe(); err != nil {
			log.Printf("agent: ingress http redirect server: %v", err)
		}
	}()
}

// ingressSyncLoop refreshes the ingress rules (and the wildcard cert, to pick
// up renewals) on the heartbeat cadence.
func (a *Agent) ingressSyncLoop() {
	ticker := time.NewTicker(heartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if rules, err := a.fetchIngresses(); err != nil {
				log.Printf("agent: fetch ingresses: %v", err)
			} else {
				a.ingress.setRules(rules)
			}
			if cert, err := a.fetchIngressCert(); err == nil {
				a.ingress.setCert(cert)
			}
		case <-a.stop:
			return
		}
	}
}

// fetchIngressCert pulls the wildcard cert+key from the control plane.
func (a *Agent) fetchIngressCert() (tls.Certificate, error) {
	url := fmt.Sprintf("https://%s/ingress/cert", a.serverAddr)
	resp, err := a.httpClient.Get(url)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("get ingress cert: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return tls.Certificate{}, fmt.Errorf("ingress cert: status %d", resp.StatusCode)
	}

	var bundle types.CertBundle
	if err := json.NewDecoder(resp.Body).Decode(&bundle); err != nil {
		return tls.Certificate{}, fmt.Errorf("decode cert bundle: %w", err)
	}
	cert, err := tls.X509KeyPair([]byte(bundle.CertPEM), []byte(bundle.KeyPEM))
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("parse wildcard cert: %w", err)
	}
	return cert, nil
}

// fetchIngresses pulls this node's resolved ingress rules from the control plane.
func (a *Agent) fetchIngresses() ([]types.IngressRule, error) {
	url := fmt.Sprintf("https://%s/nodes/%s/ingresses", a.serverAddr, a.id)
	resp, err := a.httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("get ingresses: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("ingresses failed: status %d", resp.StatusCode)
	}

	var rules []types.IngressRule
	if err := json.NewDecoder(resp.Body).Decode(&rules); err != nil {
		return nil, fmt.Errorf("decode ingresses: %w", err)
	}
	return rules, nil
}
