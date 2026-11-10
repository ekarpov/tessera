// Package migrations embeds hand-written SQLite schema migrations.
package migrations

import _ "embed"

//go:embed 001_sqlite.sql
var SQLiteV1 string

//go:embed 002_sqlite.sql
var SQLiteV2 string

//go:embed 003_sqlite.sql
var SQLiteV3 string

//go:embed 004_sqlite.sql
var SQLiteV4 string

//go:embed 001_postgres.sql
var PostgresV1 string

//go:embed 005_sqlite.sql
var SQLiteV5 string

//go:embed 002_postgres.sql
var PostgresV2 string

//go:embed 006_sqlite.sql
var SQLiteV6 string

//go:embed 003_postgres.sql
var PostgresV3 string

//go:embed 007_sqlite.sql
var SQLiteV7 string

//go:embed 004_postgres.sql
var PostgresV4 string

//go:embed 005_postgres.sql
var PostgresV5 string

//go:embed 008_sqlite.sql
var SQLiteV8 string

//go:embed 006_postgres.sql
var PostgresV6 string

//go:embed 009_sqlite.sql
var SQLiteV9 string

//go:embed 007_postgres.sql
var PostgresV7 string

//go:embed 010_sqlite.sql
var SQLiteV10 string

//go:embed 008_postgres.sql
var PostgresV8 string

//go:embed 009_postgres.sql
var PostgresV9 string

//go:embed 011_sqlite.sql
var SQLiteV11 string

//go:embed 012_sqlite.sql
var SQLiteV12 string

//go:embed 010_postgres.sql
var PostgresV10 string

//go:embed 013_sqlite.sql
var SQLiteV13 string

//go:embed 011_postgres.sql
var PostgresV11 string

//go:embed 014_sqlite.sql
var SQLiteV14 string

//go:embed 012_postgres.sql
var PostgresV12 string

//go:embed 015_sqlite.sql
var SQLiteV15 string

//go:embed 013_postgres.sql
var PostgresV13 string

//go:embed 014_postgres.sql
var PostgresV14 string

//go:embed 016_sqlite.sql
var SQLiteV16 string

//go:embed 015_postgres.sql
var PostgresV15 string

//go:embed 017_sqlite.sql
var SQLiteV17 string
