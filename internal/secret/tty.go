package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
	"unsafe"
)

// ErrInterrupted is the operator pressing Ctrl+C at the prompt.
var ErrInterrupted = errors.New("interrupted")

// TerminalReader reads one line from the controlling terminal with echo off, restoring the
// terminal even when the operator interrupts. It refuses when there is no terminal: a secret
// is never read from a pipe or a file.
func TerminalReader(ctx context.Context) func(prompt string) ([]byte, error) {
	return func(prompt string) ([]byte, error) {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return nil, errors.New("private_terminal_required")
		}
		defer tty.Close()
		return readHidden(ctx, tty, prompt)
	}
}

// termios runs one terminal ioctl. It goes through SyscallConn, never Fd: Fd would switch
// the file to blocking mode, and then closing it could no longer end a pending read.
func termios(tty *os.File, request uintptr, t *syscall.Termios) error {
	conn, err := tty.SyscallConn()
	if err != nil {
		return err
	}
	var errno syscall.Errno
	if err := conn.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, request, uintptr(unsafe.Pointer(t)))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return errno
	}
	return nil
}

func readHidden(ctx context.Context, tty *os.File, prompt string) ([]byte, error) {
	var saved syscall.Termios
	if termios(tty, syscall.TCGETS, &saved) != nil {
		return nil, errors.New("private_terminal_required")
	}
	hidden := saved
	hidden.Lflag &^= syscall.ECHO
	if termios(tty, syscall.TCSETS, &hidden) != nil {
		return nil, errors.New("private_terminal_required")
	}
	// Echo comes back before anything else can happen: on return, and on Ctrl+C before the
	// read is ended, so nothing typed afterwards is hidden or shown out of turn.
	defer termios(tty, syscall.TCSETS, &saved)
	fmt.Fprint(tty, prompt)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			termios(tty, syscall.TCSETS, &saved)
			tty.SetReadDeadline(time.Unix(1, 0))
		case <-done:
		}
	}()
	// Room enough that the secret is never copied into a grown buffer left behind.
	line := make([]byte, 0, 4096)
	buf := make([]byte, 1)
	for {
		n, err := tty.Read(buf)
		if err != nil || ctx.Err() != nil {
			clear(line)
			fmt.Fprintln(tty)
			if ctx.Err() != nil {
				return nil, ErrInterrupted
			}
			return nil, errors.New("private_terminal_required")
		}
		if n == 1 {
			if buf[0] == '\n' || buf[0] == '\r' {
				break
			}
			if len(line) == cap(line) {
				clear(line)
				return nil, errors.New("secret_too_long")
			}
			line = append(line, buf[0])
		}
	}
	fmt.Fprintln(tty)
	return line, nil
}
