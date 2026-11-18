package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/jorlaru/stargaite/internal/store"
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

// terminalOut is standard output on a terminal: a character device.
type terminalOut struct{ bytes.Buffer }

func (*terminalOut) Stat() (os.FileInfo, error) { return os.Stat(os.DevNull) }

// TestBackupStdout: backup - writes the copy to standard output, byte for
// byte, and its summary to standard error, in JSON under --json; a stream
// shorter than announced exits 1 with the error on standard error, in JSON
// under --json, and standard output holds only the bytes received; a
// terminal on standard output is refused before anything is sent.
func TestBackupStdout(t *testing.T) {
	content := bytes.Repeat([]byte("SQLite format 3\x00"), 4096)
	// Set by the test, read by the handler: atomic, as a cut connection
	// gives the race detector no ordering.
	var short atomic.Bool
	var sent atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		sent.Add(1)
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("Content-Length", strconv.Itoa(len(content)))
		if short.Load() {
			_, _ = w.Write(content[:1000])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		_, _ = w.Write(content)
	}))
	t.Cleanup(srv.Close)
	vars := map[string]string{"STARGAITE_ADMIN_URL": srv.URL, "STARGAITE_ADMIN_KEY": testKey}

	r := runMain(vars, "backup", "-")
	if r.code != 0 || r.stdout != string(content) || r.stderr != "-: 65536 bytes\n" {
		t.Errorf("exit %d, %d bytes out, stderr %q", r.code, len(r.stdout), r.stderr)
	}
	r = runMain(vars, "backup", "-", "--json")
	if r.code != 0 || r.stdout != string(content) || r.stderr != `{"bytes":65536,"path":"-"}`+"\n" {
		t.Errorf("--json: exit %d, %d bytes out, stderr %q", r.code, len(r.stdout), r.stderr)
	}

	short.Store(true)
	r = runMain(vars, "backup", "-")
	if r.code != 1 || r.stdout != string(content[:1000]) || !strings.Contains(r.stderr, "the copy on standard output is not whole") {
		t.Errorf("short: exit %d, %d bytes out, stderr %q", r.code, len(r.stdout), r.stderr)
	}
	r = runMain(vars, "backup", "-", "--json")
	var env struct {
		Error struct{ Code, Message string }
	}
	if r.code != 1 || r.stdout != string(content[:1000]) || json.Unmarshal([]byte(r.stderr), &env) != nil || env.Error.Code != "internal_error" ||
		!strings.Contains(env.Error.Message, "the copy on standard output is not whole") {
		t.Errorf("short --json: exit %d, %d bytes out, stderr %q", r.code, len(r.stdout), r.stderr)
	}

	sent.Store(0)
	var out terminalOut
	var stderr bytes.Buffer
	if code := Main([]string{"backup", "-"}, getenv(vars), unread{}, &out, &stderr); code != 2 || out.Len() != 0 || sent.Load() != 0 ||
		!strings.Contains(stderr.String(), "standard output, which is a terminal") {
		t.Errorf("a terminal: exit %d, %d bytes out, %d requests, stderr %q", code, out.Len(), sent.Load(), &stderr)
	}
}

// TestRestore: restore puts a backup in place from a file or from standard
// input, into --data-dir or $STARGAITE_DATA_DIR, and says what it moved, in
// JSON under --json. Without --yes, and with "-" on a terminal, it exits 2
// and changes nothing; while serve holds the data directory, 1. The checks
// of the copy are store.Restore's (TestRestore there).
func TestRestore(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(ctx, t.TempDir(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	f, err := st.Backup(ctx)
	if err != nil {
		t.Fatal(err)
	}
	backup, err := io.ReadAll(f)
	_ = f.Close()
	_ = st.Close()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "backup.db")
	if err := os.WriteFile(file, backup, 0o600); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "data")
	size := strconv.Itoa(len(backup))

	for _, args := range [][]string{{"restore", file, "--data-dir", dir}, {"restore", "-", "--data-dir", dir, "--json"}} {
		if r := withStdin(nil, strings.NewReader(string(backup)), args...); r.code != 2 || r.stdout != "" || !strings.Contains(r.stderr, "give --yes") {
			t.Errorf("%q: exit %d, stdout %q, stderr %q", args, r.code, r.stdout, r.stderr)
		}
	}
	if r := withStdin(nil, terminal{}, "restore", "-", "--data-dir", dir, "--yes"); r.code != 2 || !strings.Contains(r.stderr, "standard input, which is a terminal") {
		t.Errorf("a terminal: exit %d, stderr %q", r.code, r.stderr)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("a refused restore made %s: %v", dir, err)
	}

	if r := runMain(nil, "restore", file, "--data-dir", dir, "--yes"); r.code != 0 || r.stdout != file+": "+size+" bytes restored into "+dir+", which held no store\n" {
		t.Errorf("into an empty directory: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	r := withStdin(map[string]string{"STARGAITE_DATA_DIR": dir}, strings.NewReader(string(backup)), "restore", "-", "--yes", "--json")
	var printed struct {
		Path     string
		DataDir  string `json:"data_dir"`
		Bytes    int
		Replaced []string
	}
	if err := json.Unmarshal([]byte(r.stdout), &printed); r.code != 0 || err != nil || printed.DataDir != dir || printed.Bytes != len(backup) ||
		!slices.Equal(printed.Replaced, []string{"stargaite.db"}) {
		t.Errorf("from standard input: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
	if r := runMain(nil, "restore", file, "--data-dir", dir, "--yes"); r.code != 0 ||
		r.stdout != file+": "+size+" bytes restored into "+dir+"; the store it replaced, stargaite.db, is in "+filepath.Join(dir, "replaced")+"\n" {
		t.Errorf("over a store: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}

	st, err = store.Open(ctx, dir, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if r := runMain(nil, "restore", file, "--data-dir", dir, "--yes"); r.code != 1 || !strings.Contains(r.stderr, "restore runs only while serve is stopped") {
		t.Errorf("serve holds it: exit %d, stdout %q, stderr %q", r.code, r.stdout, r.stderr)
	}
}
