package logged

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alexgorbatchev/workspace-overlay-cli/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func TestClose(t *testing.T) {
	f, err := os.Open(scratch.Write(t, filepath.Join(t.TempDir(), "test-file"), "content"))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	Close(f)
	if buf.Len() > 0 {
		t.Errorf("expected no log output on successful close, got: %s", buf.String())
	}
	// Closing an already closed file fails, which is the failure to log.
	Close(f)
	if got := buf.String(); !strings.Contains(got, "close file") {
		t.Errorf("expected 'close file' in log, got: %s", got)
	}
}
