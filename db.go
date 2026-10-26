package main

import (
	"fmt"
	"net/url"
	"sort"
	"strings"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
)

// drivers maps type to the name registered with database/sql. To support
// another database, import its driver above and add an entry here.
var drivers = map[string]string{
	"mssql":    "sqlserver",
	"postgres": "pgx",
	"mysql":    "mysql",
}

func lookupDriver(dbType string) (string, error) {
	if d, ok := drivers[strings.ToLower(dbType)]; ok {
		return d, nil
	}
	names := make([]string, 0, len(drivers))
	for n := range drivers {
		names = append(names, n)
	}
	sort.Strings(names)
	return "", fmt.Errorf("config: unknown type %q (supported: %s)", dbType, strings.Join(names, ", "))
}

// describeDSN summarises a DSN for the log without ever including the password:
// host, database, whether a user is set, and the options, with any password-like
// option masked.
func describeDSN(dsn string) string {
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err != nil {
			return "(unparseable DSN)"
		}
		q := u.Query()
		db := strings.TrimPrefix(u.Path, "/")
		for k, v := range q {
			if strings.EqualFold(k, "database") && len(v) > 0 {
				db = v[0]
			}
		}
		auth := "no user (Windows integrated auth if SQL Server)"
		if u.User != nil && u.User.Username() != "" {
			auth = "user=" + u.User.Username()
		}
		opts := make([]string, 0, len(q))
		for k, v := range q {
			if strings.EqualFold(k, "database") {
				continue
			}
			val := v[0]
			if lk := strings.ToLower(k); strings.Contains(lk, "pass") || strings.Contains(lk, "pwd") {
				val = "***"
			}
			opts = append(opts, k+"="+val)
		}
		sort.Strings(opts)
		return fmt.Sprintf("host=%s database=%q %s options=[%s]", u.Host, db, auth, strings.Join(opts, " "))
	}
	// MySQL style: user:pass@tcp(host:port)/db?opts
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return "(no user in DSN)"
	}
	user, _, _ := strings.Cut(dsn[:at], ":")
	return fmt.Sprintf("user=%s %s", user, dsn[at+1:])
}
