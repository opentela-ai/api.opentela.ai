// Command keyctl seeds and manages API keys in Postgres for the opentela proxy.
//
// Usage:
//
//	keyctl migrate
//	keyctl add [--name NAME]
//	keyctl revoke <token>
//	keyctl list
//
// DATABASE_URL must be set.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"flag"
	"fmt"
	"os"

	"github.com/opentela-ai/api/internal/store"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: keyctl <migrate|add|revoke|list> [flags]")
	}
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		return fmt.Errorf("DATABASE_URL is required")
	}

	ctx := context.Background()
	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		return err
	}
	defer pg.Close()

	switch args[0] {
	case "migrate":
		return cmdMigrate(ctx, pg)
	case "add":
		return cmdAdd(ctx, pg, args[1:])
	case "revoke":
		return cmdRevoke(ctx, pg, args[1:])
	case "list":
		return cmdList(ctx, pg)
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func cmdMigrate(ctx context.Context, pg *store.Postgres) error {
	ddl, err := os.ReadFile("migrations/0001_init.sql")
	if err != nil {
		return fmt.Errorf("read migration (run from repo root): %w", err)
	}
	if err := pg.Migrate(ctx, string(ddl)); err != nil {
		return err
	}
	fmt.Println("migration applied")
	return nil
}

func cmdAdd(ctx context.Context, pg *store.Postgres, args []string) error {
	fs := flag.NewFlagSet("add", flag.ContinueOnError)
	name := fs.String("name", "", "human-readable label for the key")
	if err := fs.Parse(args); err != nil {
		return err
	}
	token, err := generateToken()
	if err != nil {
		return err
	}
	if err := pg.Insert(ctx, store.HashKey(token), *name); err != nil {
		return err
	}
	fmt.Printf("created key (name=%q)\n", *name)
	fmt.Printf("token (shown once): %s\n", token)
	return nil
}

func cmdRevoke(ctx context.Context, pg *store.Postgres, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: keyctl revoke <token>")
	}
	changed, err := pg.Revoke(ctx, store.HashKey(args[0]))
	if err != nil {
		return err
	}
	if !changed {
		fmt.Println("no active key matched that token")
		return nil
	}
	fmt.Println("key revoked")
	return nil
}

func cmdList(ctx context.Context, pg *store.Postgres) error {
	keys, err := pg.List(ctx)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		fmt.Println("no keys")
		return nil
	}
	for _, k := range keys {
		status := "active"
		if !k.Active {
			status = "revoked"
		}
		prefix := k.KeyHash
		if len(prefix) > 12 {
			prefix = prefix[:12]
		}
		fmt.Printf("%s… name=%q %s created=%s\n",
			prefix, k.Name, status, k.CreatedAt.Format("2006-01-02"))
	}
	return nil
}

// generateToken returns a random opaque token of the form "sk-<48 hex chars>".
func generateToken() (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "sk-" + hex.EncodeToString(b), nil
}
