package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const goodConfig = `{"type":"postgres","dsn":"postgres://u:p@localhost/db","port":"9000"}`

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindConfig(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := findConfig(dir); err == nil || !strings.Contains(err.Error(), "no config.json or config.txt") {
		t.Fatalf("empty dir: %v", err)
	}

	write(t, dir, "config.txt", goodConfig)
	p, ign, err := findConfig(dir)
	if err != nil || filepath.Base(p) != "config.txt" || len(ign) != 0 {
		t.Fatalf("config.txt only: %q %v %v", p, ign, err)
	}

	write(t, dir, "config.json", goodConfig)
	p, ign, err = findConfig(dir)
	if err != nil || filepath.Base(p) != "config.json" || len(ign) != 1 || filepath.Base(ign[0]) != "config.txt" {
		t.Fatalf("both present, json must win: %q %v %v", p, ign, err)
	}
}

func TestFindConfigHiddenExtension(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "config.json.txt", goodConfig)
	_, _, err := findConfig(dir)
	if err == nil || !strings.Contains(err.Error(), "config.json.txt") || !strings.Contains(err.Error(), "hiding") {
		t.Fatalf("want a hidden-extension hint, got %v", err)
	}
}

func TestParseConfig(t *testing.T) {
	c, err := parseConfig([]byte(goodConfig), "x")
	if err != nil || c.Addr != "127.0.0.1:9000" || c.Driver != "pgx" || c.QueryTimeout.Minutes() != 30 {
		t.Fatalf("parse: %+v %v", c, err)
	}

	// Notepad's UTF-8 byte-order mark must be accepted.
	bom := append([]byte{0xEF, 0xBB, 0xBF}, goodConfig...)
	if _, err := parseConfig(bom, "x"); err != nil {
		t.Fatalf("UTF-8 BOM: %v", err)
	}

	for name, in := range map[string][]byte{
		"utf16-le": append([]byte{0xFF, 0xFE}, goodConfig...),
		"utf16-be": append([]byte{0xFE, 0xFF}, goodConfig...),
	} {
		if _, err := parseConfig(in, "x"); err == nil || !strings.Contains(err.Error(), "UTF-8") {
			t.Errorf("%s: want a save-as-UTF-8 hint, got %v", name, err)
		}
	}
	if _, err := parseConfig([]byte(`{"type":"mssql"}`), "x"); err == nil || !strings.Contains(err.Error(), "dsn") {
		t.Errorf("missing dsn: %v", err)
	}
	if _, err := parseConfig([]byte(`{"dsn":"x","type":"oracle"}`), "x"); err == nil {
		t.Error("unknown type must fail")
	}
}
