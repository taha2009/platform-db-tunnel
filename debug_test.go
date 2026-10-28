package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDSNHosts(t *testing.T) {
	d := newDebug("sqlserver://dbbox.lan/INST?database=x")
	for dsn, ok := range map[string]bool{
		"sqlserver://localhost/SQLEXPRESS?database=x": true,
		"sqlserver://127.0.0.1:1433?database=x":       true,
		"sqlserver://u:p@[::1]:1433?database=x":       true,
		"postgres://u:p@localhost/db":                 true,
		"u:p@tcp(127.0.0.1:3306)/db":                  true,
		"sqlserver://dbbox.lan:1433?database=y":       true, // the configured host
		"sqlserver://10.0.0.5:1433?database=x":        false,
		"sqlserver://other.example.com/INST":          false,
		"postgres://u:p@localhost,evil.com/db":        false, // second host
		"postgres://u:p@localhost/db?host=evil.com":   false, // host override
		"sqlserver://localhost?server=evil.com":       false,
		"u:p@tcp(evil.com:3306)/db":                   false,
		"server=evil.com;user id=sa;password=x":       false, // unsupported form
	} {
		if err := d.checkDSN(dsn); (err == nil) != ok {
			t.Errorf("checkDSN(%q) = %v, allowed want %v", dsn, err, ok)
		}
	}
}

func TestDebugRoutes(t *testing.T) {
	// A probe at a refused host never touches the network and masks the password.
	rec := httptest.NewRecorder()
	body := `{"dsns":["sqlserver://sa:secret@evil.example.com?database=x"]}`
	newHandler(nil, 0, newDebug("")).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/probe", strings.NewReader(body)))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "not allowed") || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("refused probe: %d %s", rec.Code, rec.Body)
	}

	// Without a database, /query and /healthz say so, /version still works.
	for _, tc := range []struct{ method, path string }{{"POST", "/query"}, {"GET", "/healthz"}} {
		rec = httptest.NewRecorder()
		newHandler(nil, 0, nil).ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"sql":"select 1"}`)))
		if rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s %s without db: %d", tc.method, tc.path, rec.Code)
		}
	}

	// Debug off: the routes do not exist.
	rec = httptest.NewRecorder()
	newHandler(nil, 0, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/probe", strings.NewReader(`{}`)))
	if rec.Code != http.StatusNotFound && rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("debug off: %d", rec.Code)
	}

	// Bad input.
	rec = httptest.NewRecorder()
	newHandler(nil, 0, newDebug("")).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/debug/probe", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("empty probe: %d", rec.Code)
	}
}
