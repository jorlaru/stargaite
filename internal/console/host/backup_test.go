package host

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// leftIn is the file names left in dir.
func leftIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

// writing is a write that writes content and says it wrote n bytes, then
// returns err.
func writing(content string, n int64, err error) func(io.Writer) (int64, error) {
	return func(w io.Writer) (int64, error) {
		_, _ = io.WriteString(w, content)
		return n, err
	}
}

func TestBackupFile(t *testing.T) {
	h := New(t.TempDir())
	var said []string
	c, err := h.Backup(context.Background(), func(l string) { said = append(said, l) }, "v1.0+x/y", writing("SQLite format 3\x00 copy", 21, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`^backups/v1\.0_x_y-[0-9]{8}T[0-9]{6}Z\.db$`).MatchString(c.Path) || c.Size != 21 ||
		!slices.Equal(said, []string{"backing up the store to " + c.Path}) || time.Since(c.Time) > time.Minute {
		t.Fatalf("copy %+v, said %q", c, said)
	}
	for path, mode := range map[string]os.FileMode{"backups": 0o700, c.Path: 0o600} {
		if info, err := os.Stat(filepath.Join(h.Dir, path)); err != nil || info.Mode().Perm() != mode {
			t.Errorf("%s: %v, %v", path, info.Mode(), err)
		}
	}
	if read(t, filepath.Join(h.Dir, c.Path)) != "SQLite format 3\x00 copy" || !slices.Equal(leftIn(t, filepath.Join(h.Dir, "backups")), []string{filepath.Base(c.Path)}) {
		t.Errorf("left %q", leftIn(t, filepath.Join(h.Dir, "backups")))
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name  string
		ctx   context.Context
		write func(io.Writer) (int64, error)
		want  string
	}{
		{"short", context.Background(), writing("abc", 5, nil), "the copy is 3 bytes, the proxy said 5"},
		{"empty", context.Background(), writing("", 0, nil), "the copy is empty"},
		{"failed", context.Background(), writing("abc", 0, errors.New("exit 1")), "exit 1"},
		{"cancelled", cancelled, writing("abc", 3, nil), context.Canceled.Error()},
	} {
		h := New(t.TempDir())
		write(t, filepath.Join(h.Dir, "backups", "x.db.part"), "stale")
		_, err := h.Backup(tc.ctx, func(string) {}, "v1", tc.write)
		if err == nil || err.Error() != tc.want || !slices.Equal(leftIn(t, filepath.Join(h.Dir, "backups")), []string{"x.db.part"}) {
			t.Errorf("%s: err %v, left %q", tc.name, err, leftIn(t, filepath.Join(h.Dir, "backups")))
		}
		if copies, err := h.Copies(); len(copies) != 0 || err != nil {
			t.Errorf("%s: copies %+v, %v", tc.name, copies, err)
		}
	}

	// An existing backups/ keeps its mode; a name taken is not replaced.
	h = New(t.TempDir())
	if err := os.Mkdir(filepath.Join(h.Dir, "backups"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := range 3 {
		write(t, filepath.Join(h.Dir, "backups", "v1-"+now.Add(time.Duration(i)*time.Second).Format("20060102T150405Z")+".db"), "theirs")
	}
	if _, err := h.Backup(context.Background(), func(string) {}, "v1", writing("mine", 4, nil)); err == nil {
		t.Error("a name taken was replaced")
	}
	if info, _ := os.Stat(filepath.Join(h.Dir, "backups")); info.Mode().Perm() != 0o755 {
		t.Errorf("backups/ is %v", info.Mode())
	}
	for _, n := range leftIn(t, filepath.Join(h.Dir, "backups")) {
		if read(t, filepath.Join(h.Dir, "backups", n)) != "theirs" {
			t.Errorf("%s was changed or left", n)
		}
	}
}

func TestCopies(t *testing.T) {
	h := New(t.TempDir())
	if copies, err := h.Copies(); len(copies) != 0 || err != nil {
		t.Fatalf("no backups/: %+v, %v", copies, err)
	}
	base := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	for i, n := range []string{"b.db", "a.db", "c.db"} {
		p := filepath.Join(h.Dir, "backups", n)
		write(t, p, strings.Repeat("x", i+1))
		if err := os.Chtimes(p, base, base.Add(time.Duration(i)*time.Hour)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(filepath.Join(h.Dir, "backups", "x.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(h.Dir, "backups", "y.txt"), "y")
	write(t, filepath.Join(h.Dir, "backups", "z.db.part"), "z")
	copies, err := h.Copies()
	var got []string
	for _, c := range copies {
		got = append(got, c.Path+" "+strconv.FormatInt(c.Size, 10)+" "+c.Time.UTC().Format(time.TimeOnly))
	}
	if err != nil || !slices.Equal(got, []string{"backups/c.db 3 12:00:00", "backups/a.db 2 11:00:00", "backups/b.db 1 10:00:00"}) {
		t.Errorf("%q, %v", got, err)
	}

	h = New(filepath.Join(t.TempDir(), "a[b]"))
	write(t, filepath.Join(h.Dir, "backups", "a.db"), "x")
	if copies, err := h.Copies(); len(copies) != 1 || copies[0].Path != "backups/a.db" || err != nil {
		t.Errorf("a clone at a[b]: %+v, %v", copies, err)
	}
}

func TestCopyAt(t *testing.T) {
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, "backups", "a.db"), "copy")
	write(t, filepath.Join(h.Dir, "empty.db"), "")
	if err := os.Mkdir(filepath.Join(h.Dir, "dir.db"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(h.Dir, "fifo.db"), 0o600); err != nil {
		t.Fatal(err)
	}
	unread := []string{"missing.db", "dir.db", "empty.db", "fifo.db"}
	if os.Geteuid() != 0 { // root reads a file of mode 0
		write(t, filepath.Join(h.Dir, "unreadable.db"), "copy")
		if err := os.Chmod(filepath.Join(h.Dir, "unreadable.db"), 0); err != nil {
			t.Fatal(err)
		}
		unread = append(unread, "unreadable.db")
	}
	for _, p := range unread {
		if _, err := h.CopyAt(p); err == nil || err.Error() != p+" is not a file with a copy in it; nothing was changed" {
			t.Errorf("%s: %v", p, err)
		}
	}
	for _, p := range []string{"backups/a.db", filepath.Join(h.Dir, "backups", "a.db")} {
		if c, err := h.CopyAt(p); err != nil || c.Path != p || c.Size != 4 || time.Since(c.Time) > time.Minute {
			t.Errorf("%s: %+v, %v", p, c, err)
		}
	}
}

func TestRestore(t *testing.T) {
	logs := t.TempDir()
	log, got := filepath.Join(logs, "argv"), filepath.Join(logs, "stdin")
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\n/bin/cat > " + got + "\necho 'restored'\n"})
	h := New(t.TempDir())
	write(t, filepath.Join(h.Dir, "backups", "a.db"), "SQLite format 3\x00 the copy")
	c, err := h.CopyAt("backups/a.db")
	if err != nil {
		t.Fatal(err)
	}
	var said []string
	code, err := h.Restore(context.Background(), func(l string) { said = append(said, l) }, c)
	if code != 0 || err != nil || read(t, log) != "compose run --rm --no-deps -T stargaite restore --yes -\n" ||
		read(t, got) != "SQLite format 3\x00 the copy" || !slices.Equal(said, []string{"restored"}) {
		t.Fatalf("code %d, err %v, argv %q, said %q", code, err, read(t, log), said)
	}
	fakes(t, map[string]string{"docker": "exit 3\n"})
	if code, err := h.Restore(context.Background(), func(string) {}, c); code != 3 || err != nil {
		t.Errorf("exit 3: code %d, err %v", code, err)
	}
	// A copy that is no longer a file once open is refused before docker runs.
	if err := os.Remove(log); err != nil {
		t.Fatal(err)
	}
	fakes(t, map[string]string{"docker": `echo "$*" >> ` + log + "\n"})
	if _, err := h.Restore(context.Background(), func(string) {}, Copy{Path: "backups"}); err == nil ||
		err.Error() != "backups is not a file with a copy in it" || read(t, log) != "" {
		t.Errorf("a directory: %v, argv %q", err, read(t, log))
	}
	fakes(t, map[string]string{"docker": "exec /bin/sleep 5\n"})
	h.upBound = 100 * time.Millisecond
	if _, err := h.Restore(context.Background(), func(string) {}, c); err == nil || err.Error() != "docker compose run was not done within 20 minutes" {
		t.Errorf("bound: %v", err)
	}
}
