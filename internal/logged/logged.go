// Package logged holds cleanup steps whose failure deserves a log line but
// must not change the caller's result.
package logged

import (
	"log"
	"os"
)

// Close closes f and logs a failure instead of returning it.
func Close(f *os.File) {
	if err := f.Close(); err != nil {
		log.Printf("close file: %v", err)
	}
}
