package logged

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"testing"

	"workspace-overlay/internal/scratch"
)

func TestMain(m *testing.M) {
	scratch.Main(m)
}

func TestClose(t *testing.T) {
	tmp := scratch.Write(t, filepath.Join(t.TempDir(), "test-file"), "content")
	f, err := os.Open(tmp)
	if err != nil {
		t.Fatal(err)
	}

	// Redirect logger to capture output
	var buf bytes.Buffer
	oldOutput := log.Writer()
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(oldOutput) })

	// Closing an open file should log nothing
	Close(f)
	if buf.Len() > 0 {
		t.Errorf("expected no log output on successful close, got: %s", buf.String())
	}

	// Closing a second time should log an error
	Close(f)
	logged := buf.String()
	if !bytes.Contains([]byte(logged), []byte("close file")) {
		t.Errorf("expected 'close file' in log, got: %s", logged)
	}
}
