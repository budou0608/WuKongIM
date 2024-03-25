package wkdb

import (
	"encoding/binary"
	"path/filepath"

	"github.com/cockroachdb/pebble"
)

var _ DB = (*pebbleDB)(nil)

type pebbleDB struct {
	db     *pebble.DB
	opts   *Options
	wo     *pebble.WriteOptions
	endian binary.ByteOrder
}

func NewPebbleDB(opts *Options) *pebbleDB {
	return &pebbleDB{
		opts:   opts,
		endian: binary.BigEndian,
		wo:     &pebble.WriteOptions{},
	}
}

func (p *pebbleDB) Open() error {
	var err error
	p.db, err = pebble.Open(filepath.Join(p.opts.DataDir, "wukongimdb"), &pebble.Options{
		FormatMajorVersion: pebble.FormatNewest,
	})
	if err != nil {
		return err
	}
	return nil
}

func (p *pebbleDB) Close() error {
	return p.db.Close()
}