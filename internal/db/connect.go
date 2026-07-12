package db

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/pressly/goose/v3"
)

var pragmas = map[string]string{
	"foreign_keys":  "ON",
	"journal_mode":  "WAL",
	"page_size":     "4096",
	"cache_size":    "-8000",
	"synchronous":   "NORMAL",
	"secure_delete": "ON",
	"busy_timeout":  "30000",
}

var (
	testTemplatePath string
	testTemplateOnce sync.Once
	testTemplateErr  error
)

func isTest() bool {
	if flag.Lookup("test.v") != nil {
		return true
	}
	if len(os.Args) > 0 {
		base := filepath.Base(os.Args[0])
		if strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
			return true
		}
	}
	return false
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	if err != nil {
		return err
	}
	return out.Sync()
}

func setupTestTemplate(ctx context.Context) error {
	tmpDir, err := os.MkdirTemp("", "crush-test-db-template-*")
	if err != nil {
		return err
	}
	dbPath := filepath.Join(tmpDir, "template.db")

	db, err := openDB(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	if err = db.PingContext(ctx); err != nil {
		return err
	}

	goose.SetBaseFS(FS)
	if err := goose.SetDialect("sqlite3"); err != nil {
		return err
	}

	if err := goose.Up(db, "migrations"); err != nil {
		return err
	}

	testTemplatePath = dbPath
	return nil
}

// Connect opens a SQLite database connection and runs migrations.
func Connect(ctx context.Context, dataDir string) (*sql.DB, error) {
	if dataDir == "" {
		return nil, fmt.Errorf("data.dir is not set")
	}
	dbPath := filepath.Join(dataDir, "crush.db")

	if isTest() {
		testTemplateOnce.Do(func() {
			testTemplateErr = setupTestTemplate(context.Background())
		})
		if testTemplateErr != nil {
			return nil, fmt.Errorf("failed to setup test database template: %w", testTemplateErr)
		}

		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			if err := os.MkdirAll(dataDir, 0o755); err != nil {
				return nil, fmt.Errorf("failed to create db directory: %w", err)
			}
			if err := copyFile(testTemplatePath, dbPath); err != nil {
				return nil, fmt.Errorf("failed to copy test database template: %w", err)
			}
		}

		db, err := openDB(dbPath)
		if err != nil {
			return nil, err
		}

		if err = db.PingContext(ctx); err != nil {
			db.Close()
			return nil, fmt.Errorf("failed to connect to database: %w", err)
		}

		// The template copied above was already migrated inside
		// testTemplateOnce, so there's no need to run goose again here.
		// goose.SetBaseFS/SetDialect/Up mutate package-level state in the
		// goose library that isn't safe to touch concurrently, which was
		// causing data races when tests open connections in parallel.
		return db, nil
	}

	db, err := openDB(dbPath)
	if err != nil {
		return nil, err
	}

	if err = db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	goose.SetBaseFS(FS)

	if err := goose.SetDialect("sqlite3"); err != nil {
		slog.Error("Failed to set dialect", "error", err)
		return nil, fmt.Errorf("failed to set dialect: %w", err)
	}

	if err := goose.Up(db, "migrations"); err != nil {
		slog.Error("Failed to apply migrations", "error", err)
		return nil, fmt.Errorf("failed to apply migrations: %w", err)
	}

	return db, nil
}
