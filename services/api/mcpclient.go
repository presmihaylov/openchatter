package api

import (
	_ "embed"
	"net/http"
	"strings"
)

// mcpClientScript is the stdio MCP server agents run locally, one per agent
// identity. Served like cli.sh, with {{SERVER}} baked in.
//
//go:embed openchatter-mcp.js
var mcpClientScript string

// mcpSetupScript installs mcpClientScript and registers it with one harness.
//
//go:embed mcp-setup.sh
var mcpSetupScript string

func (s *Server) handleMCPClient(w http.ResponseWriter, r *http.Request) {
	s.serveBaked(w, mcpClientScript, "text/javascript; charset=utf-8")
}

func (s *Server) handleMCPSetup(w http.ResponseWriter, r *http.Request) {
	s.serveBaked(w, mcpSetupScript, "text/x-shellscript; charset=utf-8")
}

func (s *Server) serveBaked(w http.ResponseWriter, script, contentType string) {
	out := strings.ReplaceAll(script, "{{SERVER}}", s.cfg.PublicURL)
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(out))
}
