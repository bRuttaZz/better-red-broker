package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // pure Go SQLite (no cgo)
)

/*
Store is the broker's whole database: one table.

	pair
	  pair_id          TEXT PK   "<device-id>.<salted signature>"
	  data             TEXT      JSON: {"<key>": {"blob": "<base64url>", "updatedAt": <unix s>}, …}
	                             key "0" = her settings, "YYYY-MM-DD" = one day
	  updated_at       INTEGER   unix seconds of her last write
	  publisher_secret BLOB      the pair secret she sends with every write (the partner's one-time
	                             codes are made from it)

The broker is a dumb cache: a row only appears when her phone first writes, and if the whole
database is lost her phone simply writes everything again. Blobs are end-to-end encrypted on the
phone; the server can't read them and never sees the decryption key.

Two connection pools share the file:
  - writer: exactly one connection, transactions start IMMEDIATE, so writers queue instead of failing;
  - readers: several read-only connections. WAL mode lets them read while the writer writes.
*/
type Store struct {
	w *sql.DB
	r *sql.DB
}

// Entry is one value in pair.data.
type Entry struct {
	Blob      string `json:"blob,omitempty"`
	UpdatedAt int64  `json:"updatedAt"`
}

// Pair is a row as the API needs it.
type Pair struct {
	Secret    []byte
	Data      map[string]Entry
	UpdatedAt int64
}

var errNotFound = errors.New("not found")
var errFull = errors.New("server full")

// Pragmas for every connection. busy_timeout: wait (not fail) when another connection holds a lock.
// synchronous=NORMAL is safe with WAL (a power cut can lose the last commits, never corrupt).
func dsn(path string, readOnly bool) string {
	q := url.Values{}
	for _, p := range []string{
		"busy_timeout(10000)",
		"foreign_keys(ON)",
		"synchronous(NORMAL)",
		"temp_store(MEMORY)",
		"cache_size(-16000)", // ~16 MB page cache per connection
	} {
		q.Add("_pragma", p)
	}
	if readOnly {
		q.Add("_pragma", "query_only(ON)")
		q.Set("mode", "ro")
	} else {
		q.Add("_pragma", "journal_size_limit(67108864)") // keep the WAL file under 64 MB after checkpoints
		q.Set("_txlock", "immediate")
	}
	return "file:" + path + "?" + q.Encode()
}

func OpenStore(path string, readers int) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	w.SetMaxIdleConns(1)
	w.SetConnMaxLifetime(0)
	if err := migrate(w); err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(readers)
	r.SetMaxIdleConns(readers)
	if err := r.Ping(); err != nil {
		w.Close()
		r.Close()
		return nil, err
	}
	return &Store{w: w, r: r}, nil
}

func (s *Store) Close() error {
	return errors.Join(s.r.Close(), s.w.Close())
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v >= 1 {
		return nil
	}
	// auto_vacuum must be chosen before the first table exists. INCREMENTAL lets the clean-up
	// job give freed pages back to the disk a little at a time.
	for _, stmt := range []string{
		"PRAGMA auto_vacuum = INCREMENTAL",
		"PRAGMA journal_mode = WAL",
		`CREATE TABLE IF NOT EXISTS pair (
			pair_id          TEXT    PRIMARY KEY NOT NULL,
			data             TEXT    NOT NULL DEFAULT '{}' CHECK (json_valid(data)),
			updated_at       INTEGER NOT NULL,
			publisher_secret BLOB    NOT NULL CHECK (length(publisher_secret) = 32)
		) STRICT`,
		"CREATE INDEX IF NOT EXISTS pair_updated_at ON pair(updated_at)",
		"PRAGMA user_version = 1",
	} {
		if _, err := db.Exec(stmt); err != nil {
			return fmt.Errorf("migrate: %s: %w", stmt, err)
		}
	}
	return nil
}

// DeleteDevice removes every pair of a device ("<device-id>.*"). Used when she makes a new pairing.
func (s *Store) DeleteDevice(ctx context.Context, deviceID string) error {
	from, to := pairPrefixRange(deviceID)
	_, err := s.w.ExecContext(ctx, "DELETE FROM pair WHERE pair_id >= ? AND pair_id < ?", from, to)
	return err
}

