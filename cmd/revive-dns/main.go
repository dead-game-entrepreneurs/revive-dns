// Command revive-dns is a small DNS server for game revival services: it answers
// the games' hostnames with the IPs of the replacement servers, forwards chosen
// domains (such as DNAS) to another resolver, and passes everything else upstream.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dead-game-entrepreneurs/revive-dns/internal/config"
	"github.com/dead-game-entrepreneurs/revive-dns/internal/dnsserver"
)

func main() {
	path := flag.String("config", "config.yaml", "rules file")
	check := flag.Bool("check", false, "validate the config and exit")
	debug := flag.Bool("debug", os.Getenv("REVIVE_DNS_DEBUG") != "", "log every query")
	flag.Parse()

	cfg, err := config.Load(*path)
	if err != nil {
		fmt.Fprintln(os.Stderr, "revive-dns:", err)
		os.Exit(2)
	}
	if *check {
		for _, r := range cfg.Rules {
			target := r.Answer
			if r.Forward != "" {
				target = "forward " + r.Forward
			}
			fmt.Printf("%-12s %-22s %v\n", r.Service, target, r.Names)
		}
		return
	}
	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	log.Info("starting", "rules", len(cfg.Rules), "upstream", cfg.Upstream, "allow", cfg.Allow)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := dnsserver.New(cfg, log).Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}
