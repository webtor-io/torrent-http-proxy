package services

import (
	"database/sql"
	"sync"
	"time"

	_ "github.com/ClickHouse/clickhouse-go"
	"github.com/urfave/cli"
)

type DBProvider interface {
	Get() (*sql.DB, error)
}

const (
	CLICKHOUSE_DSN = "clickhouse-dsn"
)

func RegisterClickHouseDBFlags(c *cli.App) {
	c.Flags = append(c.Flags, cli.StringFlag{
		Name:   CLICKHOUSE_DSN,
		Usage:  "clickhouse dsn",
		Value:  "",
		EnvVar: "CLICKHOUSE_DSN",
	})
}

type ClickHouseDB struct {
	dsn  string
	err  error
	db   *sql.DB
	once sync.Once
}

func NewClickHouseDB(c *cli.Context) *ClickHouseDB {
	return &ClickHouseDB{
		dsn: c.String(CLICKHOUSE_DSN),
	}
}

func (s *ClickHouseDB) Get() (*sql.DB, error) {
	s.once.Do(func() {
		s.db, s.err = sql.Open("clickhouse", s.dsn)
	})
	if s.err != nil {
		s.db.SetConnMaxLifetime(15 * time.Minute)
	}
	return s.db, s.err
}

func (s *ClickHouseDB) Close() {
	if s.db != nil {
		s.db.Close()
	}
}
