package storage

import "testing"

func TestRebind(t *testing.T) {
	cases := []struct {
		dialect Dialect
		query   string
		want    string
	}{
		{SQLite, "SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = ? AND b = ?"},
		{Postgres, "SELECT * FROM t WHERE a = ? AND b = ?", "SELECT * FROM t WHERE a = $1 AND b = $2"},
		{Postgres, "SELECT 1", "SELECT 1"},
	}
	for _, c := range cases {
		db := &DB{Dialect: c.dialect}
		if got := db.Rebind(c.query); got != c.want {
			t.Errorf("Rebind(%q) dialect=%s = %q, want %q", c.query, c.dialect, got, c.want)
		}
	}
}

func TestDialectString(t *testing.T) {
	if Postgres.String() != "postgres" {
		t.Errorf("Postgres.String() = %q", Postgres.String())
	}
	if SQLite.String() != "sqlite" {
		t.Errorf("SQLite.String() = %q", SQLite.String())
	}
}