func scanPair(row *sql.Row) (*Pair, error) {
	var p Pair
	var data string
	if err := row.Scan(&p.Secret, &data, &p.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound
		}
		return nil, err
	}
	if err := json.Unmarshal([]byte(data), &p.Data); err != nil {
		return nil, err
	}
	if p.Data == nil {
		p.Data = map[string]Entry{}
	}
	return &p, nil
}

// Get reads a pair (read-only pool).
func (s *Store) Get(ctx context.Context, pairID string) (*Pair, error) {
	return scanPair(s.r.QueryRowContext(ctx,
		"SELECT publisher_secret, data, updated_at FROM pair WHERE pair_id = ?", pairID))
}

// Upsert changes a pair's data in one write transaction, creating the row if it doesn't exist
// (first write, or the database was lost). The secret is stored as sent. [change] edits the map.
func (s *Store) Upsert(ctx context.Context, pairID string, secret []byte, now time.Time, maxPairs int, change func(map[string]Entry)) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := scanPair(tx.QueryRowContext(ctx,
		"SELECT publisher_secret, data, updated_at FROM pair WHERE pair_id = ?", pairID))
	if errors.Is(err, errNotFound) {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM pair").Scan(&n); err != nil {
			return err
		}
		if n >= maxPairs {
			return errFull
		}
		p = &Pair{Data: map[string]Entry{}}
	} else if err != nil {
		return err
	}
	change(p.Data)
	raw, err := json.Marshal(p.Data)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO pair (pair_id, data, updated_at, publisher_secret) VALUES (?1, ?2, ?3, ?4)
		ON CONFLICT (pair_id) DO UPDATE SET data = ?2, updated_at = ?3, publisher_secret = ?4`,
		pairID, string(raw), now.Unix(), secret); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Delete(ctx context.Context, pairID string) error {
	_, err := s.w.ExecContext(ctx, "DELETE FROM pair WHERE pair_id = ?", pairID)
	return err
}

// Cleanup is the periodic job:
//  1. delete pairs the publisher hasn't written for staleDays;
//  2. remove day keys older than oldestDay ("YYYY-MM-DD") from the rest; key "0" always stays.
//
// It works in small batches so normal writes never wait long behind it.
func (s *Store) Cleanup(ctx context.Context, staleBefore time.Time, oldestDay string) (deleted, pruned int64, err error) {
	const batch = 500
	for {
		res, err := s.w.ExecContext(ctx,
			"DELETE FROM pair WHERE rowid IN (SELECT rowid FROM pair WHERE updated_at < ? LIMIT ?)",
			staleBefore.Unix(), batch)
		if err != nil {
			return deleted, pruned, err
		}
		n, _ := res.RowsAffected()
		deleted += n
		if n < batch {
			break
		}
	}
	var maxRowID int64
	if err := s.r.QueryRowContext(ctx, "SELECT coalesce(max(rowid), 0) FROM pair").Scan(&maxRowID); err != nil {
		return deleted, pruned, err
	}
	for from := int64(0); from <= maxRowID; from += batch {
		res, err := s.w.ExecContext(ctx, `
			UPDATE pair SET data = (
				SELECT coalesce(json_group_object(key, json(value)), '{}') FROM json_each(pair.data)
				WHERE key = '0' OR key >= ?1)
			WHERE rowid > ?2 AND rowid <= ?3
			  AND EXISTS (SELECT 1 FROM json_each(pair.data) WHERE key <> '0' AND key < ?1)`,
			oldestDay, from, from+batch)
		if err != nil {
			return deleted, pruned, err
		}
		n, _ := res.RowsAffected()
		pruned += n
	}
	// Give freed pages back to the disk, fold the WAL into the main file, refresh query stats.
	for _, stmt := range []string{"PRAGMA incremental_vacuum(2000)", "PRAGMA wal_checkpoint(TRUNCATE)", "PRAGMA optimize"} {
		if _, err := s.w.ExecContext(ctx, stmt); err != nil {
			return deleted, pruned, err
		}
	}
	return deleted, pruned, nil
}

func (s *Store) Count(ctx context.Context) (int, error) {
	var n int
	err := s.r.QueryRowContext(ctx, "SELECT count(*) FROM pair").Scan(&n)
	return n, err
}
