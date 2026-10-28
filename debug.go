package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// The debug endpoints let someone help from the other side of the tunnel
// without being at the machine: run the diagnose report, and try connection
// strings and statements until the right combination is found. They are on by
// default; "debug": false in the config file turns them off.
//
// They can only reach this machine. Every DSN's host must be local (or the host
// the configured DSN already uses), so the endpoints cannot be used to attack
// other servers on the customer's network. They never change the config file,
// and DSNs, passwords and result values are not logged.

const (
	maxProbes    = 20
	maxProbeRows = 100
)

type debugEndpoints struct {
	hosts map[string]bool // hosts, besides this machine, that a DSN may name
	busy  sync.Mutex      // one diagnose report at a time
}

// newDebug allows, besides this machine, the host of the configured DSN.
func newDebug(configuredDSN string) *debugEndpoints {
	d := &debugEndpoints{hosts: map[string]bool{}}
	if hosts, err := dsnHosts(configuredDSN); err == nil {
		for _, h := range hosts {
			d.hosts[strings.ToLower(h)] = true
		}
	}
	return d
}

func (d *debugEndpoints) register(mux *http.ServeMux) {
	mux.HandleFunc("GET /debug/diagnose", d.diagnose)
	mux.HandleFunc("POST /debug/diagnose", d.diagnose)
	mux.HandleFunc("POST /debug/probe", d.probe)
}

// dsnHosts returns every host a DSN would connect to. It fails for forms it
// cannot read and for options that override the host, so that the check in
// checkDSN cannot be sidestepped.
func dsnHosts(dsn string) ([]string, error) {
	var hostPort string
	switch {
	case strings.Contains(dsn, "://"):
		u, err := url.Parse(dsn)
		if err != nil {
			return nil, errors.New("cannot parse the DSN as a URL")
		}
		for k := range u.Query() {
			switch strings.ToLower(k) {
			case "host", "hostaddr", "server", "addr", "address", "data source":
				return nil, fmt.Errorf("the %q option is not allowed in a debug DSN; put the host in the URL", k)
			}
		}
		hostPort = u.Host
	case strings.Contains(dsn, "@tcp("):
		rest := dsn[strings.LastIndex(dsn, "@tcp(")+len("@tcp("):]
		end := strings.Index(rest, ")")
		if end < 0 {
			return nil, errors.New("cannot parse the MySQL DSN")
		}
		hostPort = rest[:end]
	default:
		return nil, errors.New("unsupported DSN form: use sqlserver://..., postgres://... or user:pass@tcp(host:port)/db")
	}
	var hosts []string
	for _, hp := range strings.Split(hostPort, ",") { // pgx accepts several hosts
		h, _, err := net.SplitHostPort(hp)
		if err != nil {
			h = hp
		}
		hosts = append(hosts, strings.Trim(h, "[]"))
	}
	return hosts, nil
}

var localNames = map[string]bool{"": true, "localhost": true, ".": true, "(local)": true}

func isLocalHost(h string) bool {
	h = strings.ToLower(h)
	if localNames[h] {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	name, _ := os.Hostname()
	return h == strings.ToLower(name)
}

// checkDSN reports why a DSN may not be used through the debug endpoints.
func (d *debugEndpoints) checkDSN(dsn string) error {
	hosts, err := dsnHosts(dsn)
	if err != nil {
		return err
	}
	for _, h := range hosts {
		if !isLocalHost(h) && !d.hosts[strings.ToLower(h)] {
			return fmt.Errorf("host %q is not allowed: debug endpoints only connect to this machine or to the host in the configured DSN", h)
		}
	}
	return nil
}

// flushWriter sends each write to the client at once, so a long report shows
// up line by line and the connection is not idle for minutes.
type flushWriter struct {
	w http.ResponseWriter
	f http.Flusher
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if fw.f != nil {
		fw.f.Flush()
	}
	return n, err
}

type diagnoseRequest struct {
	DSN      string `json:"dsn"`
	User     string `json:"user"`
	Password string `json:"password"`
	Database string `json:"database"`
}

// diagnose runs the same read-only report as the diagnose command, as the
// account the service runs as. GET uses the defaults; POST takes optional JSON
// {"database","user","password","dsn"}.
func (d *debugEndpoints) diagnose(w http.ResponseWriter, r *http.Request) {
	var req diagnoseRequest
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			http.Error(w, "body must be JSON like {\"database\":\"...\",\"user\":\"...\",\"password\":\"...\",\"dsn\":\"...\"}", http.StatusBadRequest)
			return
		}
	}
	if req.Database == "" {
		req.Database = "master"
	}
	if req.DSN != "" {
		if err := d.checkDSN(req.DSN); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if !d.busy.TryLock() {
		http.Error(w, "a diagnose report is already running", http.StatusTooManyRequests)
		return
	}
	defer d.busy.Unlock()

	log.Printf("debug: diagnose started (database=%s sqlLogin=%v dsnGiven=%v)", req.Database, req.User != "", req.DSN != "")
	began := time.Now()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	f, _ := w.(http.Flusher)
	rep := &report{w: flushWriter{w, f}}
	diagnoseReport(rep, diagOptions{DSN: req.DSN, User: req.User, Password: req.Password, Database: req.Database})
	log.Printf("debug: diagnose finished in %s (%d problem(s), %d warning(s))", time.Since(began).Round(time.Millisecond), rep.fails, rep.warns)
}

