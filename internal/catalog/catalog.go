package catalog

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/cca2878/crawlbox/internal/model"
	_ "github.com/ncruces/go-sqlite3/driver"
	"strings"
	"time"
)

type Store struct{ DB *sql.DB }
type Token struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	Sources []string   `json:"sources"`
	Created time.Time  `json:"created"`
	Expires *time.Time `json:"expires,omitempty"`
	Revoked bool       `json:"revoked"`
}

func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b)
}
func Open(path string) (*Store, error) {
	db, e := sql.Open("sqlite3", path)
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	s := &Store{db}
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON;
 CREATE TABLE IF NOT EXISTS sources(id TEXT PRIMARY KEY, identity TEXT NOT NULL, retired INTEGER NOT NULL DEFAULT 0);
 CREATE TABLE IF NOT EXISTS revisions(id TEXT PRIMARY KEY, source TEXT NOT NULL, parent TEXT NOT NULL, created TEXT NOT NULL, snapshot TEXT NOT NULL, body TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS revisions_source ON revisions(source,created);
 CREATE TABLE IF NOT EXISTS heads(source TEXT PRIMARY KEY, revision TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS runs(id TEXT PRIMARY KEY, body TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS settings(key TEXT PRIMARY KEY, value INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS tokens(id TEXT PRIMARY KEY, digest BLOB NOT NULL, body TEXT NOT NULL);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }

// Register permanently retires removed IDs; existing IDs cannot switch plugin identity.
func (s *Store) Register(ctx context.Context, identities map[string]string) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	rows, e := tx.QueryContext(ctx, "SELECT id,identity,retired FROM sources")
	if e != nil {
		return e
	}
	type old struct {
		id, identity string
		retired      bool
	}
	var olds []old
	for rows.Next() {
		var o old
		if e = rows.Scan(&o.id, &o.identity, &o.retired); e != nil {
			rows.Close()
			return e
		}
		olds = append(olds, o)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	for _, o := range olds {
		v, ok := identities[o.id]
		if ok && (o.retired || v != o.identity) {
			return fmt.Errorf("source %s is retired or changed plugin identity", o.id)
		}
		if !ok {
			if _, e = tx.ExecContext(ctx, "UPDATE sources SET retired=1 WHERE id=?", o.id); e != nil {
				return e
			}
		}
	}
	for id, v := range identities {
		if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sources(id,identity) VALUES(?,?)", id, v); e != nil {
			return e
		}
	}
	return tx.Commit()
}
func (s *Store) Commit(ctx context.Context, r model.Revision) error {
	tx, e := s.DB.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	var exists int
	if e = tx.QueryRowContext(ctx, "SELECT count(*) FROM revisions WHERE id=?", r.ID).Scan(&exists); e != nil {
		return e
	}
	if exists > 0 {
		return tx.Commit()
	}
	var head string
	e = tx.QueryRowContext(ctx, "SELECT revision FROM heads WHERE source=?", r.Source).Scan(&head)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	if head != r.Parent {
		return fmt.Errorf("revision parent mismatch: %s", r.ID)
	}
	if r.Plugin.ID != "" {
		var identity string
		err := tx.QueryRowContext(ctx, "SELECT identity FROM sources WHERE id=?", r.Source).Scan(&identity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil && identity != r.Plugin.ID {
			return errors.New("historical source identity changed")
		}
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO sources(id,identity) VALUES(?,?)", r.Source, r.Plugin.ID); err != nil {
			return err
		}
	}
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO revisions VALUES(?,?,?,?,?,?)", r.ID, r.Source, r.Parent, r.CreatedAt.Format(time.RFC3339Nano), r.Snapshot, string(b))
	if e != nil {
		return e
	}
	_, e = tx.ExecContext(ctx, "INSERT INTO heads VALUES(?,?) ON CONFLICT(source) DO UPDATE SET revision=excluded.revision", r.Source, r.ID)
	if e != nil {
		return e
	}
	return tx.Commit()
}
func (s *Store) Revision(ctx context.Context, source, id string) (model.Revision, error) {
	var b string
	var r model.Revision
	var e error
	if id == "latest" {
		e = s.DB.QueryRowContext(ctx, "SELECT body FROM revisions WHERE id=(SELECT revision FROM heads WHERE source=?)", source).Scan(&b)
	} else {
		e = s.DB.QueryRowContext(ctx, "SELECT body FROM revisions WHERE source=? AND id=?", source, id).Scan(&b)
	}
	if e != nil {
		return r, e
	}
	e = json.Unmarshal([]byte(b), &r)
	return r, e
}
func (s *Store) Revisions(ctx context.Context, source string) ([]model.Revision, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT body FROM revisions WHERE source=? ORDER BY created DESC,id DESC", source)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []model.Revision{}
	for rows.Next() {
		var b string
		var r model.Revision
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &r); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) SaveRun(ctx context.Context, r model.Run) error {
	b, e := json.Marshal(r)
	if e != nil {
		return e
	}
	_, e = s.DB.ExecContext(ctx, "INSERT INTO runs VALUES(?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body", r.ID, string(b))
	return e
}
func (s *Store) Runs(ctx context.Context) ([]model.Run, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT body FROM runs ORDER BY rowid DESC LIMIT 100")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []model.Run{}
	for rows.Next() {
		var b string
		var r model.Run
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &r); e != nil {
			return nil, e
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
func (s *Store) InterruptRuns(ctx context.Context) error {
	_, e := s.DB.ExecContext(ctx, `UPDATE runs SET body=json_set(body,'$.status','interrupted','$.finished',?) WHERE json_extract(body,'$.status') IN ('queued','running','validating','snapshotting','committing')`, time.Now().UTC().Format(time.RFC3339Nano))
	return e
}
func (s *Store) CreateToken(ctx context.Context, name string, sources []string, expires *time.Time) (Token, string, error) {
	t := Token{ID: ID(), Name: strings.TrimSpace(name), Sources: sources, Created: time.Now().UTC(), Expires: expires}
	if t.Name == "" || len(t.Name) > 100 || len(sources) == 0 {
		return t, "", errors.New("name and sources required")
	}
	if expires != nil && !expires.After(t.Created) {
		return t, "", errors.New("expiry must be in future")
	}
	for _, id := range sources {
		var n int
		if e := s.DB.QueryRowContext(ctx, "SELECT count(*) FROM sources WHERE id=? AND retired=0", id).Scan(&n); e != nil {
			return t, "", e
		}
		if n == 0 {
			return t, "", errors.New("unknown source")
		}
	}
	secret := make([]byte, 32)
	if _, e := rand.Read(secret); e != nil {
		return t, "", e
	}
	value := hex.EncodeToString(secret)
	sum := sha256.Sum256([]byte(value))
	b, _ := json.Marshal(t)
	_, e := s.DB.ExecContext(ctx, "INSERT INTO tokens VALUES(?,?,?)", t.ID, sum[:], string(b))
	return t, t.ID + "." + value, e
}
func (s *Store) Authenticate(ctx context.Context, value string) (Token, error) {
	var t Token
	parts := strings.Split(value, ".")
	if len(parts) != 2 || len(parts[0]) != 32 || len(parts[1]) != 64 {
		return t, errors.New("unauthorized")
	}
	var digest []byte
	var b string
	e := s.DB.QueryRowContext(ctx, "SELECT digest,body FROM tokens WHERE id=?", parts[0]).Scan(&digest, &b)
	if e != nil {
		return t, errors.New("unauthorized")
	}
	sum := sha256.Sum256([]byte(parts[1]))
	if subtle.ConstantTimeCompare(digest, sum[:]) != 1 {
		return t, errors.New("unauthorized")
	}
	if e = json.Unmarshal([]byte(b), &t); e != nil {
		return t, e
	}
	if t.Revoked || (t.Expires != nil && !time.Now().Before(*t.Expires)) {
		return t, errors.New("unauthorized")
	}
	return t, nil
}
func (s *Store) Tokens(ctx context.Context) ([]Token, error) {
	rows, e := s.DB.QueryContext(ctx, "SELECT body FROM tokens ORDER BY rowid DESC")
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var b string
		var t Token
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &t); e != nil {
			return nil, e
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func (s *Store) Revoke(ctx context.Context, id string) error {
	var b string
	if e := s.DB.QueryRowContext(ctx, "SELECT body FROM tokens WHERE id=?", id).Scan(&b); e != nil {
		return e
	}
	var t Token
	if e := json.Unmarshal([]byte(b), &t); e != nil {
		return e
	}
	t.Revoked = true
	bts, _ := json.Marshal(t)
	_, e := s.DB.ExecContext(ctx, "UPDATE tokens SET body=? WHERE id=?", string(bts), id)
	return e
}

// BoolSetting reads management settings; they are not part of business snapshots.
func (s *Store) BoolSetting(ctx context.Context, key string) (bool, error) {
	var value bool
	err := s.DB.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return value, err
}
func (s *Store) SetBoolSetting(ctx context.Context, key string, value bool) error {
	_, err := s.DB.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}
