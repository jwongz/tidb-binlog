package loader

import (
	"context"
	gosql "database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	"github.com/prometheus/client_golang/prometheus"
	"golang.org/x/sync/errgroup"
)

var defaultBatchSize = 128

type executor struct {
	db                *gosql.DB
	batchSize         int
	queryHistogramVec *prometheus.HistogramVec
}

func newExecutor(db *gosql.DB) *executor {
	exe := &executor{
		db:        db,
		batchSize: defaultBatchSize,
	}

	return exe
}

func (e *executor) withBatchSize(batchSize int) *executor {
	e.batchSize = batchSize
	return e
}

func (e *executor) withQueryHistogramVec(queryHistogramVec *prometheus.HistogramVec) *executor {
	e.queryHistogramVec = queryHistogramVec
	return e
}

func groupByTable(dmls []*DML) (tables map[string][]*DML) {
	if len(dmls) == 0 {
		return nil
	}

	tables = make(map[string][]*DML)
	for _, dml := range dmls {
		table := quoteSchema(dml.Database, dml.Table)
		tableDMLs := tables[table]
		tableDMLs = append(tableDMLs, dml)
		tables[table] = tableDMLs
	}

	return
}

func (e *executor) execTableBatchRetry(dmls []*DML, retryNum int, backoff time.Duration) error {
	var err error
	for i := 0; i < retryNum; i++ {
		if i > 0 {
			time.Sleep(backoff)
		}

		err = e.execTableBatch(dmls)
		if err == nil {
			return nil
		}
	}
	return errors.Trace(err)
}

// a wrap of *sql.Tx with metrics
type tx struct {
	*gosql.Tx
	queryHistogramVec *prometheus.HistogramVec
}

// wrap of sql.Tx.Exec()
func (tx *tx) exec(query string, args ...interface{}) (gosql.Result, error) {
	start := time.Now()
	res, err := tx.Tx.Exec(query, args...)
	if tx.queryHistogramVec != nil {
		tx.queryHistogramVec.WithLabelValues("exec").Observe(time.Since(start).Seconds())
	}

	return res, err
}

// wrap of sql.Tx.Commit()
func (tx *tx) commit() error {
	start := time.Now()
	err := tx.Tx.Commit()
	if tx.queryHistogramVec != nil {
		tx.queryHistogramVec.WithLabelValues("commit").Observe(time.Since(start).Seconds())
	}

	return errors.Trace(err)
}

// return a wrap of sql.Tx
func (e *executor) begin() (*tx, error) {
	sqlTx, err := e.db.Begin()
	if err != nil {
		return nil, errors.Trace(err)
	}

	return &tx{
		Tx:                sqlTx,
		queryHistogramVec: e.queryHistogramVec,
	}, nil
}

