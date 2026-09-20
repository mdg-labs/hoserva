package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/mdg-labs/hoserva/internal/api"
	cfggen "github.com/mdg-labs/hoserva/internal/config"
)

type persistedHTTPS struct {
	AllowAllSources bool `json:"allowAllSources"`
	ListenPort      int  `json:"listenPort"`
}

type httpsControl struct {
	stateDir string
	certPath string
	keyPath  string
	filter   *sourceFilteringListener
	certMu   sync.RWMutex
	cert     tls.Certificate
	livePort int
	wantPort atomic.Int64
	allowAll atomic.Bool
}

func newHTTPSControl(stateDir, certPath, keyPath string, filter *sourceFilteringListener, livePort int, allowAll bool, cert tls.Certificate) *httpsControl {
	h := &httpsControl{
		stateDir: stateDir,
		certPath: certPath,
		keyPath:  keyPath,
		filter:   filter,
		cert:     cert,
		livePort: livePort,
	}
	h.wantPort.Store(int64(livePort))
	h.allowAll.Store(allowAll)
	return h
}

func (h *httpsControl) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	h.certMu.RLock()
	defer h.certMu.RUnlock()
	c := h.cert
	return &c, nil
}

func (h *httpsControl) Certificate() (api.TLSCertView, error) {
	h.certMu.RLock()
	cert := h.cert
	h.certMu.RUnlock()
	if len(cert.Certificate) == 0 {
		return api.TLSCertView{}, fmt.Errorf("no TLS certificate loaded")
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return api.TLSCertView{}, err
	}
	return api.TLSCertView{NotAfter: parsed.NotAfter}, nil
}

func (h *httpsControl) Regenerate(context.Context) (api.TLSCertView, error) {
	if err := os.Remove(h.certPath); err != nil && !os.IsNotExist(err) {
		return api.TLSCertView{}, err
	}
	if err := os.Remove(h.keyPath); err != nil && !os.IsNotExist(err) {
		return api.TLSCertView{}, err
	}
	cert, err := loadOrGenerateTLSCertificate(h.certPath, h.keyPath)
	if err != nil {
		return api.TLSCertView{}, err
	}
	h.certMu.Lock()
	h.cert = cert
	h.certMu.Unlock()
	return h.Certificate()
}

func (h *httpsControl) AllowAllSources() bool {
	return h.allowAll.Load()
}

func (h *httpsControl) SetAllowAllSources(v bool) error {
	h.allowAll.Store(v)
	if h.filter != nil {
		h.filter.SetAllowAll(v)
	}
	return h.persist()
}

func (h *httpsControl) ListenPort() int {
	return int(h.wantPort.Load())
}

func (h *httpsControl) SetListenPort(port int) error {
	h.wantPort.Store(int64(port))
	return h.persist()
}

func (h *httpsControl) ListenPortRestartRequired() bool {
	return int(h.wantPort.Load()) != h.livePort
}

func (h *httpsControl) persistPath() string {
	return filepath.Join(h.stateDir, "https-settings.json")
}

func (h *httpsControl) persist() error {
	raw, err := json.Marshal(persistedHTTPS{
		AllowAllSources: h.allowAll.Load(),
		ListenPort:      int(h.wantPort.Load()),
	})
	if err != nil {
		return err
	}
	path := h.persistPath()
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".https-settings.json.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpPath, path)
}

func loadPersistedHTTPS(stateDir string) (persistedHTTPS, bool) {
	raw, err := os.ReadFile(filepath.Join(stateDir, "https-settings.json"))
	if err != nil {
		return persistedHTTPS{}, false
	}
	var p persistedHTTPS
	if err := json.Unmarshal(raw, &p); err != nil {
		return persistedHTTPS{}, false
	}
	return p, true
}

func applyPersistedListenPort(tcpAddr string, port int) string {
	host, _, err := net.SplitHostPort(tcpAddr)
	if err != nil {
		return net.JoinHostPort("", strconv.Itoa(port))
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func listenPortOf(tcpAddr string) int {
	_, port, err := net.SplitHostPort(tcpAddr)
	if err != nil {
		return 8008
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		return 8008
	}
	return n
}

// rootedIfupdown runs ifup/ifdown only when the generator root is the
// guest's real /etc — a --dev run writes under a workspace-local root
// and must never change the development host's live network.
type rootedIfupdown struct {
	root  string
	inner cfggen.IfupdownRunner
}

func (r rootedIfupdown) Apply(ctx context.Context, iface string) error {
	if filepath.Clean(r.root) != "/etc" {
		log.Printf("hoservad: skipping ifupdown apply for %s because config-root %s is not /etc", iface, r.root)
		return nil
	}
	return r.inner.Apply(ctx, iface)
}
