package services

import (
	"database/sql"
	"sync"

	_ "github.com/ClickHouse/clickhouse-go/v2"
	_ "github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/urfave/cli"
)

type DBProvider interface {
	Get() (*sql.DB, error)
}

const (
	ClickhouseDSNFlag = "clickhouse-dsn"
)

func RegisterClickHouseDBFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.StringFlag{
			Name:   ClickhouseDSNFlag,
			Usage:  "clickhouse dsn",
			Value:  "",
			EnvVar: "CLICKHOUSE_DSN",
		},
	)
}

type ClickHouseDB struct {
	dsn  string
	err  error
	db   *sql.DB
	once sync.Once
}

func NewClickHouseDB(c *cli.Context) *ClickHouseDB {
	return &ClickHouseDB{
		dsn: c.String(ClickhouseDSNFlag),
	}
}

func (s *ClickHouseDB) Get() (*sql.DB, error) {
	s.once.Do(func() {
		s.db, s.err = sql.Open("clickhouse", s.dsn)
	})
	return s.db, s.err
}

func (s *ClickHouseDB) Close() {
	if s.db != nil {
		_ = s.db.Close()
	}
}
