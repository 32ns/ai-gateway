package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/32ns/ai-gateway/internal/app"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "backup":
			if err := app.RunBackupCommand(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "restore":
			if err := app.RunRestoreCommand(os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		case "install", "uninstall", "upgrade", "rollback", "reinstall", "reboot", "restart", "stop":
			if err := app.RunServiceCommand(os.Args[1], os.Args[2:]); err != nil {
				log.Fatal(err)
			}
			return
		}
		if !strings.HasPrefix(os.Args[1], "-") {
			log.Fatal(fmt.Errorf("unknown command: %s", os.Args[1]))
		}
	}

	flags, err := app.ParseServerFlags(os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	service, err := app.NewBuilder().WithOptions(app.Options(flags)).Build()
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = service.Close() }()
	service.LogStartup(log.Printf)

	errCh := make(chan error, 1)
	go func() {
		errCh <- service.ListenAndServe()
	}()
	signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	select {
	case err := <-errCh:
		if err != nil {
			log.Fatal(err)
		}
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := service.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown error: %v", err)
		}
		if err := <-errCh; err != nil {
			log.Fatal(err)
		}
	}
}
