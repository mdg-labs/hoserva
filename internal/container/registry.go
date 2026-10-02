package container

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/distribution/reference"
)

// Registry failures a caller tells apart: a rate limit is skipped until the
// next check (Q81), a missing manifest or a refused login is that image's
// own result. Anything else is a failed request.
var (
	ErrRateLimited      = errors.New("container: the registry is rate limiting requests")
	ErrManifestNotFound = errors.New("container: the registry has no such manifest")
	ErrRegistryDenied   = errors.New("container: the registry refused the request")
)

// ImageRef names one tag of one repository on one registry, normalised the
// way the Engine does it: a bare "nginx" is docker.io's library/nginx.
type ImageRef struct {
	Registry   string // "docker.io", "ghcr.io", "registry.example.com:5000"
	Repository string // "library/nginx"
	Tag        string // "latest" when the reference names none
}

// ParseImageRef parses a container's repository and tag as Container.Image
// and Container.Tag report them.
func ParseImageRef(image, tag string) (ImageRef, error) {
	if tag == "" {
		tag = "latest"
	}
	named, err := reference.ParseNormalizedNamed(image)
	if err != nil {
		return ImageRef{}, fmt.Errorf("parsing image %q: %w", image, err)
	}
	return ImageRef{Registry: reference.Domain(named), Repository: reference.Path(named), Tag: tag}, nil
}

// String is the key a container's image goes by in the update check:
// "repository:tag" in the familiar form, so "library/nginx" on Docker Hub
// reads "nginx:latest".
func (r ImageRef) String() string {
	name := r.Repository
	switch {
	case r.Registry == "docker.io" && strings.HasPrefix(name, "library/"):
		name = strings.TrimPrefix(name, "library/")
	case r.Registry != "docker.io":
		name = r.Registry + "/" + name
	}
	return name + ":" + r.Tag
}

// RegistryClient is the registry access the update check sits behind: it
// reads manifests and tag names, never a layer, so nothing is pulled (Q81).
// A registry that wants a login and has no saved credential answers
// ErrRegistryDenied; one whose saved credential cannot be used or is refused
// answers ErrCredentialUnusable or ErrCredentialRejected. The real client is
// HTTPRegistry; tests use FakeRegistry.
type RegistryClient interface {
	// ManifestDigest returns the digest the registry serves for ref's tag,
	// from a manifest HEAD. It returns ErrRateLimited for a registry that
	// is limiting requests and ErrManifestNotFound for an unknown tag.
	ManifestDigest(ctx context.Context, ref ImageRef) (string, error)
	// Tags returns every tag of ref's repository, or an error: a listing
	// that was cut off is never returned as if it were complete.
	Tags(ctx context.Context, ref ImageRef) ([]string, error)
}

const (
	registryTimeout = 30 * time.Second
	maxTagPages     = 100
	maxBodyBytes    = 4 << 20
	manifestAccept  = "application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json"
)

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// HTTPRegistry is the real RegistryClient, speaking the Docker Registry
// HTTP API v2 with the anonymous bearer token every public registry hands
// out. A registry that has a credential in Credentials is logged in to
// instead, and only over https: the credential goes to the registry's own
// host, and to its token service only when that is the same host (or Docker
// Hub's auth.docker.io), never to another host or over plain HTTP, and a
// credential that cannot be used is never replaced by an anonymous request.
type HTTPRegistry struct {
	Client      *http.Client
	Credentials CredentialSource
}

var _ RegistryClient = (*HTTPRegistry)(nil)

