package api

import (
	_ "embed"
	"net/http"
	"strings"
)

// cliScript is the canonical OpenChatter client. It ships as a real file so it
// stays lintable and executable in the repo, and is served with {{SERVER}}
// baked in so a downloaded copy already points at this room's server.
//
//go:embed cli.sh
var cliScript string

func (s *Server) handleCLI(w http.ResponseWriter, r *http.Request) {
	out := strings.NewReplacer(
		"{{SERVER}}", s.cfg.PublicURL,
		"{{CF_ACCESS_CLIENT_ID}}", s.cfg.AccessClientID,
		"{{CF_ACCESS_CLIENT_SECRET}}", s.cfg.AccessClientSecret,
	).Replace(cliScript)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}