type probeRequest struct {
	DSN  string   `json:"dsn"`
	DSNs []string `json:"dsns"`
	Type string   `json:"type"` // optional; guessed from the DSN
	SQL  string   `json:"sql"`  // optional; run after connecting, first rows returned
}

type debugProbeResult struct {
	Target    string   `json:"target"` // the DSN with the password masked
	OK        bool     `json:"ok"`
	Millis    int64    `json:"ms"`
	Error     string   `json:"error,omitempty"`
	Hint      string   `json:"hint,omitempty"`
	Columns   []string `json:"columns,omitempty"`
	Types     []string `json:"types,omitempty"`
	Rows      [][]any  `json:"rows,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
}

// probe tries each given DSN in turn: connect, and if "sql" is given run it and
// return the first rows. All DSNs are tried even if earlier ones fail, so one
// request can test many combinations.
func (d *debugEndpoints) probe(w http.ResponseWriter, r *http.Request) {
	var req probeRequest
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "body must be JSON like {\"dsn\":\"...\"} or {\"dsns\":[...]}, with optional \"sql\" and \"type\"", http.StatusBadRequest)
		return
	}
	dsns := req.DSNs
	if req.DSN != "" {
		dsns = append([]string{req.DSN}, dsns...)
	}
	if len(dsns) == 0 || len(dsns) > maxProbes {
		http.Error(w, fmt.Sprintf("give between 1 and %d DSNs", maxProbes), http.StatusBadRequest)
		return
	}
	results := make([]debugProbeResult, 0, len(dsns))
	for _, dsn := range dsns {
		res := d.probeOne(r.Context(), dsn, req.Type, req.SQL)
		log.Printf("debug: probe ok=%v took=%dms target=%s", res.OK, res.Millis, res.Target)
		results = append(results, res)
	}
	writeJSON(w, http.StatusOK, map[string]any{"results": results})
}

func (d *debugEndpoints) probeOne(ctx context.Context, dsn, dbType, query string) (res debugProbeResult) {
	res.Target = describeDSN(dsn)
	began := time.Now()
	defer func() { res.Millis = time.Since(began).Milliseconds() }()
	fail := func(err error) debugProbeResult {
		res.Error, res.Hint = err.Error(), explain(err.Error())
		return res
	}

	if err := d.checkDSN(dsn); err != nil {
		res.Error = err.Error()
		return res
	}
	if dbType == "" {
		dbType = inferType(dsn)
	}
	driver, err := lookupDriver(dbType)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return fail(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	pingCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return fail(err)
	}
	res.OK = true
	if strings.TrimSpace(query) == "" {
		return res
	}

	qctx, qcancel := context.WithTimeout(ctx, 60*time.Second)
	defer qcancel()
	rows, err := db.QueryContext(qctx, query)
	if err != nil {
		res.OK = false
		return fail(err)
	}
	defer rows.Close()
	if res.Columns, err = rows.Columns(); err != nil {
		res.OK = false
		return fail(err)
	}
	res.Types = make([]string, len(res.Columns))
	if cts, err := rows.ColumnTypes(); err == nil {
		for i, ct := range cts {
			res.Types[i] = strings.ToUpper(ct.DatabaseTypeName())
		}
	}
	vals := make([]any, len(res.Columns))
	ptrs := make([]any, len(vals))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	for rows.Next() {
		if len(res.Rows) == maxProbeRows {
			res.Truncated = true
			break
		}
		if err := rows.Scan(ptrs...); err != nil {
			res.OK = false
			return fail(err)
		}
		row := make([]any, len(vals))
		for i, v := range vals {
			row[i] = cellValue(v, res.Types[i])
		}
		res.Rows = append(res.Rows, row)
	}
	if err := rows.Err(); err != nil {
		res.OK = false
		return fail(err)
	}
	return res
}
