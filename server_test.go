package main

import (
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeDriver returns canned rows for any query: "empty", "many" or "fail" (an
// error after two rows) selected by the SQL text.
type fakeDriver struct{}

func (fakeDriver) Open(string) (driver.Conn, error) { return fakeConn{}, nil }

type fakeConn struct{}

func (fakeConn) Prepare(q string) (driver.Stmt, error) { return fakeStmt{q}, nil }
func (fakeConn) Close() error                          { return nil }
func (fakeConn) Begin() (driver.Tx, error)             { return nil, errors.New("no tx") }

type fakeStmt struct{ q string }

func (fakeStmt) Close() error  { return nil }
func (fakeStmt) NumInput() int { return -1 }
func (fakeStmt) Exec([]driver.Value) (driver.Result, error) {
	return nil, errors.New("no exec")
}
func (s fakeStmt) Query([]driver.Value) (driver.Rows, error) {
	if s.q == "bad" {
		return nil, errors.New("syntax error")
	}
	return &fakeRows{q: s.q}, nil
}

type fakeRows struct {
	q string
	n int
}

func (*fakeRows) Columns() []string { return []string{"id", "name"} }
func (*fakeRows) Close() error      { return nil }
func (r *fakeRows) Next(dest []driver.Value) error {
	limit := map[string]int{"empty": 0, "many": 5000, "fail": 2}[r.q]
	if r.n >= limit {
		if r.q == "fail" {
			return errors.New("connection lost")
		}
		return io.EOF
	}
	dest[0], dest[1] = int64(r.n), []byte("n")
	r.n++
	return nil
}

func post(t *testing.T, sqlText string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	sql.Register("fake-"+t.Name(), fakeDriver{})
	db, err := sql.Open("fake-"+t.Name(), "")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"sql": sqlText})
	rec := httptest.NewRecorder()
	newHandler(db, time.Minute).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(string(body))))
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not valid JSON (%v): %.200s", err, rec.Body.String())
	}
	return rec, out
}

func TestQueryStreamsAllRows(t *testing.T) {
	rec, out := post(t, "many")
	if rec.Code != 200 || len(out["rows"].([]any)) != 5000 || out["error"] != nil {
		t.Fatalf("code=%d rows=%d error=%v", rec.Code, len(out["rows"].([]any)), out["error"])
	}
}

func TestQueryEmptyResult(t *testing.T) {
	rec, out := post(t, "empty")
	if rec.Code != 200 || len(out["rows"].([]any)) != 0 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestQueryErrorMidStreamIsValidJSONWithError(t *testing.T) {
	rec, out := post(t, "fail")
	if rec.Code != 200 || out["error"] != "connection lost" || len(out["rows"].([]any)) != 2 {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestQueryErrorBeforeRowsIs400(t *testing.T) {
	rec, out := post(t, "bad")
	if rec.Code != 400 || out["error"] == nil {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
}

func TestCellValue(t *testing.T) {
	guid := []byte{0x78, 0x56, 0x34, 0x12, 0xbc, 0x9a, 0xf0, 0xde, 1, 2, 3, 4, 5, 6, 7, 8}
	cases := []struct {
		in       any
		typeName string
		want     any
	}{
		{[]byte("12.50"), "MONEY", "12.50"},
		{[]byte{0, 255, 1}, "VARBINARY", "AP8B"},
		{guid, "UNIQUEIDENTIFIER", "12345678-9abc-def0-0102-030405060708"},
		{int64(7), "INT", int64(7)},
		{nil, "VARBINARY", nil},
	}
	for _, c := range cases {
		if got := cellValue(c.in, c.typeName); got != c.want {
			t.Errorf("cellValue(%v, %s) = %v, want %v", c.in, c.typeName, got, c.want)
		}
	}
}

func TestQueryReportsColumnTypes(t *testing.T) {
	_, out := post(t, "empty")
	if types, ok := out["types"].([]any); !ok || len(types) != 2 {
		t.Fatalf("types = %v", out["types"])
	}
}
