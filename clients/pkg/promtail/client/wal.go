package client

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"sync"

	"github.com/grafana/loki/pkg/ingester"
	"github.com/grafana/loki/pkg/logproto"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/prometheus/tsdb/wlog"
)

var (
	NoopWAL    = &noopWAL{}
	recordPool = newRecordPool()
)

// WAL interface allows us to have a no-op WAL when the WAL is disabled.
type WAL interface {
	// Log marshalls the records and writes it into the WAL.
	Log(*ingester.WALRecord) error
	Delete() error
	Sync() error
	Dir() string
	DeleteSegment(segmentNum int) error
	NextSegment() (int, error)
}

type noopWAL struct{}

func (n noopWAL) Log(*ingester.WALRecord) error {
	return nil
}

func (n noopWAL) Delete() error {
	return nil
}

func (n noopWAL) Sync() error {
	return nil
}

func (n noopWAL) Dir() string {
	return ""
}

func (n noopWAL) DeleteSegment(segmentNum int) error {
	return nil
}

func (n noopWAL) NextSegment() (int, error) {
	return 0, nil
}

type walWrapper struct {
	wal *wlog.WL
	log log.Logger
}

// newWAL creates a WAL object. If the WAL is disabled, then the returned WAL is a no-op WAL. Note that the WAL created by
// newWAL uses as directory the following path structure: cfg.Dir/clientName/tenantID.
func newWAL(log log.Logger, registerer prometheus.Registerer, cfg WALConfig, clientName string, tenantID string) (WAL, error) {
	if !cfg.Enabled {
		return NoopWAL, nil
	}

	dir := path.Join(cfg.Dir, clientName, tenantID)
	tsdbWAL, err := wlog.NewSize(log, registerer, dir, wlog.DefaultSegmentSize, false)
	if err != nil {
		return nil, err
	}
	w := &walWrapper{
		wal: tsdbWAL,
		log: log,
	}

	return w, nil
}

func (w *walWrapper) Close() error {
	return w.wal.Close()
}

func (w *walWrapper) Delete() error {
	err := w.wal.Close()
	if err != nil {
		level.Warn(w.log).Log("msg", "failed to close WAL", "err", err)
	}
	err = os.RemoveAll(w.wal.Dir())
	return err
}

func (w *walWrapper) Log(record *ingester.WALRecord) error {
	if record == nil || (len(record.Series) == 0 && len(record.RefEntries) == 0) {
		return nil
	}

	// todo we don't new a pool this is synchronous
	buf := recordPool.GetBytes()[:0]
	defer func() {
		recordPool.PutBytes(buf)
	}()

	// Always write series then entries.
	if len(record.Series) > 0 {
		buf = record.EncodeSeries(buf)
		if err := w.wal.Log(buf); err != nil {
			return err
		}
		buf = buf[:0]
	}
	if len(record.RefEntries) > 0 {
		buf = record.EncodeEntries(ingester.CurrentEntriesRec, buf)
		if err := w.wal.Log(buf); err != nil {
			return err
		}

	}
	return nil
}

// Sync flushes changes to disk. Mainly to be used for testing.
func (w *walWrapper) Sync() error {
	return w.wal.Sync()
}

func (w *walWrapper) Dir() string {
	return w.wal.Dir()
}

func (w *walWrapper) DeleteSegment(segmentNum int) error {
	// First, find segment file name corresponding to segment number
	files, err := os.ReadDir(w.Dir())
	if err != nil {
		return fmt.Errorf("error reading wal dir")
	}
	var segmentName string
	for _, f := range files {
		fileName := f.Name()
		fileNameAsNumber, err := strconv.Atoi(fileName)
		if err != nil {
			continue
		}
		if fileNameAsNumber == segmentNum {
			// found segment to delete
			segmentName = fileName
			break
		}
	}
	if segmentName == "" {
		return fmt.Errorf("segment not found")
	}
	// Now we know the segment file name, delete it
	if err = os.Remove(filepath.Join(w.Dir(), segmentName)); err != nil {
		return fmt.Errorf("failed deleting segment: %w", err)
	}
	return nil
}

func (w *walWrapper) NextSegment() (int, error) {
	return w.wal.NextSegmentSync()
}

type resettingPool struct {
	rPool *sync.Pool // records
	ePool *sync.Pool // entries
	bPool *sync.Pool // bytes
}

func (p *resettingPool) GetRecord() *ingester.WALRecord {
	rec := p.rPool.Get().(*ingester.WALRecord)
	rec.Reset()
	return rec
}

func (p *resettingPool) PutRecord(r *ingester.WALRecord) {
	p.rPool.Put(r)
}

func (p *resettingPool) GetEntries() []logproto.Entry {
	return p.ePool.Get().([]logproto.Entry)
}

func (p *resettingPool) PutEntries(es []logproto.Entry) {
	p.ePool.Put(es[:0]) // nolint:staticcheck
}

func (p *resettingPool) GetBytes() []byte {
	return p.bPool.Get().([]byte)
}

func (p *resettingPool) PutBytes(b []byte) {
	p.bPool.Put(b[:0]) // nolint:staticcheck
}

func newRecordPool() *resettingPool {
	return &resettingPool{
		rPool: &sync.Pool{
			New: func() interface{} {
				return &ingester.WALRecord{}
			},
		},
		ePool: &sync.Pool{
			New: func() interface{} {
				return make([]logproto.Entry, 0, 512)
			},
		},
		bPool: &sync.Pool{
			New: func() interface{} {
				return make([]byte, 0, 1<<10) // 1kb
			},
		},
	}
}