package api

import (
	"errors"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/SkillfulAgents/tabtender/internal/auth"
	"github.com/SkillfulAgents/tabtender/internal/config"
	"github.com/SkillfulAgents/tabtender/internal/logging"
	"github.com/SkillfulAgents/tabtender/internal/sessions"
	"github.com/SkillfulAgents/tabtender/internal/tunnel"
)

func (s *Server) registerManagementRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /mgmt/v1/status", s.handleStatus)
	mux.HandleFunc("GET /mgmt/v1/config", s.handleGetConfig)
	mux.HandleFunc("PUT /mgmt/v1/config", s.handlePutConfig)
	mux.HandleFunc("GET /mgmt/v1/apikey", s.handleGetAPIKey)
	mux.HandleFunc("POST /mgmt/v1/apikey/rotate", s.handleRotateAPIKey)
	mux.HandleFunc("PUT /mgmt/v1/tunnel/token", s.handleSetTunnelToken)
	mux.HandleFunc("POST /mgmt/v1/tunnel/restart", s.handleRestartTunnel)
	mux.HandleFunc("GET /mgmt/v1/profiles", s.handleListProfiles)
	mux.HandleFunc("DELETE /mgmt/v1/profiles/{name}", s.handleDeleteProfile)
	mux.HandleFunc("GET /mgmt/v1/logs", s.handleLogs)
}

type statusResponse struct {
	Version        string              `json:"version"`
	PID            int                 `json:"pid"`
	StartedAt      time.Time           `json:"startedAt"`
	UptimeSeconds  int64               `json:"uptimeSeconds"`
	LocalURL       string              `json:"localUrl"`
	PublicURL      string              `json:"publicUrl"`
	Sessions       capacityResponse    `json:"sessions"`
	Browser        browserResponse     `json:"browser"`
	Tunnel         tunnel.TunnelStatus `json:"tunnel"`
	TunnelToken    string              `json:"tunnelTokenSource"`
	ConfigPath     string              `json:"configPath"`
	DataDir        string              `json:"dataDir"`
	LogFile        string              `json:"logFile"`
	PendingRestart []string            `json:"pendingRestart"`
}

func (s *Server) handleStatus(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, statusResponse{
		Version:        s.Version,
		PID:            os.Getpid(),
		StartedAt:      s.startedAt.UTC(),
		UptimeSeconds:  int64(time.Since(s.startedAt).Seconds()),
		LocalURL:       s.localBaseURL(),
		PublicURL:      s.publicBaseURL(),
		Sessions:       s.capacity(),
		Browser:        s.browserStatus(true),
		Tunnel:         s.Tunnel.Status(),
		TunnelToken:    s.Backend.TunnelTokenSource(),
		ConfigPath:     s.Paths.ConfigFile,
		DataDir:        s.Paths.DataDir,
		LogFile:        s.Paths.LogFile(),
		PendingRestart: config.PendingRestart(s.Backend.RunningConfig(), s.Backend.Config().Effective),
	})
}

// configResponse separates what is stored in the file from what is in
// effect, and names the settings pinned by the environment so a UI can show
// them read-only.
type configResponse struct {
	Path                string                     `json:"path"`
	File                config.Config              `json:"file"`
	Effective           config.Config              `json:"effective"`
	EnvOverrides        map[string]config.Override `json:"envOverrides"`
	RestartRequiredKeys []string                   `json:"restartRequiredKeys"`
	PendingRestart      []string                   `json:"pendingRestart"`
}

func (s *Server) configResponse(snap config.Snapshot) configResponse {
	return configResponse{
		Path:                snap.Path,
		File:                snap.File,
		Effective:           snap.Effective,
		EnvOverrides:        snap.Overrides,
		RestartRequiredKeys: config.RestartRequiredKeys,
		PendingRestart:      config.PendingRestart(s.Backend.RunningConfig(), snap.Effective),
	}
}

func (s *Server) handleGetConfig(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.configResponse(s.Backend.Config()))
}

