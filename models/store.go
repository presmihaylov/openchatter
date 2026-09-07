package models

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5"
	"github.com/golang-migrate/migrate/v4/source/iofs"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/presmihaylov/openchatter/migrations"
)

var (
	ErrNotFound  = errors.New("not found")
	ErrConflict  = errors.New("already exists")
	ErrForbidden = errors.New("forbidden")
	ErrArchived  = errors.New("channel is archived")
)

type Store struct {
	pool *pgxpool.Pool
}

// Open connects to Postgres and runs pending migrations.
func Open(ctx context.Context, dbURL string) (*Store, error) {
	if err := runMigrations(dbURL); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func runMigrations(dbURL string) error {
	m, err := newMigrator(dbURL)
	if err != nil {
		return err
	}
	defer m.Close()
	if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return err
	}
	return nil
}

// MigrateTo moves the schema to exactly version, down or up, and returns the
// version the database ends at. It is the rollback step before deploying an
// older binary: an old binary refuses to open a database whose version is
// above the migrations it embeds. ctx is accepted for symmetry with Open;
// golang-migrate has no context API.
func MigrateTo(ctx context.Context, dbURL string, version uint) (uint, error) {
	m, err := newMigrator(dbURL)
	if err != nil {
		return 0, err
	}
	defer m.Close()
	if err := m.Migrate(version); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return 0, err
	}
	got, dirty, err := m.Version()
	if err != nil {
		return 0, err
	}
	if dirty {
		return got, fmt.Errorf("migration %d left the database dirty", got)
	}
	return got, nil
}

func newMigrator(dbURL string) (*migrate.Migrate, error) {
	src, err := iofs.New(migrations.FS, ".")
	if err != nil {
		return nil, err
	}
	// golang-migrate's pgx/v5 driver registers the pgx5:// scheme.
	url := strings.Replace(dbURL, "postgresql://", "postgres://", 1)
	url = strings.Replace(url, "postgres://", "pgx5://", 1)
	return migrate.NewWithSourceInstance("iofs", src, url)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503"
}

func mapRowErr(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	return err
}
