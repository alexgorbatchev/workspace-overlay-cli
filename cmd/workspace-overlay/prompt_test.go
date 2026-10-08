package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	"golang.org/x/sys/unix"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/holder"
)

// openTerminal creates a terminal of the given width. The test types into
// keyboard and reads from it what the program printed on screen.
func openTerminal(t *testing.T, columns uint16) (keyboard, screen *os.File) {
	t.Helper()
	keyboard, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = keyboard.Close() }) // nothing to report: the test is over
	fd := int(keyboard.Fd())
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		t.Fatal(err)
	}
	number, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		t.Fatal(err)
	}
	screen, err = os.OpenFile(fmt.Sprintf("/dev/pts/%d", number), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.IoctlSetWinsize(int(screen.Fd()), unix.TIOCSWINSZ, &unix.Winsize{Row: 24, Col: columns}); err != nil {
		t.Fatal(err)
	}
	return keyboard, screen
}

// printed closes the screen and returns everything shown on it.
func printed(t *testing.T, keyboard, screen *os.File) string {
	t.Helper()
	if err := screen.Close(); err != nil {
		t.Fatal(err)
	}
	// Reading a terminal whose other end is closed ends with an error
	// instead of end-of-file, after everything printed has been read.
	shown, _ := io.ReadAll(keyboard)
	return strings.ReplaceAll(string(shown), "\r\n", "\n")
}

func TestBusyMountQuestion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	target := filepath.Join(home, "alpha")
	holders := []holder.Process{
		{PID: 4101, Command: "nvim AGENTS.md", Dir: target},
		{PID: 4102, Command: "sleep 60", Dir: filepath.Join(target, "docs")},
	}
	tests := []struct {
		name, typed string
		want        bool
	}{
		{"y", "y\n", true},
		{"yes in capitals", "YES\n", true},
		{"n", "n\n", false},
		{"enter alone", "\n", false},
		{"anything else", "maybe\n", false},
		{"end of input", "\x04", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keyboard, screen := openTerminal(t, 100)
			confirm := confirmStop(screen, screen)
			if confirm == nil {
				t.Fatal("no question for a person at a terminal")
			}
			if _, err := keyboard.WriteString(tt.typed); err != nil {
				t.Fatal(err)
			}

			got := confirm(target, holders)

			if got != tt.want {
				t.Fatalf("answer %q stops the processes = %v, want %v", tt.typed, got, tt.want)
			}
			shown := printed(t, keyboard, screen)
			for _, want := range []string{
				"~/alpha is in use and cannot be unmounted. These processes are using it:",
				"PID", "COMMAND", "DIRECTORY",
				"4101", "nvim AGENTS.md", "~/alpha",
				"4102", "sleep 60", "~/alpha/docs",
				"Stop these processes and unmount? [y/N] ",
			} {
				if !strings.Contains(shown, want) {
					t.Errorf("screen lacks %q:\n%s", want, shown)
				}
			}
		})
	}
}

func TestBusyMountTableFitsTheScreen(t *testing.T) {
	const columns = 60
	keyboard, screen := openTerminal(t, columns)
	confirm := confirmStop(screen, screen)
	if _, err := keyboard.WriteString("n\n"); err != nil {
		t.Fatal(err)
	}
	command := "editor " + strings.Repeat("--flag=value ", 20)

	confirm("/srv/alpha", []holder.Process{{PID: 4101, Command: command, Dir: "/srv/alpha"}})

	shown := printed(t, keyboard, screen)
	if !strings.Contains(shown, "4101") || !strings.Contains(shown, "editor") {
		t.Fatalf("table lost the process:\n%s", shown)
	}
	_, table, _ := strings.Cut(shown, "These processes are using it:\n")
	for line := range strings.Lines(table) {
		if width := utf8.RuneCountInString(strings.TrimRight(line, "\n")); width > columns {
			t.Errorf("line is %d columns wide on a %d-column screen: %q", width, columns, line)
		}
	}
}

func TestNoBusyMountQuestionWithoutAPerson(t *testing.T) {
	keyboard, screen := openTerminal(t, 100)
	defer func() { _ = keyboard.Close() }() // also closed by the cleanup
	var buffer bytes.Buffer
	tests := []struct {
		name  string
		agent string
		in    io.Reader
		out   io.Writer
	}{
		{"agent mode", "1", screen, screen},
		{"input is not a terminal", "", &buffer, screen},
		{"output is not a terminal", "", screen, &buffer},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("AGENT", tt.agent)
			if confirmStop(tt.in, tt.out) != nil {
				t.Fatal("a question would be asked with nobody to answer it")
			}
		})
	}
}
