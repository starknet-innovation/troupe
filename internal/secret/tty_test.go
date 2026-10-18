package secret

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strconv"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// pty opens a pseudo terminal: the test types on master, readHidden reads the terminal.
func pty(t *testing.T) (master, terminal *os.File) {
	t.Helper()
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR, 0)
	if err != nil {
		t.Skip("no /dev/ptmx")
	}
	var unlock, number int32
	conn, _ := master.SyscallConn()
	conn.Control(func(fd uintptr) {
		syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock)))
		syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGPTN, uintptr(unsafe.Pointer(&number)))
	})
	terminal, err = os.OpenFile("/dev/pts/"+strconv.Itoa(int(number)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { terminal.Close(); master.Close() })
	return master, terminal
}

func echoes(t *testing.T, terminal *os.File) bool {
	var state syscall.Termios
	if err := termios(terminal, syscall.TCGETS, &state); err != nil {
		t.Fatal(err)
	}
	return state.Lflag&syscall.ECHO != 0
}

// The secret arrives, is never echoed back, and echo is on again afterwards.
func TestHiddenReadDoesNotEcho(t *testing.T) {
	master, terminal := pty(t)
	go func() {
		time.Sleep(100 * time.Millisecond)
		master.Write([]byte(planted + "\n"))
	}()
	got, err := readHidden(context.Background(), terminal, "Secret: ")
	if err != nil || string(got) != planted {
		t.Fatalf("got %q err %v", got, err)
	}
	if !echoes(t, terminal) {
		t.Fatal("echo was left off")
	}
	master.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	shown := make([]byte, 4096)
	n, _ := master.Read(shown)
	if bytes.Contains(shown[:n], []byte(planted)) {
		t.Fatal("the secret was echoed")
	}
}

// Ctrl+C ends the prompt at once, with echo back on, before the operator types again.
func TestInterruptEndsThePromptAtOnce(t *testing.T) {
	_, terminal := pty(t)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	_, err := readHidden(ctx, terminal, "Secret: ")
	if !errors.Is(err, ErrInterrupted) || time.Since(start) > 2*time.Second {
		t.Fatalf("err %v after %v", err, time.Since(start))
	}
	if !echoes(t, terminal) {
		t.Fatal("echo was left off")
	}
}
