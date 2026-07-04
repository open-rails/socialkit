package socialkit

import (
	"bytes"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAccessLog checks that a socialkit-internal 500 is logged at ERROR with its
// cause, while a client 4xx is logged only at DEBUG (not as an error).
func TestAccessLog(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		wantCode  int
		wantLevel string
		wantCause string
	}{
		{"internal 500", errors.New("boom from handler"), http.StatusInternalServerError, "ERROR", "boom from handler"},
		{"forbidden 403", ErrForbidden, http.StatusForbidden, "DEBUG", ""},
		{"not found 404", ErrNotFound, http.StatusNotFound, "DEBUG", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			rt := &Runtime{log: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))}

			h := rt.accessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				writeErr(w, tc.err)
			}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/gallery/1/comments", nil))

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			out := buf.String()
			t.Logf("logged: %s", strings.TrimSpace(out))
			if !strings.Contains(out, "level="+tc.wantLevel) {
				t.Fatalf("want level=%s, got: %s", tc.wantLevel, out)
			}
			if tc.wantCause != "" && !strings.Contains(out, tc.wantCause) {
				t.Fatalf("want cause %q in log, got: %s", tc.wantCause, out)
			}
			if tc.wantLevel == "DEBUG" && strings.Contains(out, "level=ERROR") {
				t.Fatalf("a 4xx must not log at ERROR, got: %s", out)
			}
		})
	}
}
