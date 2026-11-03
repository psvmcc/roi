package roi

import (
	"log/slog"
	"net/http"
	"strings"
)

// APIRequest contains only operational metadata, never authentication headers, query strings,
// request bodies, credentials, or response contents.
type APIRequest struct {
	Time, Duration                     int64
	Method, Path, Client               string
	IP, Session, Realm                 string
	Device, DeviceState, TargetSession string
	Status, Bytes                      int
}
type apiResponseWriter struct {
	http.ResponseWriter
	status, bytes int
}

func (w *apiResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	if status >= 200 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}
func (w *apiResponseWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	n, e := w.ResponseWriter.Write(b)
	w.bytes += n
	return n, e
}
func (w *apiResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (a *App) logAPI(next http.Handler) http.Handler     { return a.auditHandler("api", next) }

func logText(value string) string { return strings.ToValidUTF8(value[:min(len(value), 256)], "?") }

func logAuditError(err error) { slog.Warn("audit storage", "error", err) }
func logAuditEvent(e APIRequest) {
	slog.Debug(e.Realm+" request", "device", e.Device, "device_state", e.DeviceState, "target_session", shortSession(e.TargetSession), "method", e.Method, "path", e.Path, "status", e.Status, "ip", e.IP, "session", shortSession(e.Session), "client", e.Client, "bytes", e.Bytes, "duration_ms", e.Duration)
}
