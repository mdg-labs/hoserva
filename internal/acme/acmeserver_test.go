package acme

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// testDirectory is an in-process RFC 8555 ACME server that offers only
// DNS-01 and auto-validates it. Tests never contact Let's Encrypt.
type testDirectory struct {
	t      *testing.T
	server *httptest.Server
	key    *ecdsa.PrivateKey
	ca     *x509.Certificate
	caDER  []byte

	forceHTTP01 bool

	mu     sync.Mutex
	nonces int
	authz  []*testAuthz
	orders []*testOrder
}

type testAuthz struct {
	id     int
	domain string
	status string
	token  string
}

type testOrder struct {
	id     int
	status string
	authz  []int
	leaf   []byte
}

func startTestDirectory(t *testing.T) *testDirectory {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-acme-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	td := &testDirectory{t: t, key: key, ca: ca, caDER: der}
	td.server = httptest.NewServer(http.HandlerFunc(td.handle))
	t.Cleanup(td.server.Close)
	return td
}

func (td *testDirectory) URL() string { return td.server.URL + "/dir" }

func (td *testDirectory) handle(w http.ResponseWriter, r *http.Request) {
	td.mu.Lock()
	td.nonces++
	nonce := fmt.Sprintf("nonce-%d", td.nonces)
	td.mu.Unlock()
	w.Header().Set("Replay-Nonce", nonce)
	w.Header().Set("Cache-Control", "no-store")

	switch {
	case r.URL.Path == "/dir":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"newNonce":   td.server.URL + "/new-nonce",
			"newAccount": td.server.URL + "/new-account",
			"newOrder":   td.server.URL + "/new-order",
			"revokeCert": td.server.URL + "/revoke",
		})
	case r.URL.Path == "/new-nonce":
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/new-account":
		w.Header().Set("Location", td.server.URL+"/acct/1")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"status":"valid"}`))
	case r.URL.Path == "/new-order":
		var req struct {
			Identifiers []struct{ Value string }
		}
		if err := decodeJWSPayload(r.Body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		td.mu.Lock()
		o := &testOrder{id: len(td.orders), status: "pending"}
		for _, id := range req.Identifiers {
			a := &testAuthz{
				id:     len(td.authz),
				domain: id.Value,
				status: "pending",
				token:  fmt.Sprintf("token-%d", len(td.authz)),
			}
			td.authz = append(td.authz, a)
			o.authz = append(o.authz, a.id)
		}
		td.orders = append(td.orders, o)
		td.mu.Unlock()
		w.Header().Set("Location", fmt.Sprintf("%s/order/%d", td.server.URL, o.id))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(td.orderJSON(o))
	case strings.HasPrefix(r.URL.Path, "/order/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/order/"))
		td.mu.Lock()
		o := td.orderByID(id)
		td.refreshOrder(o)
		td.mu.Unlock()
		if o == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("%s/order/%d", td.server.URL, o.id))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(td.orderJSON(o))
	case strings.HasPrefix(r.URL.Path, "/finalize/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/finalize/"))
		var req struct {
			CSR string `json:"csr"`
		}
		if err := decodeJWSPayload(r.Body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		csrDER, err := base64.RawURLEncoding.DecodeString(req.CSR)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		csr, err := x509.ParseCertificateRequest(csrDER)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		td.mu.Lock()
		o := td.orderByID(id)
		if o == nil || o.status != "ready" {
			td.mu.Unlock()
			http.Error(w, "order not ready", http.StatusForbidden)
			return
		}
		leaf := &x509.Certificate{
			SerialNumber:          big.NewInt(int64(id + 2)),
			Subject:               pkix.Name{CommonName: csr.Subject.CommonName},
			NotBefore:             time.Now().Add(-time.Hour),
			NotAfter:              time.Now().Add(90 * 24 * time.Hour),
			KeyUsage:              x509.KeyUsageDigitalSignature,
			ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			DNSNames:              csr.DNSNames,
			BasicConstraintsValid: true,
		}
		if len(leaf.DNSNames) == 0 {
			leaf.DNSNames = []string{csr.Subject.CommonName}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, td.ca, csr.PublicKey, td.key)
		if err != nil {
			td.mu.Unlock()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		o.leaf = der
		o.status = "valid"
		td.mu.Unlock()
		w.Header().Set("Location", fmt.Sprintf("%s/order/%d", td.server.URL, o.id))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(td.orderJSON(o))
	case strings.HasPrefix(r.URL.Path, "/cert/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/cert/"))
		td.mu.Lock()
		o := td.orderByID(id)
		td.mu.Unlock()
		if o == nil || len(o.leaf) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/pem-certificate-chain")
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: o.leaf})
		_ = pem.Encode(w, &pem.Block{Type: "CERTIFICATE", Bytes: td.caDER})
	case strings.HasPrefix(r.URL.Path, "/authz/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/authz/"))
		td.mu.Lock()
		a := td.authzByID(id)
		td.mu.Unlock()
		if a == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Location", fmt.Sprintf("%s/authz/%d", td.server.URL, a.id))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(td.authzJSON(a))
	case strings.HasPrefix(r.URL.Path, "/chal/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/chal/"))
		td.mu.Lock()
		a := td.authzByID(id)
		if a != nil {
			a.status = "valid"
		}
		td.mu.Unlock()
		if a == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"type":   "dns-01",
			"url":    fmt.Sprintf("%s/chal/%d", td.server.URL, a.id),
			"status": "valid",
			"token":  a.token,
		})
	default:
		http.NotFound(w, r)
	}
}

func (td *testDirectory) orderByID(id int) *testOrder {
	if id < 0 || id >= len(td.orders) {
		return nil
	}
	return td.orders[id]
}

func (td *testDirectory) authzByID(id int) *testAuthz {
	if id < 0 || id >= len(td.authz) {
		return nil
	}
	return td.authz[id]
}

func (td *testDirectory) refreshOrder(o *testOrder) {
	if o == nil || o.status == "valid" {
		return
	}
	ready := true
	for _, id := range o.authz {
		if td.authz[id].status != "valid" {
			ready = false
			break
		}
	}
	if ready {
		o.status = "ready"
	}
}

func (td *testDirectory) orderJSON(o *testOrder) map[string]any {
	authz := make([]string, 0, len(o.authz))
	for _, id := range o.authz {
		authz = append(authz, fmt.Sprintf("%s/authz/%d", td.server.URL, id))
	}
	m := map[string]any{
		"status":         o.status,
		"authorizations": authz,
		"finalize":       fmt.Sprintf("%s/finalize/%d", td.server.URL, o.id),
	}
	if o.status == "valid" {
		m["certificate"] = fmt.Sprintf("%s/cert/%d", td.server.URL, o.id)
	}
	return m
}

func (td *testDirectory) authzJSON(a *testAuthz) map[string]any {
	chalType := "dns-01"
	if td.forceHTTP01 {
		chalType = "http-01"
	}
	return map[string]any{
		"status": a.status,
		"identifier": map[string]string{
			"type":  "dns",
			"value": a.domain,
		},
		"challenges": []map[string]string{{
			"type":   chalType,
			"url":    fmt.Sprintf("%s/chal/%d", td.server.URL, a.id),
			"uri":    fmt.Sprintf("%s/chal/%d", td.server.URL, a.id),
			"status": a.status,
			"token":  a.token,
		}},
	}
}

func decodeJWSPayload(r io.Reader, v any) error {
	var req struct{ Payload string }
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return err
	}
	if req.Payload == "" {
		return nil
	}
	payload, err := base64.RawURLEncoding.DecodeString(req.Payload)
	if err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	return json.Unmarshal(payload, v)
}
