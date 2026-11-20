package host

import (
	"net"
	"regexp"
	"strconv"
	"time"
)

// probeBound bounds each dial of the port probe.
const probeBound = time.Second

// PortTaken reports whether something accepts a TCP connection on
// 127.0.0.1:port within probeBound.
func PortTaken(port int) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), probeBound)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// NextFree is the first port of from+1 to from+100, at most 65535, that is
// not taken, or 0 when there is none.
func NextFree(from int) int {
	for p := from + 1; p <= from+100 && p <= 65535; p++ {
		if !PortTaken(p) {
			return p
		}
	}
	return 0
}

// PortNumber is s as a port number, 1 to 65535, written without a sign or
// leading zeros, as setup.sh checks it; 0 and false when it is none.
func PortNumber(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || !regexp.MustCompile(`^[1-9][0-9]{0,4}$`).MatchString(s) || n > 65535 {
		return 0, false
	}
	return n, true
}
