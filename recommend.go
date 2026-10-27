package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// diagnose ends with a recommended config.json. The DSN in it has been tested
// against the server, never guessed.

var systemDatabases = map[string]bool{"master": true, "tempdb": true, "model": true, "msdb": true}

const placeholderDB = "YOUR_DATABASE"

// probeResult is the first endpoint and option set that logged in.
type probeResult struct {
	EP   endpoint
	Opts string
	DBs  []string // user databases the login can open
}

// pickDatabase chooses the database the recommended DSN names: the one given
// with --database, else the only user database the login can open, else a
// placeholder for the person to fill in.
func pickDatabase(flagDB string, dbs []string) string {
	if !strings.EqualFold(flagDB, "master") {
		return flagDB
	}
	if len(dbs) == 1 {
		return dbs[0]
	}
	return placeholderDB
}

// instanceFor returns the named instance SQL Browser reports on a TCP port, or
// "" when there is none (a default instance is reached by port alone).
func instanceFor(browser []browserInfo, port int) string {
	for _, b := range browser {
		name := b["InstanceName"]
		if b["tcp"] == strconv.Itoa(port) && name != "" && !strings.EqualFold(name, "MSSQLSERVER") {
			return name
		}
	}
	return ""
}

// configJSON renders a complete config.json around a DSN.
func configJSON(dsn string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false) // keep & readable instead of &
	enc.Encode(dsn)
	return fmt.Sprintf("{\n  \"type\": \"mssql\",\n  \"dsn\": %s,\n  \"port\": \"8080\",\n  \"queryTimeoutSeconds\": 1800\n}", strings.TrimSpace(buf.String()))
}

func (r *report) block(text string) {
	for _, l := range strings.Split(text, "\n") {
		fmt.Fprintf(r.w, "       %s\n", l)
	}
}

func diagRecommend(r *report, res *probeResult, flagDB, user, password, configured string) {
	r.section("Recommended config.json")
	if res == nil {
		if configured != "" {
			r.ok("the DSN in config.json already connects: keep it")
		} else {
			r.warn("no working connection was found, so there is nothing to recommend yet")
			r.hint("fix the FAIL lines above and run diagnose again")
		}
		return
	}

	db := pickDatabase(flagDB, res.DBs)
	host, port := res.EP.Host, res.EP.Port
	pw := func(real bool) string {
		if user == "" {
			return ""
		}
		if real {
			return password
		}
		return "PASSWORD"
	}
	// Verify against the real database when we know it; otherwise the probe on
	// master (already successful) is the evidence.
	verifyDB := db
	if db == placeholderDB {
		verifyDB = flagDB
	}

	portDSN := func(real bool) string { return buildDSN(host, port, verifyDB, user, pw(real), res.Opts) }
	chosen := func(real bool) string { return portDSN(real) }
	how := fmt.Sprintf("by TCP port %d", port)

	if browser, err := queryBrowser(); err == nil && !strings.Contains(host, ":") {
		if inst := instanceFor(browser, port); inst != "" {
			instDSN := func(real bool) string { return buildInstanceDSN(host, inst, verifyDB, user, pw(real), res.Opts) }
			if err := tryPing("sqlserver", instDSN(true)); err == nil {
				chosen = instDSN
				how = fmt.Sprintf("by instance name %s (SQL Browser resolves the port, which can change at restart)", inst)
			} else {
				r.info("the instance-name form (%s) did not connect (%v), so the port form is recommended", inst, err)
			}
		}
	}

	show := chosen(false)
	if db == placeholderDB {
		show = strings.Replace(show, "database=master", "database="+placeholderDB, 1)
	}
	r.ok("recommended connection, %s:", how)
	r.block(configJSON(show))

	if db == placeholderDB {
		r.warn("replace %s with your database name (or run diagnose again with --database NAME to have it checked)", placeholderDB)
		if len(res.DBs) > 1 {
			r.info("databases this login can open: %s", strings.Join(res.DBs, ", "))
		}
	} else if err := tryPing("sqlserver", chosen(true)); err != nil {
		r.warn("the recommended DSN did not connect to database %q: %v", db, err)
		r.hint(explain(err.Error()))
	} else {
		r.ok("verified: it connects to database %q", db)
	}
	if user != "" {
		r.info("replace PASSWORD with the real password (URL-encode characters such as @ : / # ? %% & +)")
	} else {
		r.info("no username or password: Windows authentication signs in as the account the app runs as")
		r.hint("diagnose tested the account you ran it as. The service (plain 'install') runs as LocalSystem; if the log shows a login failure for NT AUTHORITY\\SYSTEM use 'install --current-user' or '--at-logon', or a SQL login (create-sql-login)")
	}
	if configured != "" {
		r.info("the DSN currently in config.json also connects, so changing it is optional")
	}
}
