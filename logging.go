package main

import (
	"io"
	"log"
	"os"
	"path/filepath"

	"gopkg.in/natefinch/lumberjack.v2"
)

const logFileName = "platform-db-tunnel.log"

// setupLogging sends the standard logger to a rotating file beside the
// executable (like config.json, since a service's working directory is
// System32) and, when running in a console, to the console as well.
//
// A service has no usable stderr (writing to it fails, and io.MultiWriter
// stops at the first failing writer), so it logs to the file only. If the file
// can't be written, e.g. the folder isn't writable by this account, logging
// falls back to the console and says so.
func setupLogging(console bool) {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	path := filepath.Join(filepath.Dir(exe), logFileName)

	// lumberjack opens lazily, so probe writability up front.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		if console {
			log.Printf("WARNING: cannot write log file %s (%v); logging to console only", path, err)
		}
		return
	}
	f.Close()

	file := &lumberjack.Logger{
		Filename:   path,
		MaxSize:    10, // MB per file
		MaxBackups: 5,
		MaxAge:     30, // days
		Compress:   true,
	}
	if console {
		log.SetOutput(io.MultiWriter(os.Stderr, file))
	} else {
		log.SetOutput(file)
	}
}
