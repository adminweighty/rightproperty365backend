package migrate

import (
	"strings"
	"testing"
	"testing/fstest"

	"rightproperty365.com/backend/migrations"
)

// The embedded migrations must always be loadable: this catches a missing
// down file or a badly named file at test time rather than at deploy time.
// It does NOT execute the SQL (that needs a real Postgres).
func TestLoadEmbeddedMigrations(t *testing.T) {
	migs, err := Load(migrations.FS)
	if err != nil {
		t.Fatal(err)
	}
	if len(migs) == 0 {
		t.Fatal("no migrations found")
	}
	for i, m := range migs {
		if m.Version != i+1 {
			t.Fatalf("migration %d has version %d; versions must be contiguous from 0001", i, m.Version)
		}
		if strings.TrimSpace(m.Up) == "" || strings.TrimSpace(m.Down) == "" {
			t.Fatalf("migration %04d_%s has an empty script", m.Version, m.Name)
		}
	}
	if migs[0].Name != "init" {
		t.Fatalf("first migration = %q, want init", migs[0].Name)
	}
}

func TestLoadValidation(t *testing.T) {
	tests := []struct {
		name    string
		files   fstest.MapFS
		wantErr string
	}{
		{"missing down", fstest.MapFS{"0001_a.up.sql": {Data: []byte("SELECT 1")}}, "needs both"},
		{"bad name", fstest.MapFS{"init.sql": {Data: []byte("SELECT 1")}}, "does not match"},
		{"name mismatch", fstest.MapFS{
			"0001_a.up.sql":   {Data: []byte("SELECT 1")},
			"0001_b.down.sql": {Data: []byte("SELECT 1")},
		}, "two names"},
		{"sorted", fstest.MapFS{
			"0002_b.up.sql":   {Data: []byte("SELECT 2")},
			"0002_b.down.sql": {Data: []byte("SELECT 2")},
			"0001_a.up.sql":   {Data: []byte("SELECT 1")},
			"0001_a.down.sql": {Data: []byte("SELECT 1")},
			"embed.go":        {Data: []byte("package x")},
		}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			migs, err := Load(tt.files)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(migs) != 2 || migs[0].Version != 1 || migs[1].Version != 2 {
				t.Fatalf("unexpected order: %+v", migs)
			}
		})
	}
}
