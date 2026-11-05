package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"invoice.local/system/backend/internal/account"
	"invoice.local/system/backend/internal/config"
	"os"
	"time"
)

func main() {
	if e := run(); e != nil {
		fmt.Fprintln(os.Stderr, "administrator command failed; no credentials are printed")
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) < 2 {
		return account.ErrInvalid
	}
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	cfgPath := fs.String("config", "", "configuration")
	username := fs.String("username", "", "administrator name")
	output := fs.String("output", "", "new private output file")
	if e := fs.Parse(os.Args[2:]); e != nil {
		return e
	}
	if fs.NArg() != 0 {
		return account.ErrInvalid
	}
	if *output == "" {
		return account.ErrInvalid
	}
	if os.Args[1] == "keyring-init" {
		return account.GenerateKeyringFile(*output)
	}
	if os.Args[1] != "init" && os.Args[1] != "reissue" {
		return account.ErrInvalid
	}
	cfg, e := config.Load(*cfgPath)
	if e != nil {
		return e
	}
	keys, e := account.LoadKeyring(cfg.KeyringFile)
	if e != nil {
		return e
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool, e := account.OpenPool(ctx, cfg.DatabaseDSNFile, cfg.Environment == "production")
	if e != nil {
		return e
	}
	defer pool.Close()
	service, e := account.NewService(pool, keys, cfg.PublicOrigin)
	if e != nil {
		return e
	}
	// Reserve output before mutating the database. Failed/uncertain operations
	// require an explicit reissue; no old credential is recovered from history.
	f, e := os.OpenFile(*output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer f.Close()
	result, e := service.InitializeAdministrator(ctx, *username, os.Args[1] == "reissue")
	if e != nil {
		return e
	}
	if e = json.NewEncoder(f).Encode(result); e != nil {
		return e
	}
	return f.Sync()
}
