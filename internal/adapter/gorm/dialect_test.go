package gorm

import (
	"fmt"
	"testing"

	"github.com/ncruces/go-sqlite3"
)

func TestSQLiteRetryCodes(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"busy", sqlite3.BUSY, true},
		{"snapshot", sqlite3.BUSY_SNAPSHOT, true},
		{"locked", sqlite3.LOCKED, true},
		{"constraint", sqlite3.CONSTRAINT, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableError(fmt.Errorf("write: %w", tc.err)); got != tc.want {
				t.Fatalf("retryable = %v, want %v", got, tc.want)
			}
		})
	}
}
