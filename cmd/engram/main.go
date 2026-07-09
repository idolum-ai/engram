package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/idolum-ai/engram/internal/app"
	"github.com/idolum-ai/engram/internal/config"
	"github.com/idolum-ai/engram/internal/version"
)

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		printHelp()
		return 2
	}
	switch args[0] {
	case "run":
		fs := flag.NewFlagSet("run", flag.ContinueOnError)
		envPath := fs.String("env", config.DefaultEnvPath(), "path to .env")
		if err := fs.Parse(args[1:]); err != nil {
			return 2
		}
		cfg, err := config.Load(*envPath)
		if err != nil {
			fmt.Fprintln(os.Stderr, "config:", err)
			return 1
		}
		a, err := app.New(cfg)
		if err != nil {
			fmt.Fprintln(os.Stderr, "start:", err)
			return 1
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return a.Run(ctx)
	case "version", "--version", "-v":
		fmt.Println(version.String())
		return 0
	case "help", "--help", "-h":
		printHelp()
		return 0
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		printHelp()
		return 2
	}
}

func printHelp() {
	fmt.Print(`Usage:
  engram run [--env ~/.engram/.env]
  engram version
  engram help
`)
}
