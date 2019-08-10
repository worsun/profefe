package badger

import (
	"bytes"
	"context"
	"encoding/binary"
	"time"

	"github.com/dgraph-io/badger"
	pprofProfile "github.com/profefe/profefe/internal/pprof/profile"
	"github.com/profefe/profefe/pkg/log"
	"github.com/profefe/profefe/pkg/profile"
	"github.com/profefe/profefe/pkg/storage"
	"golang.org/x/xerrors"
)

const profilePrefix byte = 1 << 7

const (
	serviceIndexID byte = profilePrefix | 1 + iota
	typeIndexID
	labelsIndexID
)

// see https://godoc.org/github.com/rs/xid
const sizeOfProfileID = 12

type Storage struct {
	logger *log.Logger
	db     *badger.DB
	ttl    time.Duration
}

var _ storage.Storage = (*Storage)(nil)

func New(logger *log.Logger, db *badger.DB, ttl time.Duration) *Storage {
	return &Storage{
		logger: logger,
		db:     db,
	}
}

func (st *Storage) WriteProfile(ctx context.Context, ptype profile.ProfileType, meta *profile.ProfileMeta, pf *profile.ProfileFactory) error {
	entries := make([]*badger.Entry, 0, 1+2+len(meta.Labels)) // 1 for profile entry, 2 for general indexes

	pid := profile.NewProfileID()
	pKey, pVal, err := createProfileKV(pid, meta, pf)
	if err != nil {
		return xerrors.Errorf("could not create key-value pair for profile: %w", err)
	}

	// FIXME(narqo) ignore error since any parsing errors were caught above, ugly but works
	pp, _ := pf.Profile()
	entries = append(entries, st.newBadgerEntry(pKey, pVal))

	// add indexes
	entries = append(entries, st.newBadgerEntry(createIndexKey(serviceIndexID, []byte(meta.Service), pp.TimeNanos, pid), nil))
	{
		indexVal := append(make([]byte, 0, len(meta.Service)+1), meta.Service...)
		indexVal = append(indexVal, byte(ptype))
		entries = append(entries, st.newBadgerEntry(createIndexKey(typeIndexID, indexVal, pp.TimeNanos, pid), nil))
	}

	for _, label := range meta.Labels {
		entries = append(entries, st.newBadgerEntry(createIndexKey(labelsIndexID, []byte(meta.Service+label.Key+label.Value), pp.TimeNanos, pid), nil))
	}

	return st.db.Update(func(txn *badger.Txn) error {
		for i := range entries {
			st.logger.Debugw("writeProfile: set entry", "pid", pid, "pk", entries[i].Key, "expires_at", entries[i].ExpiresAt)
			if err := txn.SetEntry(entries[i]); err != nil {
				return xerrors.Errorf("could not write entry: %w", err)
			}
		}
		return nil
	})
}

func (st *Storage) newBadgerEntry(key, val []byte) *badger.Entry {
	entry := badger.NewEntry(key, val)
	if st.ttl > 0 {
		entry = entry.WithTTL(st.ttl)
	}
	return entry
}

// key is profilePrefix<pid><created-at><instance-id>, value encoded pprof data
func createProfileKV(pid profile.ProfileID, meta *profile.ProfileMeta, pf *profile.ProfileFactory) ([]byte, []byte, error) {
	// writeTo parses profile from internal reader if profile data isn't available yet
	var buf bytes.Buffer
	if err := pf.WriteTo(&buf); err != nil {
		return nil, nil, err
	}

	pp, err := pf.Profile()
	if err != nil {
		return nil, nil, err
	}

	key := make([]byte, 0, len(pid)+len(meta.InstanceID)+1+8) // 1 for prefix, 8 for created-at nanos
	key = append(key, profilePrefix)
	key = append(key, pid...)
	{
		tb := make([]byte, 8)
		binary.BigEndian.PutUint64(tb, uint64(pp.TimeNanos))
		key = append(key, tb...)
	}
	key = append(key, meta.InstanceID...)

	return key, buf.Bytes(), nil
}