func (h *HTTPRegistry) client() *http.Client {
	c := &http.Client{Timeout: registryTimeout}
	if h.Client != nil {
		copied := *h.Client
		c = &copied
	}
	parent := c.CheckRedirect
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if err := sameOrigin(req.URL, via[len(via)-1].URL); err != nil {
			return err
		}
		if parent != nil {
			return parent(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return c
}

// sameOrigin refuses a redirect to another scheme, host or port. Go keeps
// the Authorization header on a redirect to the same hostname whatever the
// scheme or port, so the bearer token would otherwise follow a redirect to
// plain HTTP or to another service on the same host.
func sameOrigin(to, from *url.URL) error {
	if to.Scheme != from.Scheme || to.Host != from.Host {
		return fmt.Errorf("the registry redirected %s to %s://%s, which is not the same scheme, host and port", from.Host, to.Scheme, to.Host)
	}
	return nil
}

func (h *HTTPRegistry) ManifestDigest(ctx context.Context, ref ImageRef) (string, error) {
	u := registryURL(ref.Registry) + "/v2/" + ref.Repository + "/manifests/" + url.PathEscape(ref.Tag)
	resp, err := h.do(ctx, ref.Registry, http.MethodHead, u, manifestAccept)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	digest := resp.Header.Get("Docker-Content-Digest")
	if !digestPattern.MatchString(digest) {
		return "", fmt.Errorf("registry %s sent no usable manifest digest for %s", ref.Registry, ref)
	}
	return digest, nil
}

func (h *HTTPRegistry) Tags(ctx context.Context, ref ImageRef) ([]string, error) {
	next := registryURL(ref.Registry) + "/v2/" + ref.Repository + "/tags/list?n=1000"
	var tags []string
	for page := 0; next != ""; page++ {
		if page == maxTagPages {
			return nil, fmt.Errorf("the tag list of %s has more than %d pages, so newer tags may be missing", ref.Repository, maxTagPages)
		}
		resp, err := h.do(ctx, ref.Registry, http.MethodGet, next, "application/json")
		if err != nil {
			return nil, err
		}
		var body struct {
			Tags []string `json:"tags"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("reading the tag list of %s: %w", ref.Repository, err)
		}
		tags = append(tags, body.Tags...)
		next, err = nextPage(resp)
		if err != nil {
			return nil, err
		}
	}
	return tags, nil
}

// nextPage reads a Link: <...>; rel="next" header. The next page must be
// on the same scheme, host and port as the registry, so the token is
// never sent anywhere else.
func nextPage(resp *http.Response) (string, error) {
	link := resp.Header.Get("Link")
	start, end := strings.Index(link, "<"), strings.Index(link, ">")
	if start < 0 || end < start || !strings.Contains(link, `rel="next"`) {
		return "", nil
	}
	next, err := resp.Request.URL.Parse(link[start+1 : end])
	if err != nil || sameOrigin(next, resp.Request.URL) != nil {
		return "", fmt.Errorf("the registry's next tag page %q is not on the registry", link[start+1:end])
	}
	return next.String(), nil
}

// registryURL is the registry's base URL: https everywhere but a loopback
// host, which Docker also treats as a plain-HTTP registry.
func registryURL(registry string) string {
	if registry == "docker.io" || registry == "index.docker.io" {
		registry = "registry-1.docker.io"
	}
	if isLoopbackHost(registry) {
		return "http://" + registry
	}
	return "https://" + registry
}

func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// do sends one request and, on a challenge, sends it again with an
// Authorization header: the saved credential's basic login or its bearer
// token when registry has one, an anonymous token from the challenge's
// realm otherwise. It returns the open response for a 2xx answer and a
// classified error otherwise.
func (h *HTTPRegistry) do(ctx context.Context, registry, method, rawURL, accept string) (*http.Response, error) {
	registry = credentialKey(registry)
	cred, hasCred, err := h.credential(ctx, registry)
	if err != nil {
		return nil, err
	}
	if hasCred && !strings.HasPrefix(rawURL, "https://") {
		return nil, fmt.Errorf("%w: the credential saved for %s is sent over https only, and %s is not", ErrCredentialUnusable, registry, rawURL)
	}
	resp, err := h.send(ctx, method, rawURL, accept, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusUnauthorized {
		return classify(resp)
	}
	challenge := resp.Header.Get("WWW-Authenticate")
	_ = resp.Body.Close()
	var login *Credential
	if hasCred {
		login = &cred
	}
	auth, err := h.authorize(ctx, registry, rawURL, challenge, login)
	if err != nil {
		return nil, err
	}
	resp, err = h.send(ctx, method, rawURL, accept, auth)
	if err != nil {
		return nil, err
	}
	resp, err = classify(resp)
	if errors.Is(err, ErrRegistryDenied) && hasCred {
		return nil, credentialRejected(registry)
	}
	return resp, err
}

func (h *HTTPRegistry) credential(ctx context.Context, registry string) (Credential, bool, error) {
	if h.Credentials == nil {
		return Credential{}, false, nil
	}
	return h.Credentials.Credential(ctx, registry)
}

func credentialRejected(registry string) error {
	return fmt.Errorf("%w: the registry %s refused the credential saved for it, or the account has no access to the image", ErrCredentialRejected, registry)
}

func (h *HTTPRegistry) send(ctx context.Context, method, rawURL, accept, authorization string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("building a registry request: %w", err)
	}
	req.Header.Set("Accept", accept)
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	resp, err := h.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting %s: %w", req.URL.Host, err)
	}
	return resp, nil
}

func classify(resp *http.Response) (*http.Response, error) {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return resp, nil
	}
	_ = resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusTooManyRequests:
		return nil, ErrRateLimited
	case http.StatusNotFound:
		return nil, ErrManifestNotFound
	case http.StatusUnauthorized, http.StatusForbidden:
		return nil, ErrRegistryDenied
	}
	return nil, fmt.Errorf("registry %s answered %s", resp.Request.URL.Host, resp.Status)
}

// authorize turns a 401's challenge into an Authorization header value. With
// a saved credential that is the basic login for a basic challenge, and a
// bearer token fetched from the challenge's realm with the basic login for a
// bearer one, but only when the realm is on the registry's own host (Docker
// Hub's token service excepted) and https; the credential is never sent
// anywhere else, and a challenge that cannot be answered with it is an error,
// not a reason to ask anonymously. Without one, a bearer challenge is
// answered with an anonymous token and a basic one with ErrRegistryDenied.
func (h *HTTPRegistry) authorize(ctx context.Context, registry, rawURL, challenge string, login *Credential) (string, error) {
	scheme, params, _ := strings.Cut(challenge, " ")
	var basicLogin string
	if login != nil {
		basicLogin = "Basic " + base64.StdEncoding.EncodeToString([]byte(login.Username+":"+login.Password))
	}
	switch strings.ToLower(scheme) {
	case "bearer":
	case "basic":
		if login == nil {
			return "", ErrRegistryDenied
		}
		return basicLogin, nil
	default:
		return "", fmt.Errorf("the registry asked for an unsupported login (%q)", scheme)
	}
	attrs := challengeParams(params)
	realm, err := url.Parse(attrs["realm"])
	if err != nil || !usableRealm(realm) {
		return "", fmt.Errorf("the registry's token service %q is not usable", attrs["realm"])
	}
	if login != nil {
		if err := credentialRealm(registry, rawURL, realm); err != nil {
			return "", err
		}
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if v := attrs[k]; v != "" {
			q.Set(k, v)
		}
	}
	realm.RawQuery = q.Encode()
	resp, err := h.send(ctx, http.MethodGet, realm.String(), "application/json", basicLogin)
	if err != nil {
		return "", err
	}
	resp, err = classify(resp)
	if errors.Is(err, ErrRegistryDenied) && login != nil {
		return "", credentialRejected(registry)
	}
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	var body struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBodyBytes)).Decode(&body); err != nil {
		return "", fmt.Errorf("reading the registry's token: %w", err)
	}
	token := body.Token
	if token == "" {
		token = body.AccessToken
	}
	if token == "" {
		return "", errors.New("the registry's token service sent no token")
	}
	return "Bearer " + token, nil
}

// dockerHubTokenService is the host Docker Hub's registry sends a bearer
// challenge to.
const dockerHubTokenService = "auth.docker.io"

// credentialRealm refuses a token service the credential must not be sent
// to: one that is not https, or not on the host the request goes to.
func credentialRealm(registry, rawURL string, realm *url.URL) error {
	if realm.Scheme != "https" {
		return fmt.Errorf("%w: the token service of %s is not https, and the credential saved for it is sent over https only", ErrCredentialUnusable, registry)
	}
	request, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", rawURL, err)
	}
	if strings.EqualFold(realm.Host, request.Host) || (registry == "docker.io" && realm.Host == dockerHubTokenService) {
		return nil
	}
	return fmt.Errorf("%w: the token service of %s is on %s, and the credential saved for it is sent to the registry's own host only", ErrCredentialUnusable, registry, realm.Host)
}

// usableRealm is an https token service, or a plain-HTTP one on loopback.
func usableRealm(u *url.URL) bool {
	if u.Host == "" {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && isLoopbackHost(u.Host))
}

var challengeParam = regexp.MustCompile(`(\w+)="([^"]*)"`)

func challengeParams(s string) map[string]string {
	out := map[string]string{}
	for _, m := range challengeParam.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	return out
}
