package socialkit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
)

// httpError carries an explicit status for handler-level failures (validation,
// auth). Sentinel resolver errors are mapped separately in writeErr.
type httpError struct {
	status int
	msg    string
}

func (e httpError) Error() string { return e.msg }

func badRequest(format string, a ...any) httpError {
	return httpError{status: http.StatusBadRequest, msg: fmt.Sprintf(format, a...)}
}

var (
	errUnauthorized = httpError{status: http.StatusUnauthorized, msg: "authentication required"}
	errForbidden    = httpError{status: http.StatusForbidden, msg: "forbidden"}
)

// writeErr maps kit errors to HTTP status. Resolver sentinels hide existence
// (not-visible -> 404); Authorizer/identity failures are fail-closed.
func writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, ErrNotVisible):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "not found"})
	case errors.Is(err, ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "forbidden"})
	default:
		var he httpError
		if errors.As(err, &he) {
			writeJSON(w, he.status, map[string]string{"error": he.msg})
			return
		}
		var me moderationError
		if errors.As(err, &me) {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": me.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// decodeJSON reads a JSON body with a sane size cap.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return badRequest("invalid JSON body: %v", err)
	}
	return nil
}

// parsePage reads limit/offset with a default limit of 20 and a hard cap of 100.
// The shared pager for every module's list handler.
func parsePage(req *http.Request) (limit, offset int) {
	limit, offset = 20, 0
	if v, err := strconv.Atoi(req.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	if limit > 100 {
		limit = 100
	}
	if v, err := strconv.Atoi(req.URL.Query().Get("offset")); err == nil && v > 0 {
		offset = v
	}
	return limit, offset
}
