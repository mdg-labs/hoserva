package acme

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const cloudflareAPI = "https://api.cloudflare.com/client/v4"

// CloudflareSolver presents DNS-01 TXT records through Cloudflare's API.
// HTTP is injectable so tests use httptest and never contact Cloudflare.
type CloudflareSolver struct {
	APIToken   string
	HTTPClient *http.Client
	APIBase    string

	mu        sync.Mutex
	recordIDs map[string]recordRef
}

type recordRef struct {
	zoneID   string
	recordID string
}

func (s *CloudflareSolver) client() *http.Client {
	if s.HTTPClient != nil {
		return s.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (s *CloudflareSolver) base() string {
	if s.APIBase != "" {
		return strings.TrimSuffix(s.APIBase, "/")
	}
	return cloudflareAPI
}

func (s *CloudflareSolver) Present(ctx context.Context, fqdn, value string) error {
	zoneID, err := s.findZone(ctx, fqdn)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{
		"type":    "TXT",
		"name":    strings.TrimSuffix(fqdn, "."),
		"content": value,
		"ttl":     120,
	})
	if err != nil {
		return fmt.Errorf("acme: encoding Cloudflare DNS record: %w", err)
	}
	var out struct {
		Success bool `json:"success"`
		Result  struct {
			ID string `json:"id"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := s.doJSON(ctx, http.MethodPost, s.base()+"/zones/"+zoneID+"/dns_records", bytes.NewReader(body), &out); err != nil {
		return err
	}
	if !out.Success || out.Result.ID == "" {
		return fmt.Errorf("acme: Cloudflare create record failed: %s", cloudflareError(out.Errors))
	}
	s.mu.Lock()
	if s.recordIDs == nil {
		s.recordIDs = make(map[string]recordRef)
	}
	s.recordIDs[fqdn+"|"+value] = recordRef{zoneID: zoneID, recordID: out.Result.ID}
	s.mu.Unlock()
	return nil
}

func (s *CloudflareSolver) CleanUp(ctx context.Context, fqdn, value string) error {
	s.mu.Lock()
	ref, ok := s.recordIDs[fqdn+"|"+value]
	if ok {
		delete(s.recordIDs, fqdn+"|"+value)
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	var out struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := s.doJSON(ctx, http.MethodDelete, s.base()+"/zones/"+ref.zoneID+"/dns_records/"+ref.recordID, nil, &out); err != nil {
		return err
	}
	if !out.Success {
		return fmt.Errorf("acme: Cloudflare delete record failed: %s", cloudflareError(out.Errors))
	}
	return nil
}

func (s *CloudflareSolver) findZone(ctx context.Context, fqdn string) (string, error) {
	name := strings.TrimPrefix(strings.TrimSuffix(fqdn, "."), "_acme-challenge.")
	labels := strings.Split(name, ".")
	for i := 0; i < len(labels)-1; i++ {
		zone := strings.Join(labels[i:], ".")
		id, err := s.lookupZone(ctx, zone)
		if err != nil {
			return "", err
		}
		if id != "" {
			return id, nil
		}
	}
	return "", fmt.Errorf("acme: no Cloudflare zone for %s", name)
}

func (s *CloudflareSolver) lookupZone(ctx context.Context, name string) (string, error) {
	u, err := url.Parse(s.base() + "/zones")
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("name", name)
	u.RawQuery = q.Encode()
	var out struct {
		Success bool `json:"success"`
		Result  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := s.doJSON(ctx, http.MethodGet, u.String(), nil, &out); err != nil {
		return "", err
	}
	if !out.Success {
		return "", fmt.Errorf("acme: Cloudflare list zones failed: %s", cloudflareError(out.Errors))
	}
	for _, z := range out.Result {
		if strings.EqualFold(z.Name, name) {
			return z.ID, nil
		}
	}
	return "", nil
}

func (s *CloudflareSolver) doJSON(ctx context.Context, method, rawURL string, body io.Reader, dest any) error {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.APIToken)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client().Do(req)
	if err != nil {
		return fmt.Errorf("acme: Cloudflare API: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("acme: reading Cloudflare response: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("acme: Cloudflare API status %d", resp.StatusCode)
	}
	if dest == nil {
		return nil
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return fmt.Errorf("acme: decoding Cloudflare response: %w", err)
	}
	return nil
}

func cloudflareError(errs []struct {
	Message string `json:"message"`
}) string {
	if len(errs) == 0 {
		return "unknown error"
	}
	return errs[0].Message
}
