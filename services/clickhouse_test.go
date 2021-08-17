package services

import (
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/urfave/cli"
)

type ClickHouseDB_Mock struct {
	db *sql.DB
}

func (s *ClickHouseDB_Mock) Get() (*sql.DB, error) {
	return s.db, nil
}

func TestClickHouse(t *testing.T) {
	app := cli.NewApp()
	RegisterClickHouseFlags(app)
	app.Action = func(c *cli.Context) error {
		db, mock, err := sqlmock.New()
		if err != nil {
			return nil
		}
		mock.ExpectExec("CREATE TABLE IF NOT EXISTS").WillReturnResult(sqlmock.NewResult(0, 0))
		mock.ExpectBegin()
		stmt := mock.ExpectPrepare("INSERT INTO")
		for i := 0; i < 1000; i++ {
			stmt.ExpectExec()
		}
		mock.ExpectCommit()
		mock.ExpectBegin()
		stmt = mock.ExpectPrepare("INSERT INTO")
		for i := 0; i < 1000; i++ {
			stmt.ExpectExec()
		}
		mock.ExpectCommit()
		mock.ExpectBegin()
		stmt = mock.ExpectPrepare("INSERT INTO")
		for i := 0; i < 100; i++ {
			stmt.ExpectExec()
		}
		mock.ExpectCommit()

		clickHouseDB := &ClickHouseDB_Mock{
			db: db,
		}

		clickHouse := NewClickHouse(c, clickHouseDB)

		for i := 0; i < 2100; i++ {
			if err = clickHouse.Add(&StatRecord{Timestamp: time.Now()}); err != nil {
				t.Errorf("Error while adding stats: %s", err)
			}
		}
		<-time.After(time.Millisecond * 100)

		if len(clickHouse.batch) != 100 {
			t.Errorf("Expected batch size %v got %v", 100, len(clickHouse.batch))
		}

		clickHouse.Close()

		if len(clickHouse.batch) != 0 {
			t.Errorf("Expected empty batch but %v records still reamins", len(clickHouse.batch))
		}

		if err := mock.ExpectationsWereMet(); err != nil {
			t.Errorf("There were unfulfilled expectations: %s", err)
		}

		return nil
	}
	args := os.Args[0:1]
	app.Run(args)
}
