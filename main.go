package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "PlatformDBTunnel"
	displayName = "Platform DB Tunnel HTTP Service"
	description = "HTTP application that starts automatically at boot."
)

func main() {
	isService, err := svc.IsWindowsService()
	if err != nil {
		log.Fatalf("detect service mode: %v", err)
	}
	if isService {
		runService()
		return
	}

	cmd := "run"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}
	switch cmd {
	case "run":
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := serve(ctx); err != nil {
			log.Fatal(err)
		}
	case "install":
		err = installService()
	case "uninstall":
		err = uninstallService()
	case "start":
		err = controlService(func(s *mgr.Service) error { return s.Start() })
	case "stop":
		err = controlService(func(s *mgr.Service) error {
			_, err := s.Control(svc.Stop)
			return err
		})
	default:
		fmt.Fprintf(os.Stderr, "usage: %s [run|install|uninstall|start|stop]\n", filepath.Base(os.Args[0]))
		os.Exit(2)
	}
	if err != nil {
		log.Fatal(err)
	}
}

// serve runs the HTTP server until ctx is cancelled, then shuts down gracefully.
func serve(ctx context.Context) error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	db, err := sql.Open(cfg.Driver, cfg.DSN)
	if err != nil {
		return err
	}
	defer db.Close()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           newHandler(db, cfg.QueryTimeout),
		ReadHeaderTimeout: 10 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

type service struct{ elog *eventlog.Log }

func runService() {
	elog, err := eventlog.Open(serviceName)
	if err != nil {
		return
	}
	defer elog.Close()
	if err := svc.Run(serviceName, &service{elog: elog}); err != nil {
		elog.Error(1, fmt.Sprintf("service failed: %v", err))
	}
}

func (s *service) Execute(_ []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- serve(ctx) }()

	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	s.elog.Info(1, "service started")

	for {
		select {
		case err := <-done:
			// Server exited on its own (e.g. port in use): report failure so recovery actions kick in.
			s.elog.Error(1, fmt.Sprintf("server exited: %v", err))
			return false, 1
		case c := <-req:
			switch c.Cmd {
			case svc.Interrogate:
				status <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				status <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		}
	}
}

func installService() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()

	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: displayName,
		Description: description,
		StartType:   mgr.StartAutomatic,
	}, "run")
	if err != nil {
		return err
	}
	defer s.Close()

	// Restart on crash: after 5s, 5s, then 30s; reset failure count after a day.
	err = s.SetRecoveryActions([]mgr.RecoveryAction{
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 5 * time.Second},
		{Type: mgr.ServiceRestart, Delay: 30 * time.Second},
	}, 86400)
	if err != nil {
		return err
	}
	if err := eventlog.InstallAsEventCreate(serviceName, eventlog.Info|eventlog.Warning|eventlog.Error); err != nil {
		return err
	}
	if err := s.Start(); err != nil {
		return err
	}
	fmt.Println("installed and started", serviceName)
	return nil
}

func uninstallService() error {
	return controlService(func(s *mgr.Service) error {
		s.Control(svc.Stop) // best effort; may already be stopped
		if err := s.Delete(); err != nil {
			return err
		}
		eventlog.Remove(serviceName)
		fmt.Println("removed", serviceName)
		return nil
	})
}

func controlService(fn func(*mgr.Service) error) error {
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("connect to service manager (run as Administrator): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return fmt.Errorf("open service: %w", err)
	}
	defer s.Close()
	return fn(s)
}
