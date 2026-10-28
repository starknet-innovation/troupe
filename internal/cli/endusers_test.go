package cli

import (
	"os"
	"syscall"
	"testing"
)

// nonblocking reports whether f's descriptor is in non-blocking mode, read without Fd.
func nonblocking(t *testing.T, f *os.File) bool {
	t.Helper()
	conn, err := f.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var flags uintptr
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		flags, _, errno = syscall.Syscall(syscall.SYS_FCNTL, fd, syscall.F_GETFL, 0)
	}); err != nil {
		t.Fatal(err)
	}
	if errno != 0 {
		t.Fatal(errno)
	}
	return flags&syscall.O_NONBLOCK != 0
}

// A pipe is not a terminal, and asking leaves it non-blocking, so a read on it can still be
// ended.
func TestAPipeIsNoTerminalAndStaysNonBlocking(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	defer writer.Close()
	if !nonblocking(t, reader) {
		t.Fatal("a new pipe is already blocking")
	}
	if terminal(reader) {
		t.Fatal("a pipe is a terminal")
	}
	if !nonblocking(t, reader) {
		t.Fatal("terminal switched the file to blocking mode")
	}
}

func TestAPseudoTerminalIsATerminal(t *testing.T) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx")
	}
	defer master.Close()
	if !terminal(master) {
		t.Fatal("a pseudo terminal is not a terminal")
	}
}
