package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"sort"

	"food-telegram/db"
)

// Embed migrations into the binary so `food-telegram.exe migrate` works
// regardless of the current working directory.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

func applyMigrations(ctx context.Context, verbose bool) error {
	names, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)
	for _, name := range names {
		sqlBytes, err := migrationsFS.ReadFile(name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if _, err := db.Pool.Exec(ctx, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if verbose {
			fmt.Println("Migration", name, "applied.")
		}
	}
	return nil
}
