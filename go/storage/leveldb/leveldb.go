// Package leveldb implements the LevelDB backed storage backend.
package leveldb

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"golang.org/x/net/context"

	"github.com/oasislabs/ekiden/go/common/logging"
	epochtime "github.com/oasislabs/ekiden/go/epochtime/api"
	"github.com/oasislabs/ekiden/go/storage/api"
)

const (
	// BackendName is the name of this implementation.
	BackendName = "leveldb"

	// DBFile is the default backing store filename.
	DBFile = "storage.leveldb.db"
)

var (
	_ api.Backend          = (*leveldbBackend)(nil)
	_ api.SweepableBackend = (*leveldbBackend)(nil)

	keyVersion = []byte("version")
	dbVersion  = []byte{0x00}

	prefixValues = []byte("values/")
)

type leveldbBackend struct {
	logger *logging.Logger
	db     *leveldb.DB
}

func (b *leveldbBackend) Get(ctx context.Context, key api.Key) ([]byte, error) {
	v, err := b.GetBatch(ctx, []api.Key{key})
	if err != nil {
		return nil, err
	}

	if v[0] == nil {
		return nil, api.ErrKeyNotFound
	}

	return v[0], nil
}

func (b *leveldbBackend) GetBatch(ctx context.Context, keys []api.Key) ([][]byte, error) {
	snapshot, err := b.db.GetSnapshot()
	if err != nil {
		return nil, err
	}
	defer snapshot.Release()

	var values [][]byte
	for _, key := range keys {
		value, err := snapshot.Get(append(prefixValues, key[:]...), nil)
		switch err {
		case nil:
			break
		case leveldb.ErrNotFound:
			value = nil
		default:
			return nil, err
		}

		values = append(values, value)
	}

	return values, nil
}

func (b *leveldbBackend) Insert(ctx context.Context, value []byte, expiration uint64) error {
	return b.InsertBatch(ctx, []api.Value{api.Value{Data: value, Expiration: expiration}})
}

func (b *leveldbBackend) InsertBatch(ctx context.Context, values []api.Value) error {
	b.logger.Debug("InsertBatch",
		"values", values,
	)

	batch := new(leveldb.Batch)
	for _, value := range values {
		hash := api.HashStorageKey(value.Data)
		key := append(prefixValues, hash[:]...)

		batch.Put(key, value.Data)
	}

	return b.db.Write(batch, nil)
}

func (b *leveldbBackend) GetKeys(ctx context.Context) ([]*api.KeyInfo, error) {
	var kiVec []*api.KeyInfo

	iter := b.db.NewIterator(util.BytesPrefix(prefixValues), nil)
	defer iter.Release()

	for iter.Next() {
		// TODO: Fetch actual expiration.
		ki := &api.KeyInfo{
			Expiration: epochtime.EpochInvalid,
		}
		copy(ki.Key[:], iter.Key()[len(prefixValues):])
		kiVec = append(kiVec, ki)
	}
	if err := iter.Error(); err != nil {
		return nil, err
	}

	return kiVec, nil
}

func (b *leveldbBackend) PurgeExpired(epoch epochtime.EpochTime) {
	// TODO: Purge expired items from database.
}

func (b *leveldbBackend) Cleanup() {
}

func (b *leveldbBackend) Initialized() <-chan struct{} {
	initCh := make(chan struct{})
	close(initCh)
	return initCh
}

func checkVersion(db *leveldb.DB) error {
	ver, err := db.Get(keyVersion, nil)
	switch err {
	case leveldb.ErrNotFound:
		return db.Put(keyVersion, dbVersion, nil)
	case nil:
		break
	default:
		return err
	}

	if !bytes.Equal(ver, dbVersion) {
		return fmt.Errorf("storage/leveldb: incompatible LevelDB store version: '%v'", hex.EncodeToString(ver))
	}

	return nil
}

// New constructs a new LevelDB backed storage Backend instance, using
// the provided path for the database.
func New(fn string, timeSource epochtime.Backend) (api.Backend, error) {
	db, err := leveldb.OpenFile(fn, &opt.Options{
		Compression: opt.NoCompression,
	})
	if err != nil {
		return nil, err
	}

	if err := checkVersion(db); err != nil {
		_ = db.Close()
		return nil, err
	}

	b := &leveldbBackend{
		logger: logging.GetLogger("storage/leveldb"),
		db:     db,
	}

	return b, nil
}
