package services

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"

	"github.com/urfave/cli"
)

const (
	clickhouseBatchSizeFlag  = "clickhouse-batch-size"
	clickhouseReplicatedFlag = "clickhouse-replicated"
	clickhouseShardedFlag    = "clickhouse-sharded"
)

func RegisterClickHouseFlags(f []cli.Flag) []cli.Flag {
	return append(f,
		cli.IntFlag{
			Name:   clickhouseBatchSizeFlag,
			Usage:  "clickhouse batch size",
			Value:  1000,
			EnvVar: "CLICKHOUSE_BATCH_SIZE",
		},
		cli.BoolFlag{
			Name:   clickhouseReplicatedFlag,
			Usage:  "clickhouse replication enabled",
			EnvVar: "CLICKHOUSE_REPLICATED",
		},
		cli.BoolFlag{
			Name:   clickhouseShardedFlag,
			Usage:  "clickhouse sharded enabled",
			EnvVar: "CLICKHOUSE_SHARDED",
		},
	)
}

type ClickHouse struct {
	db         DBProvider
	batchSize  int
	batch      []*StatRecord
	mux        sync.Mutex
	storeMux   sync.Mutex
	init       sync.Once
	nodeName   string
	replicated bool
	sharded    bool
}

type StatRecord struct {
	Timestamp     time.Time
	ApiKey        string
	Client        string
	BytesWritten  uint64
	TTFB          uint64
	Duration      uint64
	Path          string
	InfoHash      string
	OriginalPath  string
	SessionID     string
	Domain        string
	Status        uint64
	GroupedStatus uint64
	Edge          string
	Source        string
	Role          string
	Ads           bool
}

func NewClickHouse(c *cli.Context, db DBProvider) *ClickHouse {

	return &ClickHouse{
		db:         db,
		batchSize:  c.Int(clickhouseBatchSizeFlag),
		batch:      make([]*StatRecord, 0, c.Int(clickhouseBatchSizeFlag)),
		nodeName:   c.String(myNodeNameFlag),
		replicated: c.Bool(clickhouseReplicatedFlag),
		sharded:    c.Bool(clickhouseShardedFlag),
	}
}

func (s *ClickHouse) makeTable(db *sql.DB) error {
	table := "proxy_stat"
	tableExpr := table
	engine := "MergeTree()"
	ttl := "3 MONTH"
	if s.sharded {
		tableExpr += " on cluster '{cluster}'"
	}
	if s.replicated {
		engine = "ReplicatedMergeTree('/clickhouse/{installation}/{cluster}/tables/{shard}/{database}/{table}', '{replica}')"
	}
	_, err := db.Exec(fmt.Sprintf(strings.TrimSpace(`
		CREATE TABLE IF NOT EXISTS %v (
			timestamp      DateTime,
			api_key        String,
			client         String,
			bytes_written  UInt64,
			ttfb           UInt32,
			duration       UInt32,
			path           String,
			infohash       String,
			original_path  String,
			session_id     String,
			domain         String,
			status         UInt16,
			grouped_status UInt16,
			edge           String,
			source         String,
			role           String,
			ads            UInt8,
			node           String
		) engine = %v
		PARTITION BY toYYYYMM(timestamp)
		ORDER BY (timestamp)
		TTL timestamp + INTERVAL %v
	`), tableExpr, engine, ttl))
	if err != nil {
		return err
	}
	if s.sharded {
		_, err = db.Exec(fmt.Sprintf(strings.TrimSpace(`
			CREATE TABLE IF NOT EXISTS %v_all on cluster '{cluster}' as %v
			ENGINE = Distributed('{cluster}', default, %v, rand())
		`), table, table, table))
	}
	return err
}

func (s *ClickHouse) store(sr []*StatRecord) error {
	s.storeMux.Lock()
	if len(sr) == 0 {
		return nil
	}
	logrus.Infof("storing %v rows to ClickHouse", len(sr))
	defer func() {
		logrus.Infof("finish storing %v rows to ClickHouse", len(sr))
		s.storeMux.Unlock()
	}()
	db, err := s.db.Get()
	if err != nil {
		return errors.Wrapf(err, "failed to get ClickHouse DB")
	}
	s.init.Do(func() {
		err = s.makeTable(db)
	})
	if err != nil {
		return errors.Wrapf(err, "failed to create table")
	}
	err = db.Ping()
	if err != nil {
		return errors.Wrapf(err, "failed to ping")
	}
	tx, err := db.Begin()
	if err != nil {
		return errors.Wrapf(err, "failed to begin")
	}
	table := "proxy_stat"
	if s.replicated {
		table += "_all"
	}
	stmt, err := tx.Prepare(fmt.Sprintf(`INSERT INTO %v (timestamp, api_key, client, bytes_written, ttfb,
		duration, path, infohash, original_path, session_id, domain, status, grouped_status, edge,
		source, role, ads, node) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, table))
	if err != nil {
		return errors.Wrapf(err, "failed to prepare")
	}
	defer func(stmt *sql.Stmt) {
		_ = stmt.Close()
	}(stmt)
	for _, r := range sr {
		var adsUInt uint8
		if r.Ads {
			adsUInt = 1
		}
		_, err = stmt.Exec(
			r.Timestamp, r.ApiKey, r.Client, r.BytesWritten, uint32(r.TTFB),
			uint32(r.Duration), r.Path, r.InfoHash, r.OriginalPath, r.SessionID,
			r.Domain, uint16(r.Status), uint16(r.GroupedStatus), r.Edge, r.Source,
			r.Role, adsUInt, s.nodeName,
		)
		if err != nil {
			return errors.Wrapf(err, "failed to exec")
		}
	}
	err = tx.Commit()
	if err != nil {
		return errors.Wrapf(err, "failed to commit")
	}
	return nil
}

func (s *ClickHouse) Add(sr *StatRecord) error {
	s.mux.Lock()
	s.batch = append(s.batch, sr)
	s.mux.Unlock()
	if len(s.batch) >= s.batchSize {
		go func(b []*StatRecord) {
			err := s.store(b)
			if err != nil {
				logrus.WithError(err).Warn("failed to store to ClickHouse")
			}
		}(s.batch)
		s.mux.Lock()
		s.batch = make([]*StatRecord, 0, s.batchSize)
		s.mux.Unlock()
	}
	return nil
}

func (s *ClickHouse) Close() {
	_ = s.store(s.batch)
	s.batch = []*StatRecord{}
}
