package server

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"gardenio/kbeads/internal/model"
)

// setConfigRequest is the JSON body for PUT /v1/configs/{key}.
type setConfigRequest struct {
	Value json.RawMessage `json:"value"`
}

// handleSetConfig handles PUT /v1/configs/{key}.
func (s *BeadsServer) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	var req setConfigRequest
	// Configs can carry formulas and type-config JSON, so use the larger
	// bound. See AUDIT-2026-04-18 §H1.
	if !decodeJSONBodyLimit(w, r, &req, beadsMaxConfigBodyBytes) {
		return
	}

	config := &model.Config{
		Key:   key,
		Value: req.Value,
	}

	if denial := authorizeSetConfigKey(r.Context(), key); denial != nil {
		writeAuthzDenial(w, denial)
		return
	}
	if denial := s.authorizeSetConfig(r.Context(), config); denial != nil {
		writeAuthzDenial(w, denial)
		return
	}
	// An agent carrying a restrict taint writes no config (restricted_ops.go).
	if denial := s.restrictTaintOpDenial(r.Context(), restrictedOpRequest{Op: opTypeWrite, Action: "update", BeadType: "config"}); denial != nil {
		writeAuthzDenial(w, denial)
		return
	}

	err := s.store.SetConfig(r.Context(), config)
	s.typeConfigs.invalidate(key)
	if err != nil {
		writeServerError(r.Context(), w, "handleSetConfig", err, "failed to set config")
		return
	}

	writeJSON(w, http.StatusOK, config)
}

// handleGetConfig handles GET /v1/configs/{key}.
func (s *BeadsServer) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}
	if denial := authorizeGetConfigKey(r.Context(), key); denial != nil {
		writeAuthzDenial(w, denial)
		return
	}

	config, err := s.store.GetConfig(r.Context(), key)
	if errors.Is(err, sql.ErrNoRows) {
		if builtin, ok := builtinConfigs[key]; ok {
			writeJSON(w, http.StatusOK, builtin)
			return
		}
		writeError(w, http.StatusNotFound, "config not found")
		return
	}
	if err != nil {
		writeServerError(r.Context(), w, "handleGetConfig", err, "failed to get config")
		return
	}

	writeJSON(w, http.StatusOK, config)
}

// handleListConfigs handles GET /v1/configs?namespace=...
func (s *BeadsServer) handleListConfigs(w http.ResponseWriter, r *http.Request) {
	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		writeError(w, http.StatusBadRequest, "namespace query parameter is required")
		return
	}
	if denial := authorizeListConfigNamespace(r.Context(), namespace); denial != nil {
		writeAuthzDenial(w, denial)
		return
	}

	configs, err := s.listConfigsWithBuiltins(r.Context(), namespace)
	if err != nil {
		writeServerError(r.Context(), w, "handleListConfigs", err, "failed to list configs")
		return
	}

	if configs == nil {
		configs = []*model.Config{}
	}

	writeJSON(w, http.StatusOK, map[string]any{"configs": configs})
}

// handleDeleteConfig handles DELETE /v1/configs/{key}.
func (s *BeadsServer) handleDeleteConfig(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	if key == "" {
		writeError(w, http.StatusBadRequest, "key is required")
		return
	}

	err := s.store.DeleteConfig(r.Context(), key)
	s.typeConfigs.invalidate(key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "config not found")
			return
		}
		writeServerError(r.Context(), w, "handleDeleteConfig", err, "failed to delete config")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}
