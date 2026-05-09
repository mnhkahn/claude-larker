package main

import (
	"flag"
	"fmt"
	"os"

	"log"

	"github.com/mnhkahn/gogogo/logger"
	"github.com/mnhkahn/larker/internal/config"
	"github.com/mnhkahn/larker/internal/hook"
	"github.com/mnhkahn/larker/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: larker <server|hook> [flags]\n")
		os.Exit(1)
	}

	subcommand := os.Args[1]
	os.Args = append([]string{os.Args[0]}, os.Args[2:]...)

	switch subcommand {
	case "server":
		runServer()
	case "hook":
		runHook()
	default:
		fmt.Fprintf(os.Stderr, "Unknown subcommand: %s\n", subcommand)
		os.Exit(1)
	}
}

func runServer() {
	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to load config: %v\n", err)
		os.Exit(1)
	}

	l := newLogger(cfg.Log)

	srv := server.New(cfg, l)
	l.Info("Starting Larker server on %s:%d", cfg.Server.Host, cfg.Server.Port)
	if err := srv.Run(); err != nil {
		l.Error("Server failed: %v", err)
		os.Exit(1)
	}
}

func newLogger(cfg config.LogConfig) *logger.Logger {
	ll := logger.NewFileLogger(cfg.File, cfg.MaxSizeMB, log.LstdFlags|log.Lshortfile, 3)
	ll.SetLevel(parseLogLevel(cfg.Level))
	return ll
}

func parseLogLevel(s string) int {
	switch s {
	case "debug":
		return logger.LevelDebug
	case "warn":
		return logger.LevelWarning
	case "error":
		return logger.LevelError
	default:
		return logger.LevelInformational
	}
}

func runHook() {
	phase := flag.String("phase", "pre", "kimi-cli hook phase: pre, post, stop, stopfailure, notification, sessionstart, promptsubmit")
	service := flag.String("service", "", "override service detection: cc, kimi, trae")
	flag.Parse()

	if err := hook.Run(*phase, *service); err != nil {
		fmt.Fprintf(os.Stderr, "Hook failed: %v\n", err)
		os.Exit(1)
	}
}
