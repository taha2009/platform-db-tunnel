package main

import (
	"fmt"
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
