package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"

	helptree "github.com/alexgorbatchev/cobra-help-tree/v2"
	"github.com/olekukonko/tablewriter"
	"github.com/olekukonko/tablewriter/tw"
	"golang.org/x/term"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/holder"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/pathname"
	"github.com/alexgorbatchev/workspace-overlay-cli/internal/session"
)

// confirmStop returns the question to ask when processes keep a mount busy:
// it shows them on out and reads the answer from in. It returns nil when
// there is nobody to ask, which is in agent mode and whenever in or out is
// not a terminal.
func confirmStop(in io.Reader, out io.Writer) session.Confirm {
	screen, ok := terminal(out)
	if _, typed := terminal(in); helptree.IsAgentMode() || !typed || !ok {
		return nil
	}
	// Several mounts can be busy at once; their questions take turns.
	var turn sync.Mutex
	answers := bufio.NewReader(in)
	return func(target string, holders []holder.Process) bool {
		turn.Lock()
		defer turn.Unlock()
		if err := showHolders(out, screen, target, holders); err != nil {
			return false
		}
		if _, err := fmt.Fprint(out, "Stop these processes and unmount? [y/N] "); err != nil {
			return false
		}
		// An unreadable answer leaves line empty, which declines.
		line, _ := answers.ReadString('\n')
		answer := strings.ToLower(strings.TrimSpace(line))
		return answer == "y" || answer == "yes"
	}
}

// terminal returns the file behind stream when it is a terminal.
func terminal(stream any) (*os.File, bool) {
	file, ok := stream.(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return nil, false
	}
	return file, true
}

// showHolders prints the processes that use target as a table no wider than
// the screen.
func showHolders(out io.Writer, screen *os.File, target string, holders []holder.Process) error {
	if _, err := fmt.Fprintf(out, "\n%s is in use and cannot be unmounted. These processes are using it:\n", pathname.Display(target)); err != nil {
		return err
	}
	options := []tablewriter.Option{tablewriter.WithRowAutoWrap(tw.WrapTruncate)}
	if width, _, err := term.GetSize(int(screen.Fd())); err == nil && width > 0 {
		options = append(options, tablewriter.WithMaxWidth(width))
	}
	table := tablewriter.NewTable(out, options...)
	table.Header("PID", "Command", "Directory")
	for _, p := range holders {
		if err := table.Append(strconv.Itoa(p.PID), p.Command, pathname.Display(p.Dir)); err != nil {
			return err
		}
	}
	return table.Render()
}
