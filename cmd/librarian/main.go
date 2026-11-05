package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/sargunv/bookorbit-librarian-service/internal/config"
	"github.com/sargunv/bookorbit-librarian-service/internal/service"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		slog.Error("librarian stopped", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	command := "serve"
	if len(os.Args) > 1 {
		command = os.Args[1]
	}
	if command == "version" {
		fmt.Println(version)
		return nil
	}
	if command == "health" {
		client := http.Client{Timeout: 3 * time.Second}
		address := os.Getenv("HEALTH_URL")
		if address == "" {
			address = "http://127.0.0.1:8080/healthz"
		}
		r, e := client.Get(address)
		if e != nil {
			return errors.New("health request failed")
		}
		defer func() { _ = r.Body.Close() }()
		if r.StatusCode != 200 {
			return errors.New("unhealthy")
		}
		return nil
	}
	if command == "request" {
		return request()
	}
	if command != "serve" && command != "reconcile" {
		return errors.New("usage: librarian [serve|version|health|reconcile --plan PATH [--apply] [--recovery PATH]|request METHOD /v1/path [JSON]]")
	}
	c, e := config.Load()
	if e != nil {
		return e
	}
	svc, e := service.New(c)
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if command == "reconcile" {
		flags := flag.NewFlagSet("reconcile", flag.ContinueOnError)
		plan := flags.String("plan", "", "private durable plan path")
		apply := flags.Bool("apply", false, "apply and verify the saved plan")
		recovery := flags.String("recovery", "", "private reference snapshot exported from a pre-merge backup")
		if err := flags.Parse(os.Args[2:]); err != nil {
			return err
		}
		if *plan == "" {
			return errors.New("reconcile requires --plan")
		}
		report, err := svc.ReconcileFile(ctx, *plan, *apply, *recovery)
		closeErr := svc.Close()
		if err != nil {
			return errors.Join(err, closeErr)
		}
		if closeErr != nil {
			return closeErr
		}
		return json.NewEncoder(os.Stdout).Encode(report)
	}
	svc.Start(ctx)
	srv := &http.Server{Addr: c.Listen, Handler: svc.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 3 * time.Minute, IdleTimeout: 60 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	slog.Info("librarian started", "version", version, "workers", c.Workers, "autoApply", c.AutoApply, "organize", c.Organize)
	e = srv.ListenAndServe()
	stop()
	closeErr := svc.Close()
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		return e
	}
	return closeErr
}

func request() error {
	if len(os.Args) < 4 {
		return errors.New("usage: librarian request METHOD /v1/path [JSON]")
	}
	address := os.Getenv("SERVICE_URL")
	if address == "" {
		address = "http://127.0.0.1:8080"
	}
	var body io.Reader
	if len(os.Args) > 4 {
		body = strings.NewReader(os.Args[4])
	}
	req, e := http.NewRequest(os.Args[2], address+os.Args[3], body)
	if e != nil {
		return errors.New("invalid request")
	}
	req.Header.Set("Authorization", "Bearer "+os.Getenv("ADMIN_TOKEN"))
	req.Header.Set("Content-Type", "application/json")
	client := http.Client{Timeout: 3 * time.Minute}
	r, e := client.Do(req)
	if e != nil {
		return errors.New("request failed")
	}
	defer func() { _ = r.Body.Close() }()
	if _, e = io.Copy(os.Stdout, io.LimitReader(r.Body, 16<<20)); e != nil {
		return e
	}
	if r.StatusCode >= 400 {
		return fmt.Errorf("HTTP %d", r.StatusCode)
	}
	return nil
}
