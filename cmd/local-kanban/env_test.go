package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(p, []byte("# comment\n\nexport LK_A=1\nLK_B = \"two words\"\nLK_C='x=y'\nLK_SET=file\n"), 0o600)
	t.Setenv("LK_SET", "env") // real env wins
	for _, k := range []string{"LK_A", "LK_B", "LK_C"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
	if err := loadEnvFile(p); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"LK_A": "1", "LK_B": "two words", "LK_C": "x=y", "LK_SET": "env"}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if err := loadEnvFile(filepath.Join(t.TempDir(), "missing")); err != nil {
		t.Errorf("missing file: %v", err)
	}
	os.WriteFile(p, []byte("NOEQUALS\n"), 0o600)
	if err := loadEnvFile(p); err == nil {
		t.Error("accepted a line without =")
	}
}
