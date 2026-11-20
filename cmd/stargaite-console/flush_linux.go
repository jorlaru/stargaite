package main

import "golang.org/x/sys/unix"

// flushInput drops what was typed on the terminal fd and not yet read.
func flushInput(fd int) error { return unix.IoctlSetInt(fd, unix.TCFLSH, unix.TCIFLUSH) }
