package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestBackup: backup writes the copy the server streams to <file> with mode
// 0600 through <file>.part, which is gone afterwards, and prints the path and
// size. A short body, an answer that is not a store copy, an error answer, a
// part already there and a <file> created while the copy arrives each exit 1
// and leave no part of the CLI's own and no <file> of its own; a file found
// in place is never replaced or removed.
func TestBackup(t *testing.T) {
	content := bytes.Repeat([]byte("SQLite format 3\x00"), 4096)
	// The handler runs in the server's goroutines: a cut connection gives
	// the race detector no ordering, so answer is swapped atomically.
	var handler atomic.Pointer[func(w http.ResponseWriter, r *http.Request)]
	answer := func(f func(w http.ResponseWriter, r *http.Request)) { handler.Store(&f) }
	var got recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = recorded{method: r.Method, target: r.URL.RequestURI(), authorization: r.Header.Get("Authorization")}
		(*handler.Load())(w, r)
	}))
	t.Cleanup(srv.Close)
	vars := map[string]string{"STARGAITE_ADMIN_URL": srv.URL, "STARGAITE_ADMIN_KEY": testKey}
	stream := func(w http.ResponseWriter, body []byte, length int) {
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("Content-Length", strconv.Itoa(length))
		_, _ = w.Write(body)
	}
	dir := t.TempDir()

	answer(func(w http.ResponseWriter, _ *http.Request) { stream(w, content, len(content)) })
	file := filepath.Join(dir, "copy.db")
	if r := runMain(vars, "backup", file); r.code != 0 || r.stdout != file+": 65536 bytes\n" || r.stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if got != (recorded{method: http.MethodPost, target: "/admin/v1/backup", authorization: "Bearer " + testKey}) {
		t.Errorf("request %+v", got)
	}
	if data, err := os.ReadFile(file); err != nil || !bytes.Equal(data, content) {
		t.Errorf("the file holds %d bytes, error %v", len(data), err)
	}
	if info, err := os.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("mode %v, error %v", info.Mode(), err)
	}
	r := runMain(vars, "backup", filepath.Join(dir, "json.db"), "--json")
	var printed struct {
		Path  string
		Bytes int
	}
	if r.code != 0 || json.Unmarshal([]byte(r.stdout), &printed) != nil || printed.Path != filepath.Join(dir, "json.db") || printed.Bytes != len(content) {
		t.Errorf("--json: exit %d, stdout %q", r.code, r.stdout)
	}

	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		code   string // of the envelope with --json
	}{
		{"short body", func(w http.ResponseWriter, _ *http.Request) {
			stream(w, content[:1000], len(content))
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler) // the connection is cut after the headers and 1000 bytes
		}, "internal_error"},
		{"not a store copy", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, "{}\n")
		}, "internal_error"},
		{"unknown length", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.sqlite3")
			w.(http.Flusher).Flush() // chunked: no Content-Length
			_, _ = w.Write(content)
		}, "internal_error"},
		{"error answer", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"internal error","type":"api_error","code":"internal_error"}}`+"\n")
		}, "internal_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			answer(tc.answer)
			file := filepath.Join(t.TempDir(), "copy.db")
			for _, args := range [][]string{{"backup", file, "--json"}, {"backup", file}} {
				r := runMain(vars, args...)
				var env struct{ Error struct{ Code string } }
				if r.code != 1 || (len(args) == 3 && (json.Unmarshal([]byte(r.stdout), &env) != nil || env.Error.Code != tc.code)) || (len(args) == 2 && r.stderr == "") {
					t.Errorf("%q: exit %d, stdout %q, stderr %q", args, r.code, r.stdout, r.stderr)
				}
				for _, name := range []string{file, file + ".part"} {
					if _, err := os.Lstat(name); !os.IsNotExist(err) {
						t.Errorf("%q: %s is there: %v", args, name, err)
					}
				}
			}
		})
	}

	// A part already there is not the CLI's: it is refused and left alone.
	answer(func(w http.ResponseWriter, _ *http.Request) { stream(w, content, len(content)) })
	file = filepath.Join(dir, "parted.db")
	if err := os.WriteFile(file+".part", []byte("someone's"), 0o600); err != nil {
		t.Fatal(err)
	}
	if r := runMain(vars, "backup", file); r.code != 1 || !strings.Contains(r.stderr, "parted.db.part") {
		t.Errorf("a part in place: exit %d, stderr %q", r.code, r.stderr)
	}
	if data, err := os.ReadFile(file + ".part"); err != nil || string(data) != "someone's" {
		t.Errorf("the part in place: %q, error %v", data, err)
	}
	if _, err := os.Lstat(file); !os.IsNotExist(err) {
		t.Errorf("%s is there: %v", file, err)
	}

	// A file created at <file> while the copy arrives is never replaced.
	file = filepath.Join(dir, "raced.db")
	answer(func(w http.ResponseWriter, _ *http.Request) {
		if err := os.WriteFile(file, []byte("theirs"), 0o600); err != nil {
			t.Error(err)
		}
		stream(w, content, len(content))
	})
	if r := runMain(vars, "backup", file); r.code != 1 || !strings.Contains(r.stderr, "file exists") {
		t.Errorf("a file created meanwhile: exit %d, stderr %q", r.code, r.stderr)
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "theirs" {
		t.Errorf("the file created meanwhile: %q, error %v", data, err)
	}
	if _, err := os.Lstat(file + ".part"); !os.IsNotExist(err) {
		t.Errorf("the part is there: %v", err)
	}

	// Ctrl-C or a SIGTERM while the copy arrives ends the transfer as an
	// error: exit 1 and no part left. The signal goes to this process once
	// the part exists; without the CLI's handler it would end the test binary.
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		file := filepath.Join(dir, "signalled-"+strconv.Itoa(int(sig))+".db")
		answer(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/vnd.sqlite3")
			w.Header().Set("Content-Length", strconv.Itoa(len(content)))
			_, _ = w.Write(content[:1000])
			w.(http.Flusher).Flush()
			for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(5 * time.Millisecond) {
				if _, err := os.Stat(file + ".part"); err == nil {
					break
				}
				if time.Now().After(deadline) {
					t.Error("the CLI did not create its part within 10 s")
					return
				}
			}
			if err := syscall.Kill(os.Getpid(), sig); err != nil {
				t.Error(err)
			}
			<-r.Context().Done()
		})
		if r := runMain(vars, "backup", file); r.code != 1 || !strings.Contains(r.stderr, "the backup was not written") {
			t.Errorf("%v: exit %d, stderr %q", sig, r.code, r.stderr)
		}
		for _, name := range []string{file, file + ".part"} {
			if _, err := os.Lstat(name); !os.IsNotExist(err) {
				t.Errorf("%v: %s is there: %v", sig, name, err)
			}
		}
	}
}