// handlePutConfig replaces the file layer. The body starts from the current
// file layer, so clients may send either the full document or only the
// sections they change.
func (s *Server) handlePutConfig(w http.ResponseWriter, r *http.Request) {
	file := s.Backend.Config().File
	if err := decodeJSON(w, r, &file); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	snap, err := s.Backend.UpdateConfig(file)
	switch {
	case err != nil && snap.Path != "":
		// The configuration was valid and has been saved, but part of it
		// could not be applied to the running service.
		writeError(w, http.StatusInternalServerError, "apply_failed", err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.configResponse(snap))
}

type apiKeyResponse struct {
	APIKey string `json:"apiKey"`
	// Source is "environment" when pinned by TABTENDER_API_KEY, otherwise
	// the secret store backend.
	Source    string `json:"source"`
	Rotatable bool   `json:"rotatable"`
}

func (s *Server) apiKeyResponse() apiKeyResponse {
	source := s.Keys.Source()
	return apiKeyResponse{APIKey: s.Keys.Key(), Source: source, Rotatable: source != auth.SourceEnvironment}
}

func (s *Server) handleGetAPIKey(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.apiKeyResponse())
}

func (s *Server) handleRotateAPIKey(w http.ResponseWriter, _ *http.Request) {
	if _, err := s.Keys.Rotate(); err != nil {
		if errors.Is(err, auth.ErrEnvironmentControlled) {
			writeError(w, http.StatusConflict, "environment_controlled", err.Error())
			return
		}
		s.Logger.Error("API key rotation failed", "error", err)
		writeError(w, http.StatusInternalServerError, "internal_error", "failed to rotate the API key")
		return
	}
	s.Logger.Info("API key rotated")
	writeJSON(w, http.StatusOK, s.apiKeyResponse())
}

func (s *Server) handleSetTunnelToken(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "validation_error", err.Error())
		return
	}
	if err := s.Backend.SetTunnelToken(body.Token); err != nil {
		if errors.Is(err, config.ErrEnvironmentControlled) {
			writeError(w, http.StatusConflict, "environment_controlled", err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"tunnelTokenSource": s.Backend.TunnelTokenSource()})
}

func (s *Server) handleRestartTunnel(w http.ResponseWriter, _ *http.Request) {
	if err := s.Backend.RestartTunnel(); err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, s.Tunnel.Status())
}

type profileResponse struct {
	Name      string `json:"name"`
	InUse     bool   `json:"inUse"`
	SessionID string `json:"sessionId,omitempty"`
}

func (s *Server) handleListProfiles(w http.ResponseWriter, _ *http.Request) {
	profiles, err := s.Sessions.Profiles()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		return
	}
	out := make([]profileResponse, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, profileResponse{Name: p.Name, InUse: p.InUse, SessionID: p.SessionID})
	}
	writeJSON(w, http.StatusOK, map[string]any{"profiles": out})
}

func (s *Server) handleDeleteProfile(w http.ResponseWriter, r *http.Request) {
	if err := s.Sessions.DeleteProfile(r.PathValue("name")); err != nil {
		var se *sessions.Error
		switch {
		case errors.As(err, &se) && se.Kind == sessions.KindNotFound:
			writeError(w, http.StatusNotFound, "profile_not_found", se.Message)
		case errors.As(err, &se):
			s.writeSessionError(w, err)
		default:
			writeError(w, http.StatusInternalServerError, "internal_error", err.Error())
		}
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit := 200
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "validation_error", "limit must be a positive integer")
			return
		}
		limit = n
	}
	minLevel := slog.LevelInfo
	if raw := r.URL.Query().Get("level"); raw != "" {
		if err := minLevel.UnmarshalText([]byte(raw)); err != nil {
			writeError(w, http.StatusBadRequest, "validation_error", "level must be one of debug, info, warn, error")
			return
		}
	}
	entries := []logging.Entry{}
	if s.Logs != nil {
		entries = s.Logs.Recent(limit, minLevel)
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": entries})
}
