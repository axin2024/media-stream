// Package catalog stores channel, media, and worker state in MySQL.
package catalog

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
	"github.com/golang-migrate/migrate/v4"
	migratemysql "github.com/golang-migrate/migrate/v4/database/mysql"
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed migrations/*.up.sql
var migrations embed.FS

var (
	ErrMissing      = errors.New("object not found")
	ErrDenied       = errors.New("credential rejected")
	ErrInvalid      = errors.New("invalid request")
	ErrConflict     = errors.New("request conflicts with current state")
	ErrFenced       = errors.New("lease or source generation is no longer current")
	ErrBackpressure = errors.New("source watermark is too far behind")
)

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
var requestKey = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,95}$`)

type Store struct {
	db      *sql.DB
	signing []byte
}

func Open(ctx context.Context, dsn string, signing []byte) (*Store, error) {
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse MySQL settings: %w", err)
	}
	config.ParseTime = true
	config.Loc = time.UTC
	config.MultiStatements = false
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 20 * time.Second
	config.WriteTimeout = 20 * time.Second
	if config.Params == nil {
		config.Params = make(map[string]string)
	}
	config.Params["time_zone"] = "'+00:00'"
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(32)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(4 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db, signing: append([]byte(nil), signing...)}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Ready(ctx context.Context) error {
	var rows int
	return s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM productions WHERE id=''").Scan(&rows)
}

func Migrate(dsn string, steps int) error {
	config, err := driver.ParseDSN(dsn)
	if err != nil {
		return err
	}
	config.MultiStatements = true
	db, err := sql.Open("mysql", config.FormatDSN())
	if err != nil {
		return err
	}
	defer db.Close()
	source, err := iofs.New(migrations, "migrations")
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := migratemysql.WithInstance(db, &migratemysql.Config{})
	if err != nil {
		return err
	}
	m, err := migrate.NewWithInstance("iofs", source, "mysql", target)
	if err != nil {
		return err
	}
	if steps > 0 {
		err = m.Steps(steps)
	} else {
		err = m.Up()
	}
	if errors.Is(err, migrate.ErrNoChange) {
		return nil
	}
	return err
}

func transaction[T any](ctx context.Context, s *Store, action func(*sql.Tx) (T, error)) (T, error) {
	var zero T
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return zero, err
	}
	defer tx.Rollback()
	value, err := action(tx)
	if err != nil {
		return zero, err
	}
	if err := tx.Commit(); err != nil {
		return zero, err
	}
	return value, nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func missing(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrMissing
	}
	return err
}

func duplicate(err error) bool {
	var failure *driver.MySQLError
	return errors.As(err, &failure) && failure.Number == 1062
}

func key() string {
	value := make([]byte, 12)
	rand.Read(value)
	return hex.EncodeToString(value)
}

func validSource(source string) bool { return source == "primary" || source == "backup" }

type Identity struct {
	Tenant           string
	Label            string
	Role             string
	Channel          string
	Session          string
	Source           string
	SourceGeneration uint64
	credential       [32]byte
}

func (i Identity) Allows(tenant, channel, session string, roles ...string) bool {
	if i.Tenant != tenant || (i.Channel != "" && i.Channel != channel) || (i.Session != "" && i.Session != session) {
		return false
	}
	for _, role := range roles {
		if i.Role == role {
			return true
		}
	}
	return false
}

func (s *Store) Identify(ctx context.Context, secret string) (Identity, error) {
	if len(secret) != 64 {
		return Identity{}, ErrDenied
	}
	sum := digestSecret(secret)
	result := Identity{credential: sum}
	err := s.db.QueryRowContext(ctx, `SELECT tenant,label,role,channel,session_id,source,source_generation
		FROM credentials WHERE digest=? AND expires_at>UTC_TIMESTAMP(6) AND revoked_at IS NULL`, sum[:]).Scan(
		&result.Tenant, &result.Label, &result.Role, &result.Channel, &result.Session, &result.Source, &result.SourceGeneration)
	if errors.Is(err, sql.ErrNoRows) {
		return Identity{}, ErrDenied
	}
	return result, err
}

type Grant struct {
	Secret     string    `json:"secret"`
	Generation uint64    `json:"generation,omitempty"`
	Expires    time.Time `json:"expires"`
}

func (s *Store) Provision(ctx context.Context, tenant, label, role, channel string, expires time.Time) (Grant, error) {
	if !identifier.MatchString(tenant) || !identifier.MatchString(label) ||
		(channel != "" && !identifier.MatchString(channel)) || !expires.After(time.Now()) {
		return Grant{}, ErrInvalid
	}
	switch role {
	case "owner", "director", "editor", "viewer", "operator":
	default:
		return Grant{}, ErrInvalid
	}
	return transaction(ctx, s, func(tx *sql.Tx) (Grant, error) {
		if _, err := tx.ExecContext(ctx, "INSERT INTO tenants(id) VALUES(?) ON DUPLICATE KEY UPDATE id=id", tenant); err != nil {
			return Grant{}, err
		}
		grant := Grant{Secret: hex.EncodeToString(randBytes(32)), Expires: expires.UTC()}
		err := insertGrant(ctx, tx, Identity{Tenant: tenant, Label: label, Role: role, Channel: channel}, grant)
		return grant, err
	})
}

func randBytes(size int) []byte {
	value := make([]byte, size)
	rand.Read(value)
	return value
}

func insertGrant(ctx context.Context, tx *sql.Tx, identity Identity, grant Grant) error {
	sum := sha256.Sum256([]byte(grant.Secret))
	_, err := tx.ExecContext(ctx, `INSERT INTO credentials
		(digest,tenant,label,role,channel,session_id,source,source_generation,expires_at)
		VALUES(?,?,?,?,?,?,?,?,?)`, sum[:], identity.Tenant, identity.Label, identity.Role, identity.Channel,
		identity.Session, identity.Source, identity.SourceGeneration, grant.Expires)
	return err
}

func (s *Store) pushSecret(session, source string, generation uint64) string {
	mac := hmac.New(sha256.New, s.signing)
	fmt.Fprintf(mac, "push/%s/%s/%d", session, source, generation)
	return hex.EncodeToString(mac.Sum(nil))
}

func digestSecret(secret string) [32]byte { return sha256.Sum256([]byte(secret)) }

func digest(value any) [32]byte {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err) // Callers hash concrete request structs with JSON-compatible fields.
	}
	return sha256.Sum256(data)
}

func audit(ctx context.Context, tx *sql.Tx, session, kind, actor string, details any) error {
	body, err := json.Marshal(details)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO session_events(session_id,kind,actor,details) VALUES(?,?,?,?)",
		session, kind, actor, body)
	return err
}

func strictText(value string, max int) bool {
	return len(value) <= max && strings.TrimSpace(value) != "" && !strings.ContainsAny(value, "\r\n\x00")
}
