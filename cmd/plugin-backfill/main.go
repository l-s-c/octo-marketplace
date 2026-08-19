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
	var mode, dsn string
	flag.StringVar(&mode, "mode", "dry-run", "dry-run, apply, or verify")
	flag.StringVar(&dsn, "dsn", os.Getenv("MYSQL_DSN"), "MySQL DSN (default MYSQL_DSN)")
	flag.Parse()
	if dsn == "" {
		fatal(fmt.Errorf("MYSQL_DSN or -dsn is required"))
	}
	db, e := marketdb.Open(dsn)
	if e != nil {
		fatal(e)
	}
	defer db.Close()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	report, e := backfill.New(db).Run(ctx, backfill.Options{Mode: backfill.Mode(mode)})
	if e != nil {
		fatal(e)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if e = enc.Encode(report); e != nil {
		fatal(e)
	}
	if report.Observed.Missing > 0 || report.Observed.Conflicts > 0 {
		os.Exit(2)
	}
}
func fatal(e error) { fmt.Fprintln(os.Stderr, "plugin-backfill:", e); os.Exit(1) }
