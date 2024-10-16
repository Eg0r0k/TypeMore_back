package db

import (
	"database/sql"
	"embed"
	"fmt"
	"log"

	"typeMore/config"

	_ "github.com/lib/pq"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*
var migrations embed.FS

func Connect(cfg *config.Config) (*sql.DB, error) {
    psqlInfo := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=disable",
        cfg.DBHost, cfg.DBPort, cfg.DBUser, cfg.DBPassword, cfg.DBName)
    
    db, err := sql.Open("postgres", psqlInfo)
    if err != nil {
        return nil, fmt.Errorf("failed to connect to the database: %w", err)
    }

    if err := migrateDB(db, cfg.ReloadDB); err != nil {
        return nil, fmt.Errorf("failed to migrate database: %w", err)
    }

    return db, nil
}

func migrateDB(db *sql.DB, reload bool) error {
    goose.SetBaseFS(migrations)

    if reload {
        log.Println("Dropping all tables and reapplying migrations")
        if err := goose.DownTo(db, "migrations", 0); err != nil {
            return fmt.Errorf("failed to rollback migrations: %w", err)
        }
    }

    log.Println("Applying migrations")
    if err := goose.Up(db, "migrations"); err != nil {
        return fmt.Errorf("failed to apply migrations: %w", err)
    }
    return nil
}