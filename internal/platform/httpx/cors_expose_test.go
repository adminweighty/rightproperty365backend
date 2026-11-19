package httpx

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The calendar downloads (API 0.39.0) and the CSV exports name their file in
// Content-Disposition: the browser may only read it when it is exposed.
func TestCORSExposesContentDisposition(t *testing.T) {
	h := CORS([]string{"http://localhost:3000"})(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="appointment-RPA-1.ics"`)
	}))
	req := httptest.NewRequest(http.MethodGet, "/v1/me/appointments/x/calendar.ics", nil)
	req.Header.Set("Origin", "http://localhost:3000")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	exposed := rec.Header().Get("Access-Control-Expose-Headers")
	found := false
	for _, name := range strings.Split(exposed, ",") {
		found = found || strings.EqualFold(strings.TrimSpace(name), "Content-Disposition")
	}
	if !found {
		t.Fatalf("Access-Control-Expose-Headers = %q, want Content-Disposition in it", exposed)
	}
}
