package restore

import (
	"bufio"
	"io"
	"os"
	"path"

	"github.com/juju/errors"
	"github.com/ngaut/log"
	pkgsql "github.com/pingcap/tidb-binlog/pkg/sql"
	"github.com/pingcap/tidb-binlog/restore/executor"
	"github.com/pingcap/tidb-binlog/restore/translator"
)

type Restore struct {
	cfg        *Config
	translator translator.Translator
	executor   executor.Executor
}

func New(cfg *Config) (*Restore, error) {
	executor, err := executor.New(cfg.DestType, cfg.DestDB)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &Restore{
		cfg:        cfg,
		translator: translator.New(cfg.DestType, false),
		executor:   executor,
	}, nil
}

// func (r *Restore)

func (r *Restore) Start() error {
	dir := r.cfg.Dir

	// read all file names
	names, err := readBinlogNames(dir)
	if err != nil {
		log.Fatalf("read binlog file name error %v", err)
	}

	// find the target file's index
	index := searchFileIndex(names, "")
	log.Debugf("index %d", index)
	for _, name := range names[index:] {
		p := path.Join(dir, name)
		f, err := os.OpenFile(p, os.O_RDONLY, 0600)
		if err != nil {
			return errors.Errorf("open file %s error %v", name, err)
		}
		defer f.Close()

		reader := bufio.NewReader(io.Reader(f))
		for {
			payload, err := readBinlog(reader)
			if err != nil && err != io.EOF {
				return errors.Errorf("decode binlog error %v", err)
			}
			if err == io.EOF {
				break
			}
			sqls, args, isDDL, err := translator.Translate(payload, r.translator)
			if err != nil {
				return errors.Trace(err)
			}

			err = r.executor.Execute(sqls, args, isDDL)
			if err != nil {
				if !pkgsql.IgnoreDDLError(err) {
					return errors.Trace(err)
				}
				log.Warnf("[ignore ddl error][sql]%s[args]%v[error]%v", sqls, args, err)
			}
		}
	}

	return nil
}

func (r *Restore) Close() error {
	return nil
}