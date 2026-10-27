package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPickDatabase(t *testing.T) {
	if got := pickDatabase("ABS 26-27", []string{"a", "b"}); got != "ABS 26-27" {
		t.Errorf("explicit --database must win, got %q", got)
	}
	if got := pickDatabase("master", []string{"ACCDATA"}); got != "ACCDATA" {
		t.Errorf("a single user database should be chosen, got %q", got)
	}
	if got := pickDatabase("master", []string{"a", "b"}); got != placeholderDB {
		t.Errorf("several databases need a placeholder, got %q", got)
	}
	if got := pickDatabase("MASTER", nil); got != placeholderDB {
		t.Errorf("no databases need a placeholder, got %q", got)
	}
}

func TestInstanceFor(t *testing.T) {
	bi := []browserInfo{
		{"InstanceName": "MSSQLSERVER", "tcp": "1433"},
		{"InstanceName": "SQLEXPRESS", "tcp": "54372"},
	}
	if got := instanceFor(bi, 54372); got != "SQLEXPRESS" {
		t.Errorf("named instance = %q", got)
	}
	if got := instanceFor(bi, 1433); got != "" {
		t.Errorf("the default instance is reached by port, got %q", got)
	}
	if got := instanceFor(bi, 9999); got != "" {
		t.Errorf("unknown port = %q", got)
	}
}

func TestDSNForms(t *testing.T) {
	if got, want := buildDSN("localhost", 54372, "ABS 26-27", "", "", "encrypt=disable"), "sqlserver://localhost:54372?database=ABS%2026-27&encrypt=disable"; got != want {
		t.Errorf("port form = %q, want %q", got, want)
	}
	if got, want := buildInstanceDSN("localhost", "SQLEXPRESS", "ABS 26-27", "", "", "encrypt=disable"), "sqlserver://localhost/SQLEXPRESS?database=ABS%2026-27&encrypt=disable"; got != want {
		t.Errorf("instance form = %q, want %q", got, want)
	}
	if got := buildInstanceDSN("localhost", "SQLEXPRESS", "d", "u", "p@ss", ""); !strings.Contains(got, "u:p%40ss@localhost/SQLEXPRESS") {
		t.Errorf("instance form with a login = %q", got)
	}
	if got := buildDSN("localhost", 1, "a+b", "", "", ""); !strings.Contains(got, "database=a%2Bb") {
		t.Errorf("a literal + must stay encoded: %q", got)
	}
}

func TestConfigJSON(t *testing.T) {
	dsn := "sqlserver://localhost/SQLEXPRESS?database=ABS%2026-27&encrypt=disable"
	out := configJSON(dsn)
	escaped := string(rune(92)) + "u00" + "26" // what encoding/json writes for & by default
	if strings.Contains(out, escaped) || !strings.Contains(out, "&encrypt=disable") {
		t.Errorf("ampersand must stay readable:\n%s", out)
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if cfg["dsn"] != dsn || cfg["type"] != "mssql" || cfg["port"] != "8080" {
		t.Errorf("unexpected config: %v", cfg)
	}
}
