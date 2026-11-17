package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/holder"
)

func TestBusyMountTableLimitsEachPIDToFiveLines(t *testing.T) {
	tests := []struct {
		name    string
		command string
		dir     string
		columns uint16
		want    []string
	}{
		{"single line", "node worker.js", "/srv/alpha", 100, []string{"node worker.js"}},
		{"exactly five lines", "node -e\nline two\nline three\nline four\nline five", "/srv/alpha", 100,
			[]string{"node -e", "line two", "line three", "line four", "line five"}},
		{"six lines", "node -e\nline two\nline three\nline four\nline five\nhidden", "/srv/alpha", 100,
			[]string{"node -e", "line two", "line three", "line four", "line five…"}},
		{"embedded code", "node -e\nconst fs = require('fs');\nconst pid = 123;\nfunction kill() {\n  process.kill(pid);\n}\n" + strings.Repeat("hidden\n", 100), "/srv/alpha", 100,
			[]string{"node -e", "const fs = require('fs');", "const pid = 123;", "function kill() {", "process.kill(pid);…"}},
		{"blank lines", "node -e\n\nline three\n\nline five\nhidden", "/srv/alpha", 100,
			[]string{"node -e", "line three", "line five…"}},
		{"narrow screen", strings.Repeat("long command argument ", 20) + "\ntwo\nthree\nfour\nfive\nhidden", "/srv/alpha", 60,
			[]string{"long command arg…", "two", "three", "four", "five…"}},
		{"multiline directory", "node worker.js", "/srv/alpha\ntwo\nthree\nfour\nfive\nhidden", 100,
			[]string{"node worker.js", "", "", "", ""}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, screen := openTerminal(t, tt.columns)
			t.Cleanup(func() { _ = screen.Close() }) // no further terminal output is needed
			holders := []holder.Process{
				{PID: 4101, Command: tt.command, Dir: tt.dir},
				{PID: 4102, Command: "sleep 60", Dir: "/srv/beta"},
			}
			var out bytes.Buffer
			if err := showHolders(&out, screen, "/srv/alpha", holders); err != nil {
				t.Fatal(err)
			}
			shown := out.String()
			rows := make(map[string][][]string)
			var pid string
			for line := range strings.Lines(shown) {
				if !strings.HasPrefix(line, "│") {
					continue
				}
				if utf8.RuneCountInString(strings.TrimSuffix(line, "\n")) > int(tt.columns) {
					t.Fatalf("table exceeds screen width: %q", line)
				}
				cells := strings.Split(line, "│")[1:4]
				for i := range cells {
					cells[i] = strings.TrimSpace(cells[i])
				}
				if cells[0] == "PID" {
					continue
				}
				if cells[0] != "" {
					pid = cells[0]
				}
				rows[pid] = append(rows[pid], cells)
			}
			if len(rows["4101"]) != len(tt.want) || len(rows["4102"]) != 1 {
				t.Fatalf("PID row heights = %d, %d; want %d, 1:\n%s", len(rows["4101"]), len(rows["4102"]), len(tt.want), shown)
			}
			for i, want := range tt.want {
				if got := rows["4101"][i][1]; got != want {
					t.Errorf("command line %d = %q, want %q", i+1, got, want)
				}
			}
			if strings.Contains(shown, "hidden") || !strings.Contains(shown, "sleep 60") {
				t.Fatalf("omitted text leaked or following PID disappeared:\n%s", shown)
			}
			if tt.name == "multiline directory" && rows["4101"][4][2] != "five…" {
				t.Errorf("directory truncation missing:\n%s", shown)
			}
		})
	}
}