func createProfilePK(pid profile.ProfileID) []byte {
	key := make([]byte, 0, len(pid)+1) // 1 for prefix
	key = append(key, profilePrefix)
	key = append(key, pid...)
	return key
}

// index key <index-id><index-val><created-at><pid>
func createIndexKey(indexID byte, indexVal []byte, createdAt int64, pid profile.ProfileID) []byte {
	var buf bytes.Buffer

	buf.WriteByte(indexID)
	buf.Write(indexVal)
	binary.Write(&buf, binary.BigEndian, createdAt)
	buf.Write(pid)

	return buf.Bytes()
}

func (st *Storage) GetProfile(ctx context.Context, pid profile.ProfileID) (*profile.ProfileFactory, error) {
	ppf, err := st.getProfiles(ctx, []profile.ProfileID{pid})
	if err != nil {
		return nil, err
	}

	if len(ppf) == 1 {
		return ppf[0], nil
	}

	return nil, xerrors.Errorf("found %d profiles for id %s", len(ppf), pid)
}

func (st *Storage) getProfiles(ctx context.Context, pids []profile.ProfileID) ([]*profile.ProfileFactory, error) {
	ppf := make([]*profile.ProfileFactory, 0, len(pids))

	// profile key prefixes
	prefixes := make([][]byte, 0, len(pids))
	for _, pid := range pids {
		pk := createProfilePK(pid)
		st.logger.Debugw("getProfiles: create pk", "pid", pid, "pk", pk)
		prefixes = append(prefixes, pk)
	}

	err := st.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchSize = 10 // pk keys are sorted
		it := txn.NewIterator(opts)
		defer it.Close()

		for _, prefix := range prefixes {
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				err := it.Item().Value(func(val []byte) error {
					// it's important to copy value to the buffer
					var buf bytes.Buffer
					buf.Write(val)
					ppf = append(ppf, profile.NewProfileFactoryFrom(&buf))
					return nil
				})
				if err != nil {
					return err
				}
			}
		}

		if len(ppf) == 0 {
			return storage.ErrNotFound
		}

		return nil
	})

	return ppf, err
}

func (st *Storage) FindProfile(ctx context.Context, req *storage.FindProfileRequest) (*profile.ProfileFactory, error) {
	pids, err := st.FindProfileIDs(ctx, req)
	if err != nil {
		return nil, err
	}

	ppf, err := st.getProfiles(ctx, pids)
	if err != nil {
		return nil, err
	}

	pps := make([]*pprofProfile.Profile, len(ppf))
	for i, pf := range ppf {
		pps[i], err = pf.Profile()
		if err != nil {
			return nil, err
		}
	}

	pp, err := pprofProfile.Merge(pps)
	if err != nil {
		return nil, xerrors.Errorf("could not merge %d profiles: %w", len(ppf), err)
	}
	return profile.NewProfileFactory(pp), nil
}

func (st *Storage) FindProfiles(ctx context.Context, req *storage.FindProfileRequest) ([]*profile.ProfileFactory, error) {
	pids, err := st.FindProfileIDs(ctx, req)
	if err != nil {
		return nil, err
	}

	return st.getProfiles(ctx, pids)
}

