package main

import (
	"strings"
	"testing"
)

func TestParseBrowser(t *testing.T) {
	msg := "ServerName;HOST1;InstanceName;SQLEXPRESS;IsClustered;No;Version;15.0.2000.5;tcp;49733;;" +
		"ServerName;HOST1;InstanceName;MSSQLSERVER;IsClustered;No;Version;16.0.1000.6;tcp;1433;np;pipe-name;;"
	b := append([]byte{0x05, byte(len(msg)), byte(len(msg) >> 8)}, msg...)
	got := parseBrowser(b)
	if len(got) != 2 || got[0]["InstanceName"] != "SQLEXPRESS" || got[0]["tcp"] != "49733" || got[1]["tcp"] != "1433" {
		t.Fatalf("parseBrowser = %v", got)
	}
	if parseBrowser([]byte{1, 2}) != nil || parseBrowser(nil) != nil {
		t.Fatal("garbage must give nil")
	}
}

func TestParseNetstatAndTasklist(t *testing.T) {
	ns := `
Active Connections

  Proto  Local Address          Foreign Address        State           PID
  TCP    0.0.0.0:1433           0.0.0.0:0              LISTENING       4568
  TCP    [::]:1433              [::]:0                 LISTENING       4568
  TCP    192.168.1.5:50000      10.0.0.1:443           ESTABLISHED     999
  TCP    127.0.0.1:8080         0.0.0.0:0              LISTENING       77
`
	got := parseNetstat(ns)
	if len(got[4568]) != 2 || got[4568][1].Addr != "::" || got[4568][1].Port != 1433 || len(got[999]) != 0 || got[77][0].Port != 8080 {
		t.Fatalf("parseNetstat = %v", got)
	}
	tl := "\"sqlservr.exe\",\"4568\",\"Services\",\"0\",\"1,234,000 K\"\r\nINFO: nothing\r\n"
	if p := parseTasklist(tl); len(p) != 1 || p[0] != 4568 {
		t.Fatalf("parseTasklist = %v", p)
	}
	if p := parseTasklist("INFO: No tasks are running which match the specified criteria."); len(p) != 0 {
		t.Fatalf("parseTasklist none = %v", p)
	}
}

func TestExplainKnownErrors(t *testing.T) {
	for msg, want := range map[string]string{
		"dial tcp 127.0.0.1:1433: connectex: No connection could be made because the target machine actively refused it.": "TCP/IP",
		"TLS Handshake failed: tls: server selected unsupported protocol version 301":                                     "tlsmin=1.0",
		"x509: certificate signed by unknown authority":                                                                   "TrustServerCertificate",
		"mssql: Login failed for user 'pluto'. (18456)":                                                                   "ERRORLOG",
		"mssql: Cannot open database \"X\" requested by the login. (4060)":                                                "database",
	} {
		if got := explain(msg); !strings.Contains(got, want) {
			t.Errorf("explain(%q) = %q, want mention of %q", msg, got, want)
		}
	}
	if explain("something unknown") != "" {
		t.Error("unknown error should have no hint")
	}
}

func TestBuildDSN(t *testing.T) {
	if got, want := buildDSN("localhost", 1433, "master", "", "", "encrypt=disable"), "sqlserver://localhost:1433?database=master&encrypt=disable"; got != want {
		t.Errorf("windows auth DSN = %q, want %q", got, want)
	}
	got := buildDSN("localhost", 1433, "d", "sa", "p@ss:w/rd", "")
	if !strings.Contains(got, "sa:p%40ss%3Aw%2Frd@") {
		t.Errorf("password must be URL-encoded: %q", got)
	}
}
