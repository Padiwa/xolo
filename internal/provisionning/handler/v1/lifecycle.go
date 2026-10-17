package v1

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"mime"
	"net/http"

	"github.com/bornholm/go-x/slogx"
	"github.com/xolo-gateway/xolo/internal/core/model"
	"github.com/xolo-gateway/xolo/internal/core/service"
)

// maxConfirmationSize bounds the body of a purge confirmation: one digest.
const maxConfirmationSize = 4096

// WithLifecycle serves the deletion of tenants, organizations and members:
// the deferred deletion, its status, the export of its scope and the
// confirmation of its purge. enabled announces the capability in the
// manifest; disabled, the routes answer lifecycle_disabled.
func WithLifecycle(lifecycle *service.LifecycleService, enabled bool) HandlerOption {
	return func(h *Handler) {
		h.lifecycle = lifecycle
		h.lifecycleEnabled = enabled
	}
}

// mountDeletes serves the deletions: immediate for domains and memberships,
// deferred for tenants, organizations and members.
func (h *Handler) mountDeletes() {
	const (
		tenant = "/v1/tenants/{tenantID}"
		org    = tenant + "/organizations/{orgID}"
	)
	h.mux.HandleFunc("DELETE "+tenant+"/domains/{hostname}", h.handleDeleteDomain)
	h.mux.HandleFunc("DELETE "+org+"/members/{memberID}", h.handleDeleteOrgMember)
	h.mux.HandleFunc("DELETE "+org+"/teams/{teamID}", h.handleDeleteTeam)
	h.mux.HandleFunc("DELETE "+org+"/teams/{teamID}/members/{memberID}", h.handleDeleteTeamMembership)
	if h.lifecycle == nil {
		return
	}
	if h.lifecycleEnabled {
		h.capabilities = append(h.capabilities, "lifecycle")
	}
	for _, route := range []struct{ path, family string }{
		{tenant, model.FamilyTenant},
		{org, model.FamilyOrganization},
		{tenant + "/members/{memberID}", model.FamilyMember},
	} {
		h.mux.HandleFunc("DELETE "+route.path, h.handleScheduleDeletion(route.family))
		h.mux.HandleFunc("GET "+route.path+"/deletion", h.handleReadDeletion(route.family))
		h.mux.HandleFunc("GET "+route.path+"/deletion/export", h.handleExportDeletion(route.family))
		h.mux.HandleFunc("POST "+route.path+"/purge-confirmation", h.handleConfirmDeletion(route.family))
	}
}

// noBody refuses a request carrying a body: a DELETE defines none, and a
// client sending one must be told rather than silently ignored.
func noBody(w http.ResponseWriter, r *http.Request) bool {
	var probe [1]byte
	if n, _ := io.ReadFull(r.Body, probe[:]); n > 0 {
		writeError(w, http.StatusBadRequest, codeInvalidRepresentation, "this request defines no body")
		return false
	}
	return true
}

func (h *Handler) handleDeleteDomain(w http.ResponseWriter, r *http.Request) {
	scope, key, ok := commonTarget(w, r, model.FamilyTenantDomain)
	if !ok || !noBody(w, r) {
		return
	}
	condition, ok := commonCondition(w, r)
	if !ok {
		return
	}
	if err := h.provisioning.DeleteDomain(r.Context(), model.TenantID(scope.TenantID), key, condition); err != nil {
		writeServiceError(r.Context(), w, err, "could not delete domain")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteOrgMember(w http.ResponseWriter, r *http.Request) {
	scope, key, ok := commonTarget(w, r, model.FamilyOrganizationMembership)
	if !ok || !noBody(w, r) {
		return
	}
	condition, ok := commonCondition(w, r)
	if !ok {
		return
	}
	err := h.provisioning.DeleteOrgMember(r.Context(), model.TenantID(scope.TenantID), model.OrgID(scope.OrganizationID), model.UserID(key), condition)
	if err != nil {
		writeServiceError(r.Context(), w, err, "could not delete membership")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteTeam(w http.ResponseWriter, r *http.Request) {
	scope, key, ok := commonTarget(w, r, model.FamilyTeam)
	if !ok || !noBody(w, r) {
		return
	}
	condition, ok := commonCondition(w, r)
	if !ok {
		return
	}
	if err := h.provisioning.DeleteTeam(r.Context(), model.TenantID(scope.TenantID), model.OrgID(scope.OrganizationID), model.TeamID(key), condition); err != nil {
		writeServiceError(r.Context(), w, err, "could not delete team")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) handleDeleteTeamMembership(w http.ResponseWriter, r *http.Request) {
	scope, key, ok := commonTarget(w, r, model.FamilyTeamMembership)
	if !ok || !noBody(w, r) {
		return
	}
	condition, ok := commonCondition(w, r)
	if !ok {
		return
	}
	err := h.provisioning.DeleteTeamMembership(r.Context(), model.TenantID(scope.TenantID), model.OrgID(scope.OrganizationID), model.TeamID(scope.TeamID), model.UserID(key), condition)
	if err != nil {
		writeServiceError(r.Context(), w, err, "could not delete team membership")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// writeDeletion answers with a deletion, and the revision of the frozen
// resource as its ETag while it is not purged.
func writeDeletion(w http.ResponseWriter, status int, deletion model.Deletion) {
	if deletion.ETag != "" {
		w.Header().Set("ETag", deletion.ETag)
	}
	writeJSON(w, status, deletion)
}

// handleScheduleDeletion records the deletion of the resource: it is frozen
// at once, and purged after the retention, once its export is confirmed. A
// repeated DELETE returns the deletion already recorded.
func (h *Handler) handleScheduleDeletion(family string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, key, ok := commonTarget(w, r, family)
		if !ok || !noBody(w, r) {
			return
		}
		condition, ok := commonCondition(w, r)
		if !ok {
			return
		}
		deletion, err := h.lifecycle.Freeze(r.Context(), scope, key, condition)
		if err != nil {
			writeServiceError(r.Context(), w, err, "could not delete resource")
			return
		}
		writeDeletion(w, http.StatusAccepted, deletion)
	}
}

func (h *Handler) handleReadDeletion(family string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, key, ok := commonTarget(w, r, family)
		if !ok || !noQuery(w, r) {
			return
		}
		deletion, err := h.lifecycle.Read(r.Context(), scope, key)
		if err != nil {
			writeServiceError(r.Context(), w, err, "deletion not found")
			return
		}
		writeDeletion(w, http.StatusOK, deletion)
	}
}

// handleExportDeletion streams the export of the scope. Once the first line
// is sent, a failure can only cut the stream: the export then lacks its
// trailer and verification rejects it.
func (h *Handler) handleExportDeletion(family string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, key, ok := commonTarget(w, r, family)
		if !ok || !noQuery(w, r) {
			return
		}
		stream := &lazyHeaderWriter{w: w, filename: "xolo-deletion.ndjson"}
		if err := h.lifecycle.Export(r.Context(), scope, key, stream); err != nil {
			if !stream.started {
				writeServiceError(r.Context(), w, err, "could not export deletion")
				return
			}
			slog.ErrorContext(r.Context(), "deletion export interrupted", slogx.Error(err))
		}
	}
}

type confirmationDTO struct {
	ExportSHA256 string `json:"export_sha256"`
}

// handleConfirmDeletion confirms the export of the scope with its digest,
// under the explicit revision of the deleted resource.
func (h *Handler) handleConfirmDeletion(family string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		scope, key, ok := commonTarget(w, r, family)
		if !ok {
			return
		}
		condition, ok := commonCondition(w, r)
		if !ok {
			return
		}
		media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || media != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, codeUnsupportedMediaType, "application/json is required")
			return
		}
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxConfirmationSize+1))
		if err != nil || len(raw) > maxConfirmationSize {
			writeError(w, http.StatusBadRequest, codeInvalidJSON, "invalid JSON body")
			return
		}
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			writeError(w, http.StatusBadRequest, codeInvalidRepresentation, "null is not a representation")
			return
		}
		var body confirmationDTO
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&body); err != nil || decoder.More() || body.ExportSHA256 == "" {
			writeError(w, http.StatusBadRequest, codeInvalidJSON, "expected one object holding export_sha256")
			return
		}
		deletion, err := h.lifecycle.Confirm(r.Context(), scope, key, condition, body.ExportSHA256)
		if err != nil {
			writeServiceError(r.Context(), w, err, "could not confirm deletion")
			return
		}
		writeDeletion(w, http.StatusOK, deletion)
	}
}