func (e *executor) bulkDelete(deletes []*DML) error {
	var sqls strings.Builder
	var argss []interface{}

	for _, dml := range deletes {
		sql, args := dml.sql()
		sqls.WriteString(sql)
		sqls.WriteByte(';')
		argss = append(argss, args...)
	}
	tx, err := e.begin()
	if err != nil {
		return errors.Trace(err)
	}
	sql := sqls.String()
	_, err = tx.exec(sql, argss...)
	if err != nil {
		log.Error("exec fail sql: %s, args: %v", sql, argss)
		tx.Rollback()
		return errors.Trace(err)
	}

	err = tx.commit()
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func (e *executor) bulkReplace(inserts []*DML) error {
	if len(inserts) == 0 {
		return nil
	}

	info := inserts[0].info
	dbName := inserts[0].Database
	tableName := inserts[0].Table

	builder := new(strings.Builder)

	fmt.Fprintf(builder, "REPLACE INTO %s(%s) VALUES ", quoteSchema(dbName, tableName), buildColumnList(info.columns))

	holder := fmt.Sprintf("(%s)", holderString(len(info.columns)))
	for i := 0; i < len(inserts); i++ {
		if i > 0 {
			builder.WriteByte(',')
		}
		builder.WriteString(holder)
	}

	var args []interface{}
	for _, insert := range inserts {
		for _, name := range info.columns {
			v := insert.Values[name]
			args = append(args, v)
		}
	}

	tx, err := e.begin()
	if err != nil {
		return errors.Trace(err)
	}
	_, err = tx.exec(builder.String(), args...)
	if err != nil {
		log.Errorf("exec fail sql: %s, args: %v, err: %v", builder.String(), args, err)
		tx.Rollback()
		return errors.Trace(err)
	}
	err = tx.commit()
	if err != nil {
		return errors.Trace(err)
	}
	return nil

}

// we merge dmls by primary key, after merge by key, we
// have only one dml for one primary key which contains the newest value(like a kv store),
// to avoid other column's duplicate entry, we should apply delete dmls first, then insert&update
// use replace to handle the update unique index case(see https://github.com/pingcap/tidb-binlog/pull/437/files)
// or we can simply check if it update unique index column or not, and for update change to (delete + insert)
// the final result should has no duplicate entry or the origin dmls is wrong.
func (e *executor) execTableBatch(dmls []*DML) error {
	if len(dmls) == 0 {
		return nil
	}

	types, err := mergeByPrimaryKey(dmls)
	if err != nil {
		return errors.Trace(err)
	}

	log.Debugf("dmls: %v after merge: %v", dmls, types)

	if allDeletes, ok := types[DeleteDMLType]; ok {
		err := e.splitExecDML(allDeletes, e.bulkDelete)
		if err != nil {
			return errors.Trace(err)
		}
	}

	if allInserts, ok := types[InsertDMLType]; ok {
		err := e.splitExecDML(allInserts, e.bulkReplace)
		if err != nil {
			return errors.Trace(err)
		}
	}

	if allUpdates, ok := types[UpdateDMLType]; ok {
		err := e.splitExecDML(allUpdates, e.bulkReplace)
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

// splitExecDML split dmls to size of e.batchSize and call exec concurrently
func (e *executor) splitExecDML(dmls []*DML, exec func(dmls []*DML) error) error {
	errg, _ := errgroup.WithContext(context.Background())

	for _, split := range splitDMLs(dmls, e.batchSize) {
		split := split
		errg.Go(func() error {
			err := exec(split)
			if err != nil {
				return errors.Trace(err)
			}
			return nil
		})
	}

	return errors.Trace(errg.Wait())
}

func (e *executor) singleExecRetry(allDMLs []*DML, safeMode bool, retryNum int, backoff time.Duration) error {
	var err error

	for _, dmls := range splitDMLs(allDMLs, e.batchSize) {
		var i int
		for i = 0; i < retryNum; i++ {
			if i > 0 {
				time.Sleep(backoff)
			}

			err = e.singleExec(dmls, safeMode)
			if err == nil {
				break
			}
		}
		if err != nil {
			return errors.Trace(err)
		}
	}

	return nil
}

func (e *executor) singleExec(dmls []*DML, safeMode bool) error {
	tx, err := e.begin()
	if err != nil {
		return errors.Trace(err)
	}

	for _, dml := range dmls {
		if safeMode && dml.Tp == UpdateDMLType {
			sql, args := dml.deleteSQL()
			log.Debugf("exec: %s, args: %v", sql, args)
			_, err := tx.exec(sql, args...)
			if err != nil {
				log.Errorf("err: %v, exec dml sql: %s, args: %v", err, sql, args)
				tx.Rollback()
				return errors.Trace(err)
			}

			sql, args = dml.replaceSQL()
			log.Debugf("exec: %s, args: %v", sql, args)
			_, err = tx.exec(sql, args...)
			if err != nil {
				log.Errorf("err: %v, exec dml sql: %s, args: %v", err, sql, args)
				tx.Rollback()
				return errors.Trace(err)
			}
		} else if safeMode && dml.Tp == InsertDMLType {
			sql, args := dml.replaceSQL()
			log.Debugf("exec dml sql: %s, args: %v", sql, args)
			_, err := tx.exec(sql, args...)
			if err != nil {
				log.Errorf("err: %v, exec dml sql: %s, args: %v", err, sql, args)
				tx.Rollback()
				return errors.Trace(err)
			}
		} else {
			sql, args := dml.sql()
			log.Debugf("exec dml sql: %s, args: %v", sql, args)
			_, err := tx.exec(sql, args...)
			if err != nil {
				log.Errorf("err: %v, exec dml sql: %s, args: %v", err, sql, args)
				tx.Rollback()
				return errors.Trace(err)
			}
		}
	}

	err = tx.commit()
	return errors.Trace(err)
}