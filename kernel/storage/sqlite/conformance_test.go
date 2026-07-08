package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/iamdoubz/lasterp/kernel/storage/conformance"
)

func TestSQLiteConformance(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "conformance.db")

	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	if err := db.PingContext(context.Background()); err != nil {
		t.Fatalf("ping: %v", err)
	}

	conformance.Run(t, db)
}
