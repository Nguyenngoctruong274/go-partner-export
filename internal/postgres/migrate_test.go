package postgres

import (
	"io/fs"
	"testing"
	"testing/fstest"
)

// brokenFS holds a migration whose SQL does not run.
type brokenFS struct{}

func (brokenFS) Open(name string) (fs.File, error) {
	return fstest.MapFS{"migrations/0001_bad.sql": {Data: []byte("CREATE TABLE")}}.Open(name)
}

func TestLoadMigrations(t *testing.T) {
	ms, err := loadMigrations(migrationFS)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].version != 1 || ms[1].version != 2 {
		t.Fatalf("embedded migrations = %+v", ms)
	}
	bad := []fstest.MapFS{
		{},
		{"migrations/nounderscore.sql": {Data: []byte("x")}},
		{"migrations/abc_x.sql": {Data: []byte("x")}},
		{"migrations/0001_a.sql": {Data: []byte("x")}, "migrations/0001_b.sql": {Data: []byte("x")}},
	}
	for i, fsys := range bad {
		if _, err := loadMigrations(fsys); err == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
