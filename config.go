package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type config struct {
	// Addr is always loopback, so only processes on this machine can reach it.
	// The app does no authentication of its own.
	Addr   string
	Driver string // database/sql driver name
	DSN    string
	// QueryTimeout bounds one /query call, including reading all rows.
	QueryTimeout time.Duration
}

// fileConfig is config.json, which must sit beside the executable (a service's
// working directory is System32, so we can't rely on a relative path).
//
//	type  mssql (default), postgres or mysql
//	dsn   driver-specific connection string (required)
//	port  listen port on 127.0.0.1 (default 8080)
//	queryTimeoutSeconds  max duration of one query incl. reading rows (default 1800)
type fileConfig struct {
	Port                string `json:"port"`
	Type                string `json:"type"`
	DSN                 string `json:"dsn"`
	QueryTimeoutSeconds int    `json:"queryTimeoutSeconds"`
}

func loadConfig() (*config, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(filepath.Dir(exe), "config.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	var f fileConfig
	if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if f.DSN == "" {
		return nil, errors.New(`config: "dsn" is required in ` + path)
	}
	if f.Type == "" {
		f.Type = "mssql"
	}
	if f.Port == "" {
		f.Port = "8080"
	}
	if f.QueryTimeoutSeconds <= 0 {
		f.QueryTimeoutSeconds = 30 * 60
	}
	driver, err := lookupDriver(f.Type)
	if err != nil {
		return nil, err
	}
	return &config{
		Addr:         "127.0.0.1:" + f.Port,
		Driver:       driver,
		DSN:          f.DSN,
		QueryTimeout: time.Duration(f.QueryTimeoutSeconds) * time.Second,
	}, nil
}
