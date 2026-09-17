package api

import (
	"context"
	"net/http"
)

// setupExemptPaths are the only two API operations reachable before an
// admin account exists (#22's acceptance criteria) — the SPA's static
// assets are exempt too, but that is handled by cmd/hoservad mounting
// this gate only in front of the "/api/v1" prefix, never in front of the
// SPA-serving branch. login/logout/session/TOTP are deliberately not
// exempt: none of them can succeed with no admin account anyway, and
// "setup required" is a clearer signal than the generic auth error they'd
// otherwise produce.
var setupExemptPaths = map[string]bool{
	"/setup/status": true,
	"/setup/admin":  true,
}

// AdminExistsChecker is behind an interface so SetupGate's test doesn't
// need a real database — the same "system-touching subsystem behind an
// interface with a fake" pattern CLAUDE.md asks for elsewhere, applied to
// the one check that decides whether the whole API is reachable at all.
type AdminExistsChecker interface {
	CountAdmins(ctx context.Context) (int64, error)
}

var _ AdminExistsChecker = (*AuthStore)(nil)

const (
	setupRequiredCode    = "setup_required"
	setupRequiredMessage = "no admin account exists yet — complete first-run setup first"
)

// SetupGate wraps next (the API mux) and refuses every request whose path
// (with apiPrefix stripped) is not in setupExemptPaths while no admin
// account exists yet — before the request ever reaches routing, security
// or a handler method. apiPrefix is the same "/api/v1" prefix the
// generated server itself is mounted under (apiv1.WithPathPrefix).
func SetupGate(next http.Handler, checker AdminExistsChecker, apiPrefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if len(path) >= len(apiPrefix) && path[:len(apiPrefix)] == apiPrefix {
			path = path[len(apiPrefix):]
		}
		if setupExemptPaths[path] {
			next.ServeHTTP(w, r)
			return
		}

		n, err := checker.CountAdmins(r.Context())
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"code":"internal","message":"an internal error occurred"}`))
			return
		}
		if n == 0 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"code":"` + setupRequiredCode + `","message":"` + setupRequiredMessage + `"}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
