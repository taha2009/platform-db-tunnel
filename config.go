package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type config struct {
	// Addr is always loopback, so only processes on this machine can reach it.
	// The app does no authentication of its own.
	Addr   string
	Driver string // database/sql driver name
	DSN    string
	Path   string // config file that was loaded
	// Ignored lists other config files found beside it (config.json wins over
	// config.txt), so the log can say so.
	Ignored []string
	// QueryTimeout bounds one /query call, including reading all rows.
	QueryTimeout time.Duration
	// Debug enables the /debug endpoints (on unless "debug": false).
	Debug bool
}

// noConfigError means no config file exists, as opposed to one that is broken.
// The service still starts then, with no database, so it can be diagnosed.
type noConfigError struct{ dir string }

func (e noConfigError) Error() string {
	return "config: no config.json or config.txt in " + e.dir
}

// defaultConfig is what the service runs with when there is no config file.
func defaultConfig() *config {
	return &config{Addr: "127.0.0.1:8080", QueryTimeout: 30 * time.Minute, Debug: true}
}

// configNames are the accepted file names, in order of preference. config.txt
// has exactly the same JSON content; it exists because it is easier to create
// and edit with Notepad on machines that hide file extensions.
var configNames = []string{"config.json", "config.txt"}

// fileConfig is the config file, which must sit beside the executable (a
// service's working directory is System32, so we can't rely on a relative path).
//
//	type  mssql, postgres or mysql (default: guessed from the dsn)
//	dsn   driver-specific connection string (required)
//	port  listen port on 127.0.0.1 (default 8080)
//	queryTimeoutSeconds  max duration of one query incl. reading rows (default 1800)
//	debug  false turns off the /debug endpoints (default true)
type fileConfig struct {
	Port                string `json:"port"`
	Type                string `json:"type"`
	DSN                 string `json:"dsn"`
	QueryTimeoutSeconds int    `json:"queryTimeoutSeconds"`
	Debug               *bool  `json:"debug"`
}

// findConfig returns the config file to use in dir and any other candidates
// that were ignored.
func findConfig(dir string) (path string, ignored []string, err error) {
	for _, n := range configNames {
		p := filepath.Join(dir, n)
		if fi, serr := os.Stat(p); serr == nil && !fi.IsDir() {
			if path == "" {
				path = p
			} else {
				ignored = append(ignored, p)
			}
		}
	}
	if path != "" {
		return path, ignored, nil
	}
	// The classic Windows mistake: with extensions hidden, Notepad saves
	// "config.json" as "config.json.txt".
	for _, n := range []string{"config.json.txt", "config.txt.txt"} {
		if _, serr := os.Stat(filepath.Join(dir, n)); serr == nil {
			return "", nil, fmt.Errorf("config: found %s in %s, but the real name is %s: Windows is hiding the .txt extension. Rename it to config.json (or config.txt)", n, dir, n[:len(n)-len(".txt")])
		}
	}
	return "", nil, noConfigError{dir}
}

// inferType guesses the database type from the DSN when "type" is not given.
func inferType(dsn string) string {
	l := strings.ToLower(dsn)
	switch {
	case strings.HasPrefix(l, "postgres://"), strings.HasPrefix(l, "postgresql://"):
		return "postgres"
	case strings.Contains(l, "@tcp(") && !strings.Contains(l, "://"), strings.Contains(l, "@unix("):
		return "mysql"
	}
	return "mssql"
}

// parseConfig reads the contents of a config file. A UTF-8 byte-order mark,
// which Notepad adds, is accepted.
func parseConfig(b []byte, path string) (*config, error) {
	b = bytes.TrimPrefix(b, []byte{0xEF, 0xBB, 0xBF})
	if bytes.HasPrefix(b, []byte{0xFF, 0xFE}) || bytes.HasPrefix(b, []byte{0xFE, 0xFF}) {
		return nil, fmt.Errorf("config: %s is saved as UTF-16; save it again as UTF-8 (Notepad: Save As > Encoding > UTF-8)", path)
	}
	var f fileConfig
	if t := bytes.TrimSpace(b); len(t) > 0 && t[0] != '{' {
		// Not JSON: the whole file is the DSN.
		f.DSN = string(t)
	} else if err := json.Unmarshal(b, &f); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	if f.DSN == "" {
		return nil, errors.New(`config: "dsn" is required in ` + path)
	}
	if f.Type == "" {
		f.Type = inferType(f.DSN)
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
		Path:         path,
		QueryTimeout: time.Duration(f.QueryTimeoutSeconds) * time.Second,
		Debug:        f.Debug == nil || *f.Debug,
	}, nil
}

func loadConfig() (*config, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	path, ignored, err := findConfig(filepath.Dir(exe))
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	cfg, err := parseConfig(b, path)
	if err != nil {
		return nil, err
	}
	cfg.Ignored = ignored
	return cfg, nil
}
