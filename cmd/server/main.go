package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"pieceobot/internal"
)

func main() {
	application := internal.NewApp()

	go func() {
		application.Start()
	}()
	slog.Info("application started successfully")

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	<-signalChan
	slog.Info("received shutdown signal")

	application.Stop()
	slog.Info("application stopped successfully")
}
