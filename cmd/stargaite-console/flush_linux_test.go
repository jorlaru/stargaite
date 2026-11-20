package main

import (
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestFlushInput: what was typed before the console starts is dropped, so a
// stray Enter cannot open the first suggestion; without the flush it is
// read.
func TestFlushInput(t *testing.T) {
	for _, flush := range []bool{true, false} {
		master, slave := openPTY(t)
		fd := int(slave.Fd())
		tio, err := unix.IoctlGetTermios(fd, unix.TCGETS)
		if err != nil {
			t.Fatal(err)
		}
		tio.Lflag &^= unix.ICANON | unix.ECHO
		tio.Iflag &^= unix.ICRNL
		tio.Cc[unix.VMIN], tio.Cc[unix.VTIME] = 0, 2
		if err := unix.IoctlSetTermios(fd, unix.TCSETS, tio); err != nil {
			t.Fatal(err)
		}
		if _, err := master.WriteString("abc\r"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(50 * time.Millisecond) // the line discipline takes the bytes
		if flush {
			if err := flushInput(fd); err != nil {
				t.Fatal(err)
			}
		}
		buf := make([]byte, 16)
		n, _ := unix.Read(fd, buf)
		if got := string(buf[:max(n, 0)]); flush && got != "" || !flush && got != "abc\r" {
			t.Fatalf("flush %v: read %q", flush, got)
		}
	}
}
