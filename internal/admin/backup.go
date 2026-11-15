package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"
)

// sendBackup streams a checked copy of the store (ADR-018) as
// application/vnd.sqlite3 with its Content-Length. The listener's read and
// write timeouts would cut a long copy, so the read deadline is cleared and
// the write deadline set to backupWait. An error before the first byte is
// answered in the envelope; one after it cuts the connection, since the body
// then falls short of its Content-Length. A stop cuts the stream too. A read:
// not audited (SEC-7); one log line with the size.
func (s *server) sendBackup(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		s.fail(w, invalid("this route takes no query parameter"))
		return
	}
	rc := http.NewResponseController(w)
	if err := rc.SetReadDeadline(time.Time{}); err != nil {
		s.fail(w, fmt.Errorf("backup: the read deadline was not cleared: %w", err))
		return
	}
	if err := rc.SetWriteDeadline(time.Now().Add(backupWait)); err != nil {
		s.fail(w, fmt.Errorf("backup: the write deadline was not set: %w", err))
		return
	}
	f, err := s.backup(r.Context())
	if err != nil {
		s.fail(w, fmt.Errorf("backup: %w", err))
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		s.fail(w, fmt.Errorf("backup: %w", err))
		return
	}
	stop := context.AfterFunc(r.Context(), func() { _ = rc.SetWriteDeadline(time.Now()) })
	defer stop()
	w.Header().Set("Content-Type", "application/vnd.sqlite3")
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	n, err := io.Copy(w, f)
	id := w.Header().Get(RequestIDHeader)
	if err != nil || n != info.Size() {
		s.log.Error("backup cut", "request_id", id, "bytes", n, "size", info.Size(), "error", fmt.Sprint(err))
		return
	}
	s.log.Info("backup sent", "request_id", id, "bytes", n)
}
