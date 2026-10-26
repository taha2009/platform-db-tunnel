package main

import (
	"strings"
	"testing"
)

func TestDescribeDSNHidesPassword(t *testing.T) {
	for _, dsn := range []string{
		"sqlserver://sa:s3cret@localhost:1433?database=d&encrypt=disable",
		"sqlserver://localhost?database=d&password=s3cret",
		"postgres://u:s3cret@localhost:5432/d",
		"u:s3cret@tcp(localhost:3306)/d",
	} {
		if got := describeDSN(dsn); strings.Contains(got, "s3cret") {
			t.Errorf("%q leaked password: %s", dsn, got)
		}
	}
	if got := describeDSN("sqlserver://localhost:1433?database=d"); !strings.Contains(got, "Windows integrated") {
		t.Errorf("no-user DSN not flagged: %s", got)
	}
}
