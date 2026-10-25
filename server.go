package main

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	maxBodyBytes = 1 << 20
)

type queryRequest struct {
	SQL  string `json:"sql"`
	Args []any  `json:"args"` // bound positionally as @p1, @p2, ...
}

func newHandler(db *sql.DB, queryTimeout time.Duration) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := db.PingContext(ctx); err != nil {
			log.Printf("healthz: database unreachable: %v", err)
			http.Error(w, "db unreachable", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /query", queryHandler(db, queryTimeout))
	return mux
}

// queryHandler streams the result as {"columns":[...],"rows":[...]} so memory
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
		var bw *bufio.Writer
		start := func() {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			bw = bufio.NewWriter(w)
			colsJSON, _ := json.Marshal(cols)
			bw.WriteString(`{"columns":`)
			bw.Write(colsJSON)
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
				if b, ok := v.([]byte); ok {
					vals[i] = string(b)
				}
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
