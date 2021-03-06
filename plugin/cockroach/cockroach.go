// Copyright 2020 Asim Aslam
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
// Original source: github.com/micro/go-plugins/v3/store/cockroach/cockroach.go

// Package cockroach implements the cockroach store
package cockroach

import (
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
	"github.com/micro/micro/v3/service/logger"
	"github.com/micro/micro/v3/service/store"
	"github.com/pkg/errors"
)

// DefaultDatabase is the namespace that the sql store
// will use if no namespace is provided.
var (
	DefaultDatabase = "micro"
	DefaultTable    = "micro"
)

var (
	re = regexp.MustCompile("[^a-zA-Z0-9]+")

	statements = map[string]string{
		"list":       "SELECT key, value, metadata, expiry FROM %s.%s WHERE key LIKE $1 ORDER BY key DESC LIMIT $2 OFFSET $3;",
		"read":       "SELECT key, value, metadata, expiry FROM %s.%s WHERE key = $1;",
		"readMany":   "SELECT key, value, metadata, expiry FROM %s.%s WHERE key LIKE $1;",
		"readOffset": "SELECT key, value, metadata, expiry FROM %s.%s WHERE key LIKE $1 ORDER BY key DESC LIMIT $2 OFFSET $3;",
		"write":      "INSERT INTO %s.%s(key, value, metadata, expiry) VALUES ($1, $2::bytea, $3, $4) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, metadata = EXCLUDED.metadata, expiry = EXCLUDED.expiry;",
		"delete":     "DELETE FROM %s.%s WHERE key = $1;",
	}
)

type sqlStore struct {
	options store.StoreOptions
	db      *sql.DB

	sync.RWMutex
	// known databases
	databases map[string]bool
}

func (s *sqlStore) getDB(database, table string) (string, string) {
	if len(database) == 0 {
		if len(s.options.Database) > 0 {
			database = s.options.Database
		} else {
			database = DefaultDatabase
		}
	}

	if len(table) == 0 {
		if len(s.options.Table) > 0 {
			table = s.options.Table
		} else {
			table = DefaultTable
		}
	}

	// store.namespace must only contain letters, numbers and underscores
	database = re.ReplaceAllString(database, "_")
	table = re.ReplaceAllString(table, "_")

	return database, table
}

func (s *sqlStore) createDB(database, table string) error {
	database, table = s.getDB(database, table)

	s.Lock()
	defer s.Unlock()

	if _, ok := s.databases[database+":"+table]; ok {
		return nil
	}

	if err := s.initDB(database, table); err != nil {
		return err
	}

	s.databases[database+":"+table] = true
	return nil
}

