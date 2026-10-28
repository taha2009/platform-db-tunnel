package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	maxBodyBytes = 1 << 20
)

// version is the release tag (e.g. v1.0.11), set at build time by the release
// workflow with -ldflags "-X main.version=<tag>"; "dev" for a local build.
var version = "dev"

type queryRequest struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"` // bound positionally as @p1, @p2, ...
}

// newHandler builds the routes. db is nil when there is no config file, and
// dbg is nil when the debug endpoints are turned off.
func newHandler(db *sql.DB, queryTimeout time.Duration, dbg *debugEndpoints) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if db == nil {
			http.Error(w, "no database configured", http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			log.Printf("healthz: database unreachable: %v", err)
			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"version": version})
	})
	if db == nil {
		mux.HandleFunc("POST /query", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no database configured: there is no config.json or config.txt next to the exe"})
		})
	} else {
		mux.HandleFunc("POST /query", queryHandler(db, queryTimeout))
	}
	if dbg != nil {
		dbg.register(mux)
	}
	return mux
}

// binaryTypes are the driver type names whose values are raw bytes rather than
// text; they are sent base64-encoded because JSON strings cannot carry
// arbitrary bytes (a plain string() conversion would corrupt them).
var binaryTypes = map[string]bool{
	"BINARY": true, "VARBINARY": true, "IMAGE": true, "TIMESTAMP": true, "ROWVERSION": true, // SQL Server
	"BYTEA": true,                                                         // PostgreSQL
	"BLOB":  true, "TINYBLOB": true, "MEDIUMBLOB": true, "LONGBLOB": true, // MySQL
}

// guidString formats SQL Server's 16-byte UNIQUEIDENTIFIER wire value, whose
// first three groups are little-endian, as the canonical 8-4-4-4-12 text.
func guidString(b []byte) string {
	return fmt.Sprintf("%02x%02x%02x%02x-%02x%02x-%02x%02x-%02x%02x-%02x%02x%02x%02x%02x%02x",
		b[3], b[2], b[1], b[0], b[5], b[4], b[7], b[6], b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

// cellValue converts one scanned value for JSON. Byte slices become text,
// except binary columns (base64) and SQL Server GUIDs (canonical text).
func cellValue(v any, typeName string) any {
	b, ok := v.([]byte)
	if !ok {
		return v
	}
	switch {
	case typeName == "UNIQUEIDENTIFIER" && len(b) == 16:
		return guidString(b)
	case binaryTypes[typeName]:
		return base64.StdEncoding.EncodeToString(b)
	}
	return string(b)
}

// queryHandler streams the result as {"columns":[...],"types":[...],"rows":[...]} so memory
// use stays flat however many rows come back. Once streaming has begun the
// status is already 200, so a failure mid-result ends the document with
// {...],"error":"..."} instead; clients must check for an "error" key.
func queryHandler(db *sql.DB, timeout time.Duration) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req queryRequest
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.SQL) == "" {
			log.Printf("query rejected: body must be JSON with a non-empty \"sql\" (from %s)", r.RemoteAddr)
			http.Error(w, "body must be JSON with a non-empty \"sql\"", http.StatusBadRequest)
			return
		}

		// One log line per query. The SQL text is shortened and the bound args are
		// never logged, since they are the most likely place for sensitive values.
		began, nrows, outcome := time.Now(), 0, "ok"
		defer func() {
			log.Printf("query %s rows=%d took=%s sql=%q", outcome, nrows, time.Since(began).Round(time.Millisecond), brief(req.SQL))
		}()

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()

		rows, err := db.QueryContext(ctx, req.SQL, req.Args...)
		if err != nil {
			outcome = "error: " + err.Error()
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		defer rows.Close()

		cols, err := rows.Columns()
		if err != nil {
			outcome = "error: " + err.Error()
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}

		// The response is started lazily, on the first row or at the end, so an
		// error before any row is written can still be returned as a real 500.
		// "types" carries the driver's type name per column (upper case, "" when
		// the driver doesn't report one), so clients can tell dates, decimals
		// and base64 binary apart: JSON alone can't.
		types := make([]string, len(cols))
		if cts, err := rows.ColumnTypes(); err == nil {
			for i, ct := range cts {
				types[i] = strings.ToUpper(ct.DatabaseTypeName())
			}
		}

		var bw *bufio.Writer
		start := func() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			bw = bufio.NewWriter(w)
			colsJSON, _ := json.Marshal(cols)
			typesJSON, _ := json.Marshal(types)
			bw.WriteString(`{"columns":`)
			bw.Write(colsJSON)
			bw.WriteString(`,"types":`)
			bw.Write(typesJSON)
			bw.WriteString(`,"rows":[`)
		}
		fail := func(err error) {
			outcome = "error: " + err.Error()
			if bw == nil {
				writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
				return
			}
			msg, _ := json.Marshal(err.Error())
			bw.WriteString(`],"error":`)
			bw.Write(msg)
			bw.WriteString("}")
			bw.Flush()
		}

		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		for rows.Next() {
			if err := rows.Scan(ptrs...); err != nil {
				fail(err)
				return
			}
			for i, v := range vals {
				vals[i] = cellValue(v, types[i])
			}
			row, err := json.Marshal(vals)
			if err != nil {
				fail(err)
				return
			}
			if bw == nil {
				start()
			} else {
				bw.WriteByte(',')
			}
			bw.Write(row)
			nrows++
		}
		if err := rows.Err(); err != nil {
			fail(err)
			return
		}
		if bw == nil {
			start()
		}
		bw.WriteString("]}")
		bw.Flush()
	}
}

// brief collapses whitespace and shortens SQL for logging.
func brief(q string) string {
	q = strings.Join(strings.Fields(q), " ")
	if r := []rune(q); len(r) > 200 {
		return string(r[:200]) + "..."
	}
	return q
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}