func (st *Storage) FindProfileIDs(ctx context.Context, req *storage.FindProfileRequest) ([]profile.ProfileID, error) {
	if req.Service == "" {
		return nil, xerrors.New("empty service")
	}

	indexesToScan := make([][]byte, 0, 1)
	{
		indexKey := make([]byte, 0, 64)
		if req.Type != profile.UnknownProfile {
			// by-service-type
			indexKey = append(indexKey, typeIndexID)
			indexKey = append(indexKey, req.Service...)
			indexKey = append(indexKey, byte(req.Type))
		} else {
			// by-service
			indexKey = append(indexKey, serviceIndexID)
			indexKey = append(indexKey, req.Service...)
		}

		indexesToScan = append(indexesToScan, indexKey)

		// by-service-type-labels
		for _, label := range req.Labels {
			indexKey := make([]byte, 0, 1+len(req.Service)+len(label.Key)+len(label.Value))
			indexKey = append(indexKey, labelsIndexID)
			indexKey = append(indexKey, req.Service...)
			indexKey = append(indexKey, label.Key...)
			indexKey = append(indexKey, label.Value...)
			indexesToScan = append(indexesToScan, indexKey)
		}
	}

	ids := make([][]profile.ProfileID, 0, len(indexesToScan))

	// scan prepared indexes
	for i, s := range indexesToScan {
		keys, err := st.scanIndexKeys(s, req.CreatedAtMin, req.CreatedAtMax)
		if err != nil {
			return nil, err
		}

		ids = append(ids, make([]profile.ProfileID, 0, len(keys)))
		for _, k := range keys {
			pid := k[len(k)-sizeOfProfileID:]
			st.logger.Debugw("findProfileIDs: found profile id", "pid", pid)
			ids[i] = append(ids[i], pid)
		}
	}

	if len(ids) == 0 {
		return nil, storage.ErrNotFound
	}

	return mergeProfileIDs(ids, req), nil
}

func (st *Storage) scanIndexKeys(indexKey []byte, createdAtMin, createdAtMax time.Time) (keys [][]byte, err error) {
	createdAtBytes := make([]byte, 8)
	binary.BigEndian.PutUint64(createdAtBytes, uint64(createdAtMin.UnixNano()))

	err = st.db.View(func(txn *badger.Txn) error {
		opts := badger.DefaultIteratorOptions
		opts.PrefetchValues = false // we're interested in keys only

		it := txn.NewIterator(opts)
		defer it.Close()

		// key to start scan from
		key := append([]byte{}, indexKey...)
		key = append(key, createdAtBytes...)

		st.logger.Debugw("scanIndexKeys", "key", key)

		for it.Seek(key); isScanIteratorValid(it, indexKey, uint64(createdAtMax.UnixNano())); it.Next() {
			item := it.Item()

			// check if item's key chunk before the timestamp is equal indexKey
			tsStartPos := len(it.Item().Key()) - sizeOfProfileID - 8
			itemKey := item.Key()[:tsStartPos]

			st.logger.Debugw("scanIndexKeys: check keys", "indexKey", indexKey, "itemKey", itemKey)

			if bytes.Equal(indexKey, itemKey) {
				var key []byte
				key = item.KeyCopy(key)
				keys = append(keys, key)
			}
		}
		return nil
	})

	return keys, err
}

func isScanIteratorValid(it *badger.Iterator, prefix []byte, tsEnd uint64) bool {
	if !it.Valid() || it.Item() == nil {
		return false
	}

	tsStartPos := len(it.Item().Key()) - sizeOfProfileID - 8 // 8 is for created-at nanos
	ts := binary.BigEndian.Uint64(it.Item().Key()[tsStartPos:])

	return it.ValidForPrefix(prefix) && ts <= tsEnd
}

// does merge part of sort-merge join
func mergeProfileIDs(ids [][]profile.ProfileID, req *storage.FindProfileRequest) (res []profile.ProfileID) {
	mergedIDs := ids[0]

	if len(ids) > 1 {
		for i := 1; i < len(ids); i++ {
			merged := make([]profile.ProfileID, 0, len(mergedIDs))
			k := len(mergedIDs) - 1
			for j := len(ids[i]) - 1; j >= 0 && k >= 0; {
				switch bytes.Compare(mergedIDs[k], ids[i][j]) {
				case 0:
					// left == right
					merged = append(merged, mergedIDs[k])
					k--
				case 1:
					// left > right
					k--
				case -1:
					// left < right
					j--
				}
			}
			mergedIDs = merged
		}
	}

	// by this point the order of ids in mergedIDs is reversed, e.g. badger uses ASC
	if req.Limit > 0 && len(mergedIDs) > req.Limit {
		mergedIDs = mergedIDs[len(mergedIDs)-req.Limit:]
	}

	// reverse ids
	for left, right := 0, len(mergedIDs)-1; left < right; left, right = left+1, right-1 {
		mergedIDs[left], mergedIDs[right] = mergedIDs[right], mergedIDs[left]
	}

	return mergedIDs
}