func (s *sqlStore) initDB(database, table string) error {
	if s.db == nil {
		return errors.New("Database connection not initialised")
	}

	// Create the namespace's database
	_, err := s.db.Exec(fmt.Sprintf("CREATE DATABASE IF NOT EXISTS %s;", database))
	if err != nil {
		return err
	}

	// Create a table for the namespace's prefix
	_, err = s.db.Exec(fmt.Sprintf(`CREATE TABLE IF NOT EXISTS %s.%s
	(
		key text NOT NULL,
		value bytea,
		metadata JSONB,
		expiry timestamp with time zone,
		CONSTRAINT %s_pkey PRIMARY KEY (key)
	);`, database, table, table))
	if err != nil {
		return errors.Wrap(err, "Couldn't create table")
	}

	// Create Index
	_, err = s.db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS "%s" ON %s.%s USING btree ("key");`, "key_index_"+table, database, table))
	if err != nil {
		return err
	}

	// Create Metadata Index
	_, err = s.db.Exec(fmt.Sprintf(`CREATE INDEX IF NOT EXISTS "%s" ON %s.%s USING GIN ("metadata");`, "metadata_index_"+table, database, table))
	if err != nil {
		return err
	}

	return nil
}

func (s *sqlStore) configure() error {
	if len(s.options.Nodes) == 0 {
		s.options.Nodes = []string{"postgresql://root@localhost:26257?sslmode=disable"}
	}

	source := s.options.Nodes[0]
	// check if it is a standard connection string eg: host=%s port=%d user=%s password=%s dbname=%s sslmode=disable
	// if err is nil which means it would be a URL like postgre://xxxx?yy=zz
	_, err := url.Parse(source)
	if err != nil {
		if !strings.Contains(source, " ") {
			source = fmt.Sprintf("host=%s", source)
		}
	}

	// create source from first node
	db, err := sql.Open("postgres", source)
	if err != nil {
		return err
	}

	if err := db.Ping(); err != nil {
		return err
	}

	if s.db != nil {
		s.db.Close()
	}

	// save the values
	s.db = db

	// get DB
	database, table := s.getDB(s.options.Database, s.options.Table)

	// initialise the database
	return s.initDB(database, table)
}

func (s *sqlStore) prepare(database, table, query string) (*sql.Stmt, error) {
	st, ok := statements[query]
	if !ok {
		return nil, errors.New("unsupported statement")
	}

	// get DB
	database, table = s.getDB(database, table)

	q := fmt.Sprintf(st, database, table)
	stmt, err := s.db.Prepare(q)
	if err != nil {
		return nil, err
	}
	return stmt, nil
}

func (s *sqlStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

func (s *sqlStore) Init(opts ...store.StoreOption) error {
	for _, o := range opts {
		o(&s.options)
	}
	// reconfigure
	return s.configure()
}

// List all the known records
func (s *sqlStore) List(opts ...store.ListOption) ([]string, error) {
	var options store.ListOptions
	for _, o := range opts {
		o(&options)
	}

	// create the db if not exists
	if err := s.createDB(options.Database, options.Table); err != nil {
		return nil, err
	}
	limit := sql.NullInt32{}
	offset := 0
	pattern := "%"
	if options.Prefix != "" || options.Suffix != "" {
		if options.Prefix != "" {
			pattern = options.Prefix + pattern
		}
		if options.Suffix != "" {
			pattern = pattern + options.Suffix
		}
	}
	if options.Offset > 0 {
		offset = int(options.Offset)
	}
	if options.Limit > 0 {
		limit = sql.NullInt32{Int32: int32(options.Limit), Valid: true}
	}

	st, err := s.prepare(options.Database, options.Table, "list")
	if err != nil {
		return nil, err
	}
	defer st.Close()

	rows, err := st.Query(pattern, limit, offset)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	defer rows.Close()
	var keys []string
	records, err := s.rowsToRecords(rows)
	if err != nil {
		return nil, err
	}
	for _, k := range records {
		keys = append(keys, k.Key)
	}
	rowErr := rows.Close()
	if rowErr != nil {
		// transaction rollback or something
		return keys, rowErr
	}
	if err := rows.Err(); err != nil {
		return keys, err
	}
	return keys, nil
}

// rowToRecord converts from sql.Row to a store.Record. If the record has expired it will issue a delete in a separate goroutine
func (s *sqlStore) rowToRecord(row *sql.Row) (*store.Record, error) {
	var timehelper pq.NullTime
	record := &store.Record{}
	metadata := make(Metadata)

	if err := row.Scan(&record.Key, &record.Value, &metadata, &timehelper); err != nil {
		if err == sql.ErrNoRows {
			return record, store.ErrNotFound
		}
		return nil, err
	}

	// set the metadata
	record.Metadata = toMetadata(&metadata)
	if timehelper.Valid {
		if timehelper.Time.Before(time.Now()) {
			// record has expired
			go s.Delete(record.Key)
			return nil, store.ErrNotFound
		}
		record.Expiry = time.Until(timehelper.Time)

	}
	return record, nil
}

// rowsToRecords converts from sql.Rows to  []*store.Record. If a record has expired it will issue a delete in a separate goroutine
func (s *sqlStore) rowsToRecords(rows *sql.Rows) ([]*store.Record, error) {
	var records []*store.Record
	var timehelper pq.NullTime

	for rows.Next() {
		record := &store.Record{}
		metadata := make(Metadata)

		if err := rows.Scan(&record.Key, &record.Value, &metadata, &timehelper); err != nil {
			return records, err
		}

		// set the metadata
		record.Metadata = toMetadata(&metadata)

		if timehelper.Valid {
			if timehelper.Time.Before(time.Now()) {
				// record has expired
				go s.Delete(record.Key)
			} else {
				record.Expiry = time.Until(timehelper.Time)
				records = append(records, record)
			}
		} else {
			records = append(records, record)
		}
	}
	return records, nil
}

// Read a single key
func (s *sqlStore) Read(key string, opts ...store.ReadOption) ([]*store.Record, error) {
	var options store.ReadOptions
	for _, o := range opts {
		o(&options)
	}

	// create the db if not exists
	if err := s.createDB(options.Database, options.Table); err != nil {
		return nil, err
	}

	if options.Prefix || options.Suffix {
		return s.read(key, options)
	}

	st, err := s.prepare(options.Database, options.Table, "read")
	if err != nil {
		return nil, err
	}
	defer st.Close()

	row := st.QueryRow(key)
	record, err := s.rowToRecord(row)
	if err != nil {
		return nil, err
	}
	var records []*store.Record
	return append(records, record), nil
}

// Read Many records
func (s *sqlStore) read(key string, options store.ReadOptions) ([]*store.Record, error) {
	pattern := "%"
	if options.Prefix {
		pattern = key + pattern
	}
	if options.Suffix {
		pattern = pattern + key
	}

	var rows *sql.Rows
	var st *sql.Stmt
	var err error

	if options.Limit != 0 {
		st, err = s.prepare(options.Database, options.Table, "readOffset")
		if err != nil {
			return nil, err
		}
		defer st.Close()

		rows, err = st.Query(pattern, options.Limit, options.Offset)
	} else {
		st, err = s.prepare(options.Database, options.Table, "readMany")
		if err != nil {
			return nil, err
		}
		defer st.Close()

		rows, err = st.Query(pattern)
	}
	if err != nil {
		if err == sql.ErrNoRows {
			return []*store.Record{}, nil
		}
		return []*store.Record{}, errors.Wrap(err, "sqlStore.read failed")
	}

	defer rows.Close()

	records, err := s.rowsToRecords(rows)
	if err != nil {
		return nil, err
	}
	rowErr := rows.Close()
	if rowErr != nil {
		// transaction rollback or something
		return records, rowErr
	}
	if err := rows.Err(); err != nil {
		return records, err
	}

	return records, nil
}

// Write records
func (s *sqlStore) Write(r *store.Record, opts ...store.WriteOption) error {
	var options store.WriteOptions
	for _, o := range opts {
		o(&options)
	}

	// create the db if not exists
	if err := s.createDB(options.Database, options.Table); err != nil {
		return err
	}

	st, err := s.prepare(options.Database, options.Table, "write")
	if err != nil {
		return err
	}
	defer st.Close()

	metadata := make(Metadata)
	for k, v := range r.Metadata {
		metadata[k] = v
	}

	var expiry time.Time
	if r.Expiry != 0 {
		expiry = time.Now().Add(r.Expiry)
	}

	if expiry.IsZero() {
		_, err = st.Exec(r.Key, r.Value, metadata, nil)
	} else {
		_, err = st.Exec(r.Key, r.Value, metadata, expiry)
	}

	if err != nil {
		return errors.Wrap(err, "Couldn't insert record "+r.Key)
	}

	return nil
}

// Delete records with keys
func (s *sqlStore) Delete(key string, opts ...store.DeleteOption) error {
	var options store.DeleteOptions
	for _, o := range opts {
		o(&options)
	}

	// create the db if not exists
	if err := s.createDB(options.Database, options.Table); err != nil {
		return err
	}

	st, err := s.prepare(options.Database, options.Table, "delete")
	if err != nil {
		return err
	}
	defer st.Close()

	result, err := st.Exec(key)
	if err != nil {
		return err
	}

	_, err = result.RowsAffected()
	if err != nil {
		return err
	}

	return nil
}

func (s *sqlStore) Options() store.StoreOptions {
	return s.options
}

func (s *sqlStore) String() string {
	return "cockroach"
}

// NewStore returns a new micro Store backed by sql
func NewStore(opts ...store.StoreOption) store.Store {
	options := store.StoreOptions{
		Database: DefaultDatabase,
		Table:    DefaultTable,
	}

	for _, o := range opts {
		o(&options)
	}

	// new store
	s := new(sqlStore)
	// set the options
	s.options = options
	// mark known databases
	s.databases = make(map[string]bool)
	// best-effort configure the store
	if err := s.configure(); err != nil {
		if logger.V(logger.ErrorLevel, logger.DefaultLogger) {
			logger.Error("Error configuring store ", err)
		}
	}

	// return store
	return s
}
