package main

import "golang.org/x/sys/unix"

// flushInput drops what was typed on the terminal fd and not yet read:
// TIOCFLUSH with FREAD, as the BSD libc's tcflush(fd, TCIFLUSH) asks it.
func flushInput(fd int) error { return unix.IoctlSetPointerInt(fd, unix.TIOCFLUSH, unix.TCIFLUSH) }
