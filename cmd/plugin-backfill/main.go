package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	backfill "github.com/Mininglamp-OSS/octo-marketplace/internal/backfill/plugin"
	marketdb "github.com/Mininglamp-OSS/octo-marketplace/internal/db"
)

func main() {
	var mode, dsn, phase string
	var runMigrations bool
	flag.StringVar(&mode, "mode", "dry-run", "dry-run, apply, or verify")
	flag.StringVar(&phase, "phase", "plan", "plan (deterministic historical backfill), enrich (icons, connector categories, tool counts, metrics migration), or repackage (migrate stored packages to the plugin-lib layout)")
	flag.StringVar(&dsn, "dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN (default MYSQL_DSN)")
	flag.BoolVar(&runMigrations, "migrate", false, "run the embedded schema migrations (same set marketplace-api applies at startup) before the data phase; lets the standalone image prepare a database ahead of the service deploy")
	flag.Parse()
	if dsn == "" {
		fatal(fmt.Errorf("MYSQL_DSN or -dsn is required"))
	}
	db, e := marketdb.Open(dsn)
	if e != nil {
		fatal(e)
	}
	defer db.Close()
	if runMigrations {
		applied, err := marketdb.RunMigrations(db)
		if err != nil {
			fatal(fmt.Errorf("schema migrations: %w", err))
		}
		fmt.Fprintf(os.Stderr, "schema migrations applied: %d\n", applied)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	switch phase {
	case "plan":
		report, e := backfill.New(db).Run(ctx, backfill.Options{Mode: backfill.Mode(mode)})
		if e != nil {
			fatal(e)
		}
		if e = enc.Encode(report); e != nil {
			fatal(e)
		}
		if report.Observed.Missing > 0 || report.Observed.Conflicts > 0 {
			os.Exit(2)
		}
	case "enrich":
		report, e := backfill.New(db).Enrich(ctx, backfill.Options{Mode: backfill.Mode(mode)})
		if e != nil {
			fatal(e)
		}
		if e = enc.Encode(report); e != nil {
			fatal(e)
		}
		if mode == "verify" && report.Remaining != (backfill.EnrichCounts{}) {
			os.Exit(2)
		}
	case "repackage":
		report, e := backfill.New(db).Repackage(ctx, backfill.Options{Mode: backfill.Mode(mode)})
		if e != nil {
			fatal(e)
		}
		if e = enc.Encode(report); e != nil {
			fatal(e)
		}
		if mode == "verify" && report.Remaining != (backfill.RepackageCounts{}) {
			os.Exit(2)
		}
	default:
		fatal(fmt.Errorf("invalid phase %q", phase))
	}
}
func fatal(e error) { fmt.Fprintln(os.Stderr, "plugin-backfill:", e); os.Exit(1) }
