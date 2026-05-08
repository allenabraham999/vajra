package store

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"

	"github.com/golang-migrate/migrate/v4"
	pgmigrate "github.com/golang-migrate/migrate/v4/database/postgres"
	"github.com/golang-migrate/migrate/v4/source/iofs"

	// file:// source registration is a side-effect import; needed when the
	// caller provides a filesystem-path source URL.
	_ "github.com/golang-migrate/migrate/v4/source/file"
)

// Migrator runs schema migrations against a Postgres database. It is a
// thin wrapper around golang-migrate so the rest of the codebase never
// imports migrate/v4 directly.
type Migrator struct {
	m *migrate.Migrate
}

// NewMigrator builds a Migrator from a *sql.DB and a golang-migrate
// source URL such as "file:///abs/path/to/migrations". For embed.FS
// sources, prefer NewMigratorFS.
func NewMigrator(db *sql.DB, sourceURL string) (*Migrator, error) {
	driver, err := pgmigrate.WithInstance(db, &pgmigrate.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrations: open driver: %w", err)
	}
	m, err := migrate.NewWithDatabaseInstance(sourceURL, "postgres", driver)
	if err != nil {
		return nil, fmt.Errorf("migrations: open source %q: %w", sourceURL, err)
	}
	return &Migrator{m: m}, nil
}

// NewMigratorFS builds a Migrator that reads SQL files from an fs.FS
// (e.g. an embed.FS). dir is the path within fsys that contains the
// numbered migration files.
func NewMigratorFS(db *sql.DB, fsys fs.FS, dir string) (*Migrator, error) {
	src, err := iofs.New(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("migrations: open iofs source: %w", err)
	}
	driver, err := pgmigrate.WithInstance(db, &pgmigrate.Config{})
	if err != nil {
		return nil, fmt.Errorf("migrations: open driver: %w", err)
	}
	m, err := migrate.NewWithInstance("iofs", src, "postgres", driver)
	if err != nil {
		return nil, fmt.Errorf("migrations: new instance: %w", err)
	}
	return &Migrator{m: m}, nil
}

// Up applies every pending up migration. ErrNoChange is swallowed so
// repeated Up calls during a startup retry loop don't fail.
func (mig *Migrator) Up() error {
	if err := mig.m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrations: up: %w", err)
	}
	return nil
}

// Down rolls back every applied migration. Used in tests; production
// code should prefer Steps with an explicit count.
func (mig *Migrator) Down() error {
	if err := mig.m.Down(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrations: down: %w", err)
	}
	return nil
}

// Steps runs n migrations forward (positive n) or backward (negative n).
func (mig *Migrator) Steps(n int) error {
	if err := mig.m.Steps(n); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("migrations: steps(%d): %w", n, err)
	}
	return nil
}

// Version returns the most recently applied migration version. dirty is
// true when a migration crashed mid-way and needs operator attention.
// Returns (0, false, nil) when no migrations have been applied yet.
func (mig *Migrator) Version() (version uint, dirty bool, err error) {
	v, d, err := mig.m.Version()
	if err != nil {
		if errors.Is(err, migrate.ErrNilVersion) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("migrations: version: %w", err)
	}
	return v, d, nil
}

// Close releases both the source and database driver. Joining the two
// errors avoids the awkward (sourceErr, dbErr) signature golang-migrate
// exposes by default.
func (mig *Migrator) Close() error {
	srcErr, dbErr := mig.m.Close()
	return errors.Join(srcErr, dbErr)
}
