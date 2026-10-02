package common

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/xolo-gateway/xolo/internal/core/port"
)

// WriteInvitationError keeps absent, foreign and wrong-recipient invitations
// indistinguishable. Internal errors are logged, never included in the response.
func WriteInvitationError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusInternalServerError
	switch {
	case errors.Is(err, port.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, port.ErrInvalid):
		status = http.StatusBadRequest
	case errors.Is(err, port.ErrNotAllowed):
		status = http.StatusForbidden
	default:
		slog.ErrorContext(r.Context(), "invitation operation failed", slog.Any("error", err))
	}
	http.Error(w, http.StatusText(status), status)
}
