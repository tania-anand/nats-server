// Copyright 2019-2020 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"path"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/minio/highwayhash"
)

type FileStoreConfig struct {
	// Where the parent directory for all storage will be located.
	StoreDir string
	// BlockSize is the file block size. This also represents the maximum overhead size.
	BlockSize uint64
	// CacheExpire is how long with no activity until we expire the cache.
	CacheExpire time.Duration
	// SyncInterval is how often we sync to disk in the background.
	SyncInterval time.Duration
}

// FileStreamInfo allows us to remember created time.
type FileStreamInfo struct {
	Created time.Time
	StreamConfig
}

// File ConsumerInfo is used for creating consumer stores.
type FileConsumerInfo struct {
	Created time.Time
	Name    string
	ConsumerConfig
}

type fileStore struct {
	mu       sync.RWMutex
	state    StreamState
	scb      func(int64, int64, uint64)
	ageChk   *time.Timer
	syncTmr  *time.Timer
	cfg      FileStreamInfo
	fcfg     FileStoreConfig
	lmb      *msgBlock
	blks     []*msgBlock
	hh       hash.Hash64
	fch      chan struct{}
	qch      chan struct{}
	cfs      []*consumerFileStore
	closed   bool
	expiring bool
	sips     int
}

// Represents a message store block and its data.
type msgBlock struct {
	mu      sync.RWMutex
	mfn     string
	mfd     *os.File
	ifn     string
	ifd     *os.File
	liwsz   int64
	index   uint64
	bytes   uint64
	msgs    uint64
	first   msgId
	last    msgId
	lwts    int64
	llts    int64
	lrts    int64
	hh      hash.Hash64
	cache   *cache
	cloads  uint64
	cexp    time.Duration
	ctmr    *time.Timer
	loading bool
	dmap    map[uint64]struct{}
	dch     chan struct{}
	qch     chan struct{}
	lchk    [8]byte
}

// Write through caching layer that is also used on loading messages.
type cache struct {
	buf   []byte
	off   int
	wp    int
	idx   []uint32
	lrl   uint32
	fseq  uint64
	flush bool
}

type msgId struct {
	seq uint64
	ts  int64
}

type fileStoredMsg struct {
	subj string
	hdr  []byte
	msg  []byte
	seq  uint64
	ts   int64 // nanoseconds
	mb   *msgBlock
	off  int64 // offset into block file
}

const (
	// Magic is used to identify the file store files.
	magic = uint8(22)
	// Version
	version = uint8(1)
	// hdrLen
	hdrLen = 2
	// This is where we keep the streams.
	streamsDir = "streams"
	// This is where we keep the message store blocks.
	msgDir = "msgs"
	// This is where we temporarily move the messages dir.
	purgeDir = "__msgs__"
	// used to scan blk file names.
	blkScan = "%d.blk"
	// used to scan index file names.
	indexScan = "%d.idx"
	// This is where we keep state on consumers.
	consumerDir = "obs"
	// Index file for a consumer.
	consumerState = "o.dat"
	// This is where we keep state on templates.
	tmplsDir = "templates"
	// Maximum size of a write buffer we may consider for re-use.
	maxBufReuse = 2 * 1024 * 1024
	// default cache buffer expiration
	defaultCacheBufferExpiration = 5 * time.Second
	// cache idx expiration
	defaultCacheIdxExpiration = 5 * time.Minute
	// default sync interval
	defaultSyncInterval = 10 * time.Second
	// coalesceMinimum
	coalesceMinimum = 4 * 1024
	// maxFlushWait is maximum we will wait to gather messages to flush.
	maxFlushWait = 8 * time.Millisecond
	// Metafiles for streams and consumers.
	JetStreamMetaFile    = "meta.inf"
	JetStreamMetaFileSum = "meta.sum"

	// Default stream block size.
	defaultStreamBlockSize = 64 * 1024 * 1024 // 64MB
	// Default for workqueue or interest based.
	defaultOtherBlockSize = 32 * 1024 * 1024 // 32MB
	// max block size for now.
	maxBlockSize = 2 * defaultStreamBlockSize
	// FileStoreMinBlkSize is minimum size we will do for a blk size.
	FileStoreMinBlkSize = 32 * 1000 // 32kib
	// FileStoreMaxBlkSize is maximum size we will do for a blk size.
	FileStoreMaxBlkSize = maxBlockSize
)

func newFileStore(fcfg FileStoreConfig, cfg StreamConfig) (*fileStore, error) {
	return newFileStoreWithCreated(fcfg, cfg, time.Now())
}

func newFileStoreWithCreated(fcfg FileStoreConfig, cfg StreamConfig, created time.Time) (*fileStore, error) {
	if cfg.Name == "" {
		return nil, fmt.Errorf("name required")
	}
	if cfg.Storage != FileStorage {
		return nil, fmt.Errorf("fileStore requires file storage type in config")
	}
	// Default values.
	if fcfg.BlockSize == 0 {
		fcfg.BlockSize = dynBlkSize(cfg.Retention, cfg.MaxBytes)
	}
	if fcfg.BlockSize > maxBlockSize {
		return nil, fmt.Errorf("filestore max block size is %s", FriendlyBytes(maxBlockSize))
	}
	if fcfg.CacheExpire == 0 {
		fcfg.CacheExpire = defaultCacheBufferExpiration
	}
	if fcfg.SyncInterval == 0 {
		fcfg.SyncInterval = defaultSyncInterval
	}

	// Check the directory
	if stat, err := os.Stat(fcfg.StoreDir); os.IsNotExist(err) {
		if err := os.MkdirAll(fcfg.StoreDir, 0755); err != nil {
			return nil, fmt.Errorf("could not create storage directory - %v", err)
		}
	} else if stat == nil || !stat.IsDir() {
		return nil, fmt.Errorf("store directory is not a directory")
	}
	tmpfile, err := ioutil.TempFile(fcfg.StoreDir, "_test_")
	if err != nil {
		return nil, fmt.Errorf("storage directory is not writable")
	}
	os.Remove(tmpfile.Name())

	fs := &fileStore{
		fcfg: fcfg,
		cfg:  FileStreamInfo{Created: created, StreamConfig: cfg},
		fch:  make(chan struct{}),
		qch:  make(chan struct{}),
	}

	// Check if this is a new setup.
	mdir := path.Join(fcfg.StoreDir, msgDir)
	odir := path.Join(fcfg.StoreDir, consumerDir)
	if err := os.MkdirAll(mdir, 0755); err != nil {
		return nil, fmt.Errorf("could not create message storage directory - %v", err)
	}
	if err := os.MkdirAll(odir, 0755); err != nil {
		return nil, fmt.Errorf("could not create message storage directory - %v", err)
	}

	// Create highway hash for message blocks. Use sha256 of directory as key.
	key := sha256.Sum256([]byte(cfg.Name))
	fs.hh, err = highwayhash.New64(key[:])
	if err != nil {
		return nil, fmt.Errorf("could not create hash: %v", err)
	}

	// Recover our state.
	if err := fs.recoverMsgs(); err != nil {
		return nil, err
	}

	// Write our meta data iff does not exist.
	meta := path.Join(fcfg.StoreDir, JetStreamMetaFile)
	if _, err := os.Stat(meta); err != nil && os.IsNotExist(err) {
		if err := fs.writeStreamMeta(); err != nil {
			return nil, err
		}
	}

	go fs.flushLoop(fs.fch, fs.qch)

	fs.syncTmr = time.AfterFunc(fs.fcfg.SyncInterval, fs.syncBlocks)

	return fs, nil
}

func (fs *fileStore) UpdateConfig(cfg *StreamConfig) error {
	if fs.isClosed() {
		return ErrStoreClosed
	}

	if cfg.Name == "" {
		return fmt.Errorf("name required")
	}
	if cfg.Storage != FileStorage {
		return fmt.Errorf("fileStore requires file storage type in config")
	}

	fs.mu.Lock()
	new_cfg := FileStreamInfo{Created: fs.cfg.Created, StreamConfig: *cfg}
	old_cfg := fs.cfg
	fs.cfg = new_cfg
	if err := fs.writeStreamMeta(); err != nil {
		fs.cfg = old_cfg
		fs.mu.Unlock()
		return err
	}
	// Limits checks and enforcement.
	fs.enforceMsgLimit()
	fs.enforceBytesLimit()
	// Do age timers.
	if fs.ageChk == nil && fs.cfg.MaxAge != 0 {
		fs.startAgeChk()
	}
	if fs.ageChk != nil && fs.cfg.MaxAge == 0 {
		fs.ageChk.Stop()
		fs.ageChk = nil
	}
	fs.mu.Unlock()

	if cfg.MaxAge != 0 {
		fs.expireMsgs()
	}
	return nil
}

func dynBlkSize(retention RetentionPolicy, maxBytes int64) uint64 {
	if maxBytes > 0 {
		blkSize := (maxBytes / 4) + 1 // (25% overhead)
		// Round up to nearest 100
		if m := blkSize % 100; m != 0 {
			blkSize += 100 - m
		}
		if blkSize < FileStoreMinBlkSize {
			blkSize = FileStoreMinBlkSize
		}
		if blkSize > FileStoreMaxBlkSize {
			blkSize = FileStoreMaxBlkSize
		}
		return uint64(blkSize)
	}

	if retention == LimitsPolicy {
		// TODO(dlc) - Make the blocksize relative to this if set.
		return defaultStreamBlockSize
	} else {
		// TODO(dlc) - Make the blocksize relative to this if set.
		return defaultOtherBlockSize
	}
}

// Write out meta and the checksum.
// Lock should be held.
func (fs *fileStore) writeStreamMeta() error {
	meta := path.Join(fs.fcfg.StoreDir, JetStreamMetaFile)
	if _, err := os.Stat(meta); err != nil && !os.IsNotExist(err) {
		return err
	}
	b, err := json.MarshalIndent(fs.cfg, _EMPTY_, "  ")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(meta, b, 0644); err != nil {
		return err
	}
	fs.hh.Reset()
	fs.hh.Write(b)
	checksum := hex.EncodeToString(fs.hh.Sum(nil))
	sum := path.Join(fs.fcfg.StoreDir, JetStreamMetaFileSum)
	if err := ioutil.WriteFile(sum, []byte(checksum), 0644); err != nil {
		return err
	}
	return nil
}

const msgHdrSize = 22
const checksumSize = 8

// This is the max room needed for index header.
const indexHdrSize = 7*binary.MaxVarintLen64 + hdrLen + checksumSize

func (fs *fileStore) recoverMsgBlock(fi os.FileInfo, index uint64) *msgBlock {
	var le = binary.LittleEndian

	mb := &msgBlock{index: index, cexp: fs.fcfg.CacheExpire}

	mdir := path.Join(fs.fcfg.StoreDir, msgDir)
	mb.mfn = path.Join(mdir, fi.Name())
	mb.ifn = path.Join(mdir, fmt.Sprintf(indexScan, index))

	if mb.hh == nil {
		key := sha256.Sum256(fs.hashKeyForBlock(index))
		mb.hh, _ = highwayhash.New64(key[:])
	}

	// Open up the message file, but we will try to recover from the index file.
	// We will check that the last checksums match.
	file, err := os.Open(mb.mfn)
	if err != nil {
		return nil
	}
	defer file.Close()

	// Read our index file. Use this as source of truth if possible.
	if err := mb.readIndexInfo(); err == nil {
		// Quick sanity check here.
		// Note this only checks that the message blk file is not newer then this file.
		var lchk [8]byte
		file.ReadAt(lchk[:], fi.Size()-8)
		if bytes.Equal(lchk[:], mb.lchk[:]) {
			fs.blks = append(fs.blks, mb)
			return mb
		}
		// Fall back on the data file itself. We will keep the delete map if present.
		mb.msgs = 0
		mb.bytes = 0
		mb.first.seq = 0
	}

	addToDmap := func(seq uint64) {
		if seq == 0 {
			return
		}
		if mb.dmap == nil {
			mb.dmap = make(map[uint64]struct{})
		}
		mb.dmap[seq] = struct{}{}
	}

	// Use data file itself to rebuild.
	var hdr [msgHdrSize]byte
	var offset int64

	for {
		if _, err := file.ReadAt(hdr[:], offset); err != nil {
			// FIXME(dlc) - If this is not EOF we probably should try to fix.
			break
		}
		rl := le.Uint32(hdr[0:])
		seq := le.Uint64(hdr[4:])

		// Can't recover with zero record length.
		if rl == 0 {
			return nil
		}

		// This is an old erased message, or a new one that we can track.
		if seq == 0 || seq&ebit != 0 {
			addToDmap(seq &^ ebit)
			offset += int64(rl)
			continue
		}
		ts := int64(le.Uint64(hdr[12:]))
		if mb.first.seq == 0 {
			mb.first.seq = seq
			mb.first.ts = ts
		}
		mb.last.seq = seq
		mb.last.ts = ts

		mb.msgs++
		mb.bytes += uint64(rl)
		offset += int64(rl)
	}
	// Rewrite this to make sure we are sync'd.
	mb.writeIndexInfo()
	fs.blks = append(fs.blks, mb)
	fs.lmb = mb
	return mb
}

func (fs *fileStore) recoverMsgs() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	// Check for any left over purged messages.
	pdir := path.Join(fs.fcfg.StoreDir, purgeDir)
	if _, err := os.Stat(pdir); err == nil {
		os.RemoveAll(pdir)
	}

	mdir := path.Join(fs.fcfg.StoreDir, msgDir)
	fis, err := ioutil.ReadDir(mdir)
	if err != nil {
		return errNotReadable
	}

	// Recover all of the msg blocks.
	// These can come in a random order, so account for that.
	for _, fi := range fis {
		var index uint64
		if n, err := fmt.Sscanf(fi.Name(), blkScan, &index); err == nil && n == 1 {
			if mb := fs.recoverMsgBlock(fi, index); mb != nil {
				if fs.state.FirstSeq == 0 || mb.first.seq < fs.state.FirstSeq {
					fs.state.FirstSeq = mb.first.seq
					fs.state.FirstTime = time.Unix(0, mb.first.ts).UTC()
				}
				if mb.last.seq > fs.state.LastSeq {
					fs.state.LastSeq = mb.last.seq
					fs.state.LastTime = time.Unix(0, mb.last.ts).UTC()
				}
				fs.state.Msgs += mb.msgs
				fs.state.Bytes += mb.bytes
			}
		}
	}

	// Now make sure to sort blks for efficient lookup later with selectMsgBlock().
	if len(fs.blks) > 0 {
		sort.Slice(fs.blks, func(i, j int) bool { return fs.blks[i].index < fs.blks[j].index })
		fs.lmb = fs.blks[len(fs.blks)-1]
		err = fs.enableLastMsgBlockForWriting()
	} else {
		_, err = fs.newMsgBlockForWrite()
	}

	if err != nil {
		return err
	}

	// Limits checks and enforcement.
	fs.enforceMsgLimit()
	fs.enforceBytesLimit()

	// Do age checks too, make sure to call in place.
	if fs.cfg.MaxAge != 0 && fs.state.Msgs > 0 {
		fs.startAgeChk()
		fs.expireMsgsLocked()
	}
	return nil
}

// GetSeqFromTime looks for the first sequence number that has
// the message with >= timestamp.
// FIXME(dlc) - inefficient, and dumb really. Make this better.
func (fs *fileStore) GetSeqFromTime(t time.Time) uint64 {
	fs.mu.RLock()
	lastSeq := fs.state.LastSeq
	closed := fs.closed
	fs.mu.RUnlock()

	if closed {
		return 0
	}

	mb := fs.selectMsgBlockForStart(t)
	if mb == nil {
		return lastSeq + 1
	}

	mb.mu.RLock()
	fseq := mb.first.seq
	lseq := mb.last.seq
	mb.mu.RUnlock()

	// Linear search, hence the dumb part..
	ts := t.UnixNano()
	for seq := fseq; seq <= lseq; seq++ {
		sm, _ := mb.fetchMsg(seq)
		if sm != nil && sm.ts >= ts {
			return sm.seq
		}
	}
	return 0
}

// RegisterStorageUpdates registers a callback for updates to storage changes.
// It will present number of messages and bytes as a signed integer and an
// optional sequence number of the message if a single.
func (fs *fileStore) RegisterStorageUpdates(cb func(int64, int64, uint64)) {
	fs.mu.Lock()
	fs.scb = cb
	bsz := fs.state.Bytes
	fs.mu.Unlock()
	if cb != nil && bsz > 0 {
		cb(0, int64(bsz), 0)
	}
}

// Helper to get hash key for specific message block.
// Lock should be held
func (fs *fileStore) hashKeyForBlock(index uint64) []byte {
	return []byte(fmt.Sprintf("%s-%d", fs.cfg.Name, index))
}

// This rolls to a new append msg block.
// Lock should be held.
func (fs *fileStore) newMsgBlockForWrite() (*msgBlock, error) {
	index := uint64(1)

	if fs.lmb != nil {
		index = fs.lmb.index + 1
		fs.flushPendingWrites()
		fs.closeLastMsgBlock(false)
	}

	mb := &msgBlock{index: index, cexp: fs.fcfg.CacheExpire}
	fs.blks = append(fs.blks, mb)
	fs.lmb = mb

	mdir := path.Join(fs.fcfg.StoreDir, msgDir)
	mb.mfn = path.Join(mdir, fmt.Sprintf(blkScan, mb.index))
	mfd, err := os.OpenFile(mb.mfn, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("Error creating msg block file [%q]: %v", mb.mfn, err)
	}
	mb.mfd = mfd

	mb.ifn = path.Join(mdir, fmt.Sprintf(indexScan, mb.index))
	ifd, err := os.OpenFile(mb.ifn, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, fmt.Errorf("Error creating msg index file [%q]: %v", mb.mfn, err)
	}
	mb.ifd = ifd

	// Now do local hash.
	key := sha256.Sum256(fs.hashKeyForBlock(index))
	mb.hh, err = highwayhash.New64(key[:])
	if err != nil {
		return nil, fmt.Errorf("could not create hash: %v", err)
	}

	return mb, nil
}

// Make sure we can write to the last message block.
// Lock should be held.
func (fs *fileStore) enableLastMsgBlockForWriting() error {
	mb := fs.lmb
	if mb == nil {
		return fmt.Errorf("no last message block assigned, can not enable for writing")
	}
	if mb.mfd != nil {
		return nil
	}
	mfd, err := os.OpenFile(mb.mfn, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return fmt.Errorf("error opening msg block file [%q]: %v", mb.mfn, err)
	}
	mb.mfd = mfd
	return nil
}

// Store stores a message.
func (fs *fileStore) StoreMsg(subj string, hdr, msg []byte) (uint64, int64, error) {
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		return 0, 0, ErrStoreClosed
	}

	// Check if we are discarding new messages when we reach the limit.
	if fs.cfg.Discard == DiscardNew {
		if fs.cfg.MaxMsgs > 0 && fs.state.Msgs >= uint64(fs.cfg.MaxMsgs) {
			fs.mu.Unlock()
			return 0, 0, ErrMaxMsgs
		}
		if fs.cfg.MaxBytes > 0 && fs.state.Bytes+uint64(len(msg)+len(hdr)) >= uint64(fs.cfg.MaxBytes) {
			fs.mu.Unlock()
			return 0, 0, ErrMaxBytes
		}
	}

	seq := fs.state.LastSeq + 1

	n, ts, err := fs.writeMsgRecord(seq, subj, hdr, msg)
	if err != nil {
		fs.mu.Unlock()
		return 0, 0, err
	}
	fs.kickFlusher()

	if fs.state.Msgs == 0 {
		fs.state.FirstSeq = seq
		fs.state.FirstTime = time.Unix(0, ts).UTC()
	}

	fs.state.Msgs++
	fs.state.Bytes += n
	fs.state.LastSeq = seq
	fs.state.LastTime = time.Unix(0, ts).UTC()

	// Limits checks and enforcement.
	// If they do any deletions they will update the
	// byte count on their own, so no need to compensate.
	fs.enforceMsgLimit()
	fs.enforceBytesLimit()

	// Check if we have and need the age expiration timer running.
	if fs.ageChk == nil && fs.cfg.MaxAge != 0 {
		fs.startAgeChk()
	}

	cb := fs.scb
	fs.mu.Unlock()

	if cb != nil {
		cb(1, int64(n), seq)
	}

	return seq, ts, nil
}

// skipMsg will update message block for a skipped message. If first
// just meta data, but if interior an empty message record with erase bit.
// fs lock will be held.
func (mb *msgBlock) skipMsg(seq uint64, now time.Time) {
	if mb == nil {
		return
	}
	// If we are empty can just do meta.
	if mb.msgs == 0 {
		mb.mu.Lock()
		mb.last.seq = seq
		mb.last.ts = now.UnixNano()
		mb.first.seq = seq + 1
		mb.first.ts = now.UnixNano()
		mb.mu.Unlock()
	} else {
		mb.writeMsgRecord(emptyRecordLen, seq|ebit, _EMPTY_, nil, nil, now.UnixNano())
		mb.mu.Lock()
		if mb.dmap == nil {
			mb.dmap = make(map[uint64]struct{})
		}
		mb.dmap[seq] = struct{}{}
		mb.msgs--
		mb.bytes -= emptyRecordLen
		mb.mu.Unlock()
	}
	mb.kickWriteFlusher()
}

// SkipMsg will use the next sequence number but not store anything.
func (fs *fileStore) SkipMsg() uint64 {
	// Grab time.
	now := time.Now().UTC()
	fs.mu.Lock()
	seq := fs.state.LastSeq + 1
	fs.state.LastSeq = seq
	fs.state.LastTime = now
	if fs.state.Msgs == 0 {
		fs.state.FirstSeq = seq
		fs.state.FirstTime = now
	}
	if seq == fs.state.FirstSeq {
		fs.state.FirstSeq = seq + 1
		fs.state.FirstTime = now
	}
	fs.lmb.skipMsg(seq, now)
	fs.kickFlusher()
	fs.mu.Unlock()
	return seq
}

// Will check the msg limit and drop firstSeq msg if needed.
// Lock should be held.
func (fs *fileStore) enforceMsgLimit() {
	if fs.cfg.MaxMsgs <= 0 || fs.state.Msgs <= uint64(fs.cfg.MaxMsgs) {
		return
	}
	for nmsgs := fs.state.Msgs; nmsgs > uint64(fs.cfg.MaxMsgs); nmsgs = fs.state.Msgs {
		fs.deleteFirstMsgLocked()
	}
}

// Will check the bytes limit and drop msgs if needed.
// Lock should be held.
func (fs *fileStore) enforceBytesLimit() {
	if fs.cfg.MaxBytes <= 0 || fs.state.Bytes <= uint64(fs.cfg.MaxBytes) {
		return
	}
	for bs := fs.state.Bytes; bs > uint64(fs.cfg.MaxBytes); bs = fs.state.Bytes {
		fs.deleteFirstMsgLocked()
	}
}

// Lock should be held but will be released during actual remove.
func (fs *fileStore) deleteFirstMsgLocked() (bool, error) {
	fs.mu.Unlock()
	defer fs.mu.Lock()
	return fs.removeMsg(fs.state.FirstSeq, false)
}

// Lock should NOT be held.
func (fs *fileStore) deleteFirstMsg() (bool, error) {
	fs.mu.RLock()
	seq := fs.state.FirstSeq
	fs.mu.RUnlock()
	return fs.removeMsg(seq, false)
}

// RemoveMsg will remove the message from this store.
// Will return the number of bytes removed.
func (fs *fileStore) RemoveMsg(seq uint64) (bool, error) {
	return fs.removeMsg(seq, false)
}

func (fs *fileStore) EraseMsg(seq uint64) (bool, error) {
	return fs.removeMsg(seq, true)
}

// Remove a message, optionally rewriting the mb file.
func (fs *fileStore) removeMsg(seq uint64, secure bool) (bool, error) {
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		return false, ErrStoreClosed
	}
	if fs.sips > 0 {
		fs.mu.Unlock()
		return false, ErrStoreSnapshotInProgress
	}

	mb := fs.selectMsgBlock(seq)
	if mb == nil {
		var err = ErrStoreEOF
		if seq <= fs.state.LastSeq {
			err = ErrStoreMsgNotFound
		}
		fs.mu.Unlock()
		return false, err
	}

	mb.mu.Lock()

	// Check cache. This will be very rare, will hold lock on this one.
	if mb.cache == nil || mb.cache.idx == nil {
		mb.mu.Unlock()
		if err := mb.loadMsgs(); err != nil {
			fs.mu.Unlock()
			return false, err
		}
		mb.mu.Lock()
	}

	// See if the sequence numbers is still relevant. Check first and cache first.
	if seq < mb.first.seq || seq < mb.cache.fseq || (seq-mb.cache.fseq) >= uint64(len(mb.cache.idx)) {
		mb.mu.Unlock()
		fs.mu.Unlock()
		return false, nil
	}

	// Now check dmap if it is there.
	if mb.dmap != nil {
		if _, ok := mb.dmap[seq]; ok {
			mb.mu.Unlock()
			fs.mu.Unlock()
			return false, nil
		}
	}

	// Grab record length from idx.
	slot := seq - mb.cache.fseq
	ri, rl, _, _ := mb.slotInfo(int(slot))
	msz := uint64(rl)

	// Global stats
	fs.state.Msgs--
	fs.state.Bytes -= msz

	// Now local mb updates.
	mb.msgs--
	mb.bytes -= msz

	// Set cache timestamp for last remove.
	mb.lrts = time.Now().UnixNano()

	var shouldWriteIndex bool
	var firstSeqNeedsUpdate bool

	if secure {
		mb.eraseMsg(seq, int(ri), int(rl))
	}

	// Optimize for FIFO case.
	if seq == mb.first.seq {
		mb.selectNextFirst()
		if mb.isEmpty() {
			fs.removeMsgBlock(mb)
			firstSeqNeedsUpdate = seq == fs.state.FirstSeq
		} else {
			shouldWriteIndex = true
			if seq == fs.state.FirstSeq {
				fs.state.FirstSeq = mb.first.seq // new one.
				fs.state.FirstTime = time.Unix(0, mb.first.ts).UTC()
			}
		}
	} else {
		// Out of order delete.
		if mb.dmap == nil {
			mb.dmap = make(map[uint64]struct{})
		}
		mb.dmap[seq] = struct{}{}
		shouldWriteIndex = true
	}

	var dch chan struct{}

	if shouldWriteIndex {
		if mb.dch == nil {
			// Spin up the write flusher.
			mb.qch = make(chan struct{})
			mb.dch = make(chan struct{})
			go fs.flushWriteIndexLoop(mb, mb.dch, mb.qch)
			// Do a blocking kick here to make sure loop is running.
			mb.dch <- struct{}{}
		} else {
			dch = mb.dch
		}
	}
	mb.mu.Unlock()

	// Kick outside of lock.
	if shouldWriteIndex && dch != nil {
		select {
		case dch <- struct{}{}:
		default:
		}
	}

	// If we emptied the current message block and the seq was state.First.Seq
	// then we need to jump message blocks.
	if firstSeqNeedsUpdate {
		fs.selectNextFirst()
	}
	fs.mu.Unlock()

	if fs.scb != nil {
		delta := int64(msz)
		fs.scb(-delta)
	}

	return true, nil
}

// Grab info from a slot.
// Lock should be held.
func (mb *msgBlock) slotInfo(slot int) (uint32, uint32, bool, error) {
	if mb.cache == nil || mb.cache.idx == nil {
		return 0, 0, false, errPartialCache
	}
	bi := mb.cache.idx[slot]
	ri := (bi &^ hbit)
	hashChecked := (bi & hbit) != 0
	// Determine record length
	var rl uint32
	if len(mb.cache.idx) > slot+1 {
		ni := mb.cache.idx[slot+1] &^ hbit
		rl = ni - ri
	} else {
		rl = mb.cache.lrl
	}
	if rl < msgHdrSize {
		return 0, 0, false, errBadMsg
	}
	return uint32(ri), rl, hashChecked, nil
}

func (fs *fileStore) isClosed() bool {
	fs.mu.RLock()
	closed := fs.closed
	fs.mu.RUnlock()
	return closed
}

// Loop on requests to write out our index file. This is used when calling
// remove for a message. Updates to the last.seq etc are handled by main
// flush loop when storing messages.
func (fs *fileStore) flushWriteIndexLoop(mb *msgBlock, dch, qch chan struct{}) {
	for {
		select {
		case <-dch:
			mb.writeIndexInfo()
		case <-qch:
			return
		}
	}
}

// Lock should be held.
func (mb *msgBlock) eraseMsg(seq uint64, ri, rl int) error {
	var le = binary.LittleEndian
	var hdr [msgHdrSize]byte

	le.PutUint32(hdr[0:], uint32(rl))
	le.PutUint64(hdr[4:], seq|ebit)
	le.PutUint64(hdr[12:], 0)
	le.PutUint16(hdr[20:], 0)

	// Randomize record
	data := make([]byte, rl-emptyRecordLen)
	rand.Read(data)

	// Now write to underlying buffer.
	var b bytes.Buffer
	b.Write(hdr[:])
	b.Write(data)

	// Calculate hash.
	mb.hh.Reset()
	mb.hh.Write(hdr[4:20])
	mb.hh.Write(data)
	checksum := mb.hh.Sum(nil)
	// Write to msg record.
	b.Write(checksum)

	// Update both cache and disk.
	nbytes := b.Bytes()

	// Cache
	if ri >= mb.cache.off {
		li := ri - mb.cache.off
		buf := mb.cache.buf[li : li+rl]
		copy(buf, nbytes)
	}
	// Disk
	if mb.cache.off+mb.cache.wp > ri {
		mfd, err := os.OpenFile(mb.mfn, os.O_RDWR, 0644)
		if err != nil {
			return err
		}
		defer mfd.Close()
		if _, err = mfd.WriteAt(nbytes, int64(ri)); err == nil {
			mfd.Sync()
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (mb *msgBlock) kickWriteFlusher() {
	mb.mu.RLock()
	select {
	case mb.dch <- struct{}{}:
	default:
	}
	mb.mu.RUnlock()
}

// Lock should be held.
func (mb *msgBlock) isEmpty() bool {
	return mb.first.seq > mb.last.seq
}

// Lock should be held.
func (mb *msgBlock) selectNextFirst() {
	var seq uint64
	for seq = mb.first.seq + 1; seq <= mb.last.seq; seq++ {
		if _, ok := mb.dmap[seq]; ok {
			// We will move past this so we can delete the entry.
			delete(mb.dmap, seq)
		} else {
			break
		}
	}
	// Set new first sequence.
	mb.first.seq = seq
	// Check if we are empty..
	if mb.isEmpty() {
		mb.first.ts = 0
		return
	}

	// Need to get the timestamp.
	// We will try the cache direct and fallback if needed.
	sm, _ := mb.cacheLookupLocked(seq)
	if sm == nil {
		// Slow path, need to unlock.
		mb.mu.Unlock()
		sm, _ = mb.fetchMsg(seq)
		mb.mu.Lock()
	}
	if sm != nil {
		mb.first.ts = sm.ts
	} else {
		mb.first.ts = 0
	}
}

// Select the next FirstSeq
func (fs *fileStore) selectNextFirst() {
	if len(fs.blks) > 0 {
		mb := fs.blks[0]
		mb.mu.RLock()
		fs.state.FirstSeq = mb.first.seq
		fs.state.FirstTime = time.Unix(0, mb.first.ts).UTC()
		mb.mu.RUnlock()
	} else {
		// Could not find anything, so treat like purge
		fs.state.FirstSeq = fs.state.LastSeq + 1
		fs.state.FirstTime = time.Time{}
	}
}

// Lock should be held.
func (mb *msgBlock) resetCacheExpireTimer(td time.Duration) {
	if td == 0 {
		td = mb.cexp
	}
	if mb.ctmr == nil {
		mb.ctmr = time.AfterFunc(td, mb.expireCache)
	} else {
		mb.ctmr.Reset(td)
	}
}

// Lock should be held.
func (mb *msgBlock) startCacheExpireTimer() {
	mb.resetCacheExpireTimer(0)
}

// Lock should be held.
func (mb *msgBlock) clearCache() {
	if mb.ctmr != nil {
		mb.ctmr.Stop()
		mb.ctmr = nil
	}
	mb.cache = nil
}

// Called to possibly expire a message block cache.
func (mb *msgBlock) expireCache() {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	if mb.cache == nil {
		mb.ctmr = nil
		return
	}

	// Can't expire if we are flushing or still have pending.
	if mb.cache.flush || (len(mb.cache.buf)-mb.cache.wp > 0) {
		mb.resetCacheExpireTimer(mb.cexp)
		return
	}

	// Grab timestamp to compare.
	tns := time.Now().UnixNano()

	// For the core buffer of messages, we care about reads and writes, but not removes.
	bufts := mb.llts
	if mb.lwts > bufts {
		bufts = mb.lwts
	}

	// Check for the underlying buffer first.
	if tns-bufts <= int64(mb.cexp) {
		mb.resetCacheExpireTimer(mb.cexp - time.Duration(tns-bufts))
		return
	}

	// If we are here we will at least expire the core msg buffer.
	mb.cache.buf = nil
	mb.cache.wp = 0

	// The idx is used in removes, and will have a longer timeframe.
	// See if we should also remove the idx.
	if tns-mb.lrts > int64(defaultCacheIdxExpiration) {
		mb.clearCache()
	} else {
		mb.resetCacheExpireTimer(mb.cexp)
	}
}

func (fs *fileStore) startAgeChk() {
	if fs.ageChk == nil && fs.cfg.MaxAge != 0 {
		fs.ageChk = time.AfterFunc(fs.cfg.MaxAge, fs.expireMsgs)
	}
}

// Lock should be held.
func (fs *fileStore) expireMsgsLocked() {
	fs.mu.Unlock()
	fs.expireMsgs()
	fs.mu.Lock()
}

// Will expire msgs that are too old.
func (fs *fileStore) expireMsgs() {
	// Make sure this is only running one at a time.
	fs.mu.Lock()
	if fs.expiring {
		fs.mu.Unlock()
		return
	}
	fs.expiring = true
	fs.mu.Unlock()

	defer func() {
		fs.mu.Lock()
		fs.expiring = false
		fs.mu.Unlock()
	}()

	now := time.Now().UnixNano()
	minAge := now - int64(fs.cfg.MaxAge)

	for {
		sm, _ := fs.msgForSeq(0)
		if sm != nil && sm.ts <= minAge {
			fs.deleteFirstMsg()
		} else {
			fs.mu.Lock()
			if sm == nil {
				if fs.ageChk != nil {
					fs.ageChk.Stop()
					fs.ageChk = nil
				}
			} else {
				fireIn := time.Duration(sm.ts-now) + fs.cfg.MaxAge
				if fs.ageChk != nil {
					fs.ageChk.Reset(fireIn)
				} else {
					fs.ageChk = time.AfterFunc(fireIn, fs.expireMsgs)
				}
			}
			fs.mu.Unlock()
			return
		}
	}
}

// Check all the checksums for a message block.
func checkMsgBlockFile(fp *os.File, hh hash.Hash) []uint64 {
	var le = binary.LittleEndian
	var hdr [msgHdrSize]byte
	var bad []uint64

	r := bufio.NewReaderSize(fp, 64*1024*1024)

	for {
		if _, err := io.ReadFull(r, hdr[0:]); err != nil {
			break
		}
		rl := le.Uint32(hdr[0:])
		seq := le.Uint64(hdr[4:])
		slen := le.Uint16(hdr[20:])
		dlen := int(rl) - msgHdrSize
		if dlen < 0 || int(slen) > dlen || dlen > int(rl) {
			bad = append(bad, seq)
			break
		}
		data := make([]byte, dlen)
		if _, err := io.ReadFull(r, data); err != nil {
			bad = append(bad, seq)
			break
		}
		hh.Reset()
		hh.Write(hdr[4:20])
		hh.Write(data[:slen])
		hh.Write(data[slen : dlen-8])
		checksum := hh.Sum(nil)
		if !bytes.Equal(checksum, data[len(data)-8:]) {
			bad = append(bad, seq)
		}
	}
	return bad
}

// This will check all the checksums on messages and report back any sequence numbers with errors.
func (fs *fileStore) checkMsgs() []uint64 {
	fs.flushPendingWritesUnlocked()

	mdir := path.Join(fs.fcfg.StoreDir, msgDir)
	fis, err := ioutil.ReadDir(mdir)
	if err != nil {
		return nil
	}

	var bad []uint64

	// Check all of the msg blocks.
	for _, fi := range fis {
		var index uint64
		if n, err := fmt.Sscanf(fi.Name(), blkScan, &index); err == nil && n == 1 {
			if fp, err := os.Open(path.Join(mdir, fi.Name())); err != nil {
				continue
			} else {
				key := sha256.Sum256(fs.hashKeyForBlock(index))
				hh, _ := highwayhash.New64(key[:])
				bad = append(bad, checkMsgBlockFile(fp, hh)...)
				fp.Close()
			}
		}
	}
	return bad
}

// This will kick out our flush routine if its waiting.
func (fs *fileStore) kickFlusher() {
	select {
	case fs.fch <- struct{}{}:
	default:
	}
}

func (fs *fileStore) lastMsgBlock() *msgBlock {
	fs.mu.RLock()
	mb := fs.lmb
	fs.mu.RUnlock()
	return mb
}

// Looks at active write message block (lmb).
func (fs *fileStore) pendingWriteSize() int {
	return fs.lastMsgBlock().numWriteBytesPending()
}

func (fs *fileStore) flushLoop(fch, qch chan struct{}) {
	for {
		select {
		case <-fch:
			waiting := fs.pendingWriteSize()
			if waiting == 0 {
				continue
			}
			ts := 1 * time.Millisecond
			var waited time.Duration

			for waiting < coalesceMinimum {
				time.Sleep(ts)
				newWaiting := fs.pendingWriteSize()
				if newWaiting <= waiting {
					break
				}
				if waited = waited + ts; waited > maxFlushWait {
					break
				}
				select {
				case <-qch:
					return
				default:
				}
				waiting = newWaiting
				ts *= 2
			}
			fs.flushPendingWritesUnlocked()
		case <-qch:
			return
		}
	}
}

// Will write the message record to the underlying message block.
// filestore lock will be held.
func (mb *msgBlock) writeMsgRecord(rl, seq uint64, subj string, mhdr, msg []byte, ts int64) {
	mb.mu.Lock()
	defer mb.mu.Unlock()

	// Make sure we have a cache setup.
	if mb.cache == nil {
		mb.cache = &cache{}
		mb.startCacheExpireTimer()
	}

	// Indexing
	index := len(mb.cache.buf)
	if mb.cache.off > 0 {
		index += mb.cache.off
	}

	// Formats
	// Format with no header
	// total_len(4) sequence(8) timestamp(8) subj_len(2) subj msg hash(8)
	// With headers, high bit on total length will be set.
	// total_len(4) sequence(8) timestamp(8) subj_len(2) subj hdr_len(4) hdr msg hash(8)

	// First write header, etc.
	var le = binary.LittleEndian
	var hdr [msgHdrSize]byte

	l := uint32(rl)
	hasHeaders := len(mhdr) > 0
	if hasHeaders {
		l |= hbit
	}

	le.PutUint32(hdr[0:], l)
	le.PutUint64(hdr[4:], seq)
	le.PutUint64(hdr[12:], uint64(ts))
	le.PutUint16(hdr[20:], uint16(len(subj)))

	// Now write to underlying buffer.
	mb.cache.buf = append(mb.cache.buf, hdr[:]...)
	mb.cache.buf = append(mb.cache.buf, subj...)

	if hasHeaders {
		var hlen [4]byte
		le.PutUint32(hlen[0:], uint32(len(mhdr)))
		mb.cache.buf = append(mb.cache.buf, hlen[:]...)
		mb.cache.buf = append(mb.cache.buf, mhdr...)
	}
	mb.cache.buf = append(mb.cache.buf, msg...)

	// Calculate hash.
	mb.hh.Reset()
	mb.hh.Write(hdr[4:20])
	mb.hh.Write([]byte(subj))
	if hasHeaders {
		mb.hh.Write(mhdr)
	}
	mb.hh.Write(msg)
	checksum := mb.hh.Sum(nil)
	// Grab last checksum
	copy(mb.lchk[0:], checksum)

	// Update writethrough cache.
	// Write to msg record.
	mb.cache.buf = append(mb.cache.buf, checksum...)
	// Write index
	mb.cache.idx = append(mb.cache.idx, uint32(index)|hbit)
	mb.cache.lrl = uint32(rl)
	if mb.cache.fseq == 0 {
		mb.cache.fseq = seq
	}

	// Set cache timestamp for last store.
	mb.lwts = ts

	// Accounting
	mb.updateAccounting(seq, ts, rl)
}

// How many bytes pending to be written for this message block.
func (mb *msgBlock) numWriteBytesPending() int {
	if mb == nil {
		return 0
	}
	var pending int
	mb.mu.RLock()
	if mb.mfd != nil && mb.cache != nil {
		pending = len(mb.cache.buf) - mb.cache.wp
	}
	mb.mu.RUnlock()

	return pending
}

func (mb *msgBlock) clearFlushing() {
	mb.mu.Lock()
	if mb.cache != nil {
		mb.cache.flush = false
	}
	mb.mu.Unlock()
}

// writeBytesPending returns the buffer to be used for writing to the underlying file.
// This marks we are in flush and will return nil if asked again until cleared.
func (mb *msgBlock) writeBytesPending() ([]byte, error) {
	if mb == nil || mb.mfd == nil {
		return nil, errNoPending
	}
	mb.mu.Lock()
	if mb.cache == nil {
		mb.mu.Unlock()
		return nil, errNoCache
	}
	if mb.cache.flush {
		mb.mu.Unlock()
		return nil, errFlushRunning
	}
	buf := mb.cache.buf[mb.cache.wp:]
	if len(buf) == 0 {
		mb.mu.Unlock()
		return nil, errNoPending
	}
	mb.cache.flush = true
	mb.mu.Unlock()
	return buf, nil
}

// Return the number of bytes in this message block.
func (mb *msgBlock) numBytes() uint64 {
	mb.mu.RLock()
	nb := mb.bytes
	mb.mu.RUnlock()
	return nb
}

// Update accounting on a write msg.
// Lock should be held.
func (mb *msgBlock) updateAccounting(seq uint64, ts int64, rl uint64) {
	if mb.first.seq == 0 {
		mb.first.seq = seq
		mb.first.ts = ts
	}
	// Need atomics here for selectMsgBlock speed.
	atomic.StoreUint64(&mb.last.seq, seq)
	mb.last.ts = ts
	mb.bytes += rl
	mb.msgs++
}

// Lock should be held.
func (fs *fileStore) writeMsgRecord(seq uint64, subj string, mhdr, msg []byte) (uint64, int64, error) {
	var err error

	// Get size for this message.
	rl := fileStoreMsgSize(subj, mhdr, msg)
	if rl&hbit != 0 {
		return 0, 0, ErrMsgTooLarge
	}
	// Grab our current last message block.
	mb := fs.lmb
	if mb == nil || mb.numBytes()+rl > fs.fcfg.BlockSize {
		if mb, err = fs.newMsgBlockForWrite(); err != nil {
			return 0, 0, err
		}
	}

	// Grab time
	ts := time.Now().UnixNano()

	// Ask msg block to store in write through cache.
	mb.writeMsgRecord(rl, seq, subj, mhdr, msg, ts)

	return rl, ts, nil
}

// Sync msg and index files as needed. This is called from a timer.
func (fs *fileStore) syncBlocks() {
	fs.mu.RLock()
	closed := fs.closed
	blks := fs.blks
	fs.mu.RUnlock()

	if closed {
		return
	}
	for _, mb := range blks {
		mb.mu.RLock()
		if mb.mfd != nil {
			mb.mfd.Sync()
		}
		if mb.ifd != nil {
			mb.ifd.Sync()
			mb.ifd.Truncate(mb.liwsz)
		}
		mb.mu.RUnlock()
	}

	var _cfs [256]*consumerFileStore

	fs.mu.Lock()
	cfs := append(_cfs[:0], fs.cfs...)
	fs.syncTmr = time.AfterFunc(fs.fcfg.SyncInterval, fs.syncBlocks)
	fs.mu.Unlock()

	// Do consumers.
	for _, o := range cfs {
		o.syncStateFile()
	}
}

// Select the message block where this message should be found.
// Return nil if not in the set.
// Read lock should be held.
func (fs *fileStore) selectMsgBlock(seq uint64) *msgBlock {
	// Check for out of range.
	if seq < fs.state.FirstSeq || seq > fs.state.LastSeq {
		return nil
	}

	// blks are sorted in ascending order.
	// TODO(dlc) - Can be smarter here, when lots of blks maybe use binary search.
	// For now this is cache friendly for small to medium numbers of blks.
	for _, mb := range fs.blks {
		if seq <= atomic.LoadUint64(&mb.last.seq) {
			return mb
		}
	}
	return nil
}

// Select the message block where this message should be found.
// Return nil if not in the set.
func (fs *fileStore) selectMsgBlockForStart(minTime time.Time) *msgBlock {
	fs.mu.RLock()
	blks := fs.blks
	lmb := fs.lmb
	fs.mu.RUnlock()

	t := minTime.UnixNano()
	for _, mb := range blks {
		mb.mu.RLock()
		found := t <= mb.last.ts
		mb.mu.RUnlock()

		if found {
			// This detects if what we may be looking for is staged in the write buffer.
			if mb == lmb {
				fs.flushPendingWritesUnlocked()
			}
			return mb
		}
	}
	return nil
}

// Index a raw msg buffer.
// Lock should be held.
func (mb *msgBlock) indexCacheBuf(buf []byte) error {
	var le = binary.LittleEndian

	var fseq uint64
	var idx []uint32
	var index uint32

	if mb.cache == nil {
		// Approximation, may adjust below.
		fseq = mb.first.seq
		idx = make([]uint32, 0, mb.msgs)
		mb.cache = &cache{}
	} else {
		fseq = mb.cache.fseq
		idx = mb.cache.idx
		if len(idx) == 0 {
			idx = make([]uint32, 0, mb.msgs)
		}
		index = uint32(len(mb.cache.buf))
		buf = append(mb.cache.buf, buf...)
	}

	lbuf := uint32(len(buf))

	for index < lbuf {
		hdr := buf[index : index+msgHdrSize]
		rl := le.Uint32(hdr[0:])
		seq := le.Uint64(hdr[4:])
		slen := le.Uint16(hdr[20:])

		// Clear any headers bit that could be set.
		rl &^= hbit

		dlen := int(rl) - msgHdrSize

		// Do some quick sanity checks here.
		if dlen < 0 || int(slen) > dlen || dlen > int(rl) {
			// This means something is off.
			// TODO(dlc) - Add into bad list?
			return errBadMsg
		}
		// Adjust if we guessed wrong.
		if seq != 0 && seq < fseq {
			fseq = seq
		}
		// We defer checksum checks to individual msg cache lookups to amortorize costs and
		// not introduce latency for first message from a newly loaded block.
		idx = append(idx, index)
		mb.cache.lrl = uint32(rl)
		index += mb.cache.lrl
	}
	mb.cache.buf = buf
	mb.cache.idx = idx
	mb.cache.fseq = fseq
	mb.cache.wp += len(buf)

	return nil
}

// flushPendingWrites for this message block.
func (mb *msgBlock) flushPendingWrites() error {
	buf, err := mb.writeBytesPending()
	if err != nil {
		return err
	}
	defer mb.clearFlushing()

	var lbb, tn int
	// Append new data to the message block file.
	for lbb = len(buf); lbb > 0; lbb = len(buf) {
		n, err := mb.mfd.Write(buf)
		if err != nil {
			// FIXME(dlc) - What is the correct behavior here?
			mb.mu.Lock()
			mb.removeIndex()
			mb.mu.Unlock()
			return err
		}
		tn += n

		// Success
		if int(n) == lbb {
			break
		}
		// Partial write..
		buf = buf[n:]
	}

	mb.mu.Lock()

	if mb.cache == nil {
		mb.mu.Unlock()
		return nil
	}
	mb.cache.flush = false

	// We did a successful write. If we have active consumers (recent loads) hold onto the cache.
	if ts := time.Now().UnixNano(); ts < mb.llts || (ts-mb.llts) <= int64(mb.cexp) {
		mb.cache.wp += tn
	} else {
		if lbb <= maxBufReuse {
			buf = buf[:0]
		} else {
			buf = nil
		}
		// Reset write pointer.
		mb.cache.wp = 0
		// Update our cache offset.
		mb.cache.off += tn
		// Place buffer back in the cache structure.
		mb.cache.buf = buf
	}
	mb.mu.Unlock()

	return nil
}

func (mb *msgBlock) clearLoading() {
	mb.mu.Lock()
	mb.loading = false
	mb.mu.Unlock()
}

// Will load msgs from disk.
func (mb *msgBlock) loadMsgs() error {
	mb.mu.Lock()
	// Check to see if we are loading already.
	if mb.loading {
		mb.mu.Unlock()
		return nil
	}
	// Check to see if we are full already.
	if mb.cache != nil && len(mb.cache.idx) == int(mb.msgs) && mb.cache.off == 0 {
		mb.mu.Unlock()
		return nil
	}
	mb.loading = true
	defer mb.clearLoading()
	mfn := mb.mfn
	mb.llts = time.Now().UnixNano()
	mb.mu.Unlock()

	// FIXME(dlc) - We could be smarter here.
	if mb.numWriteBytesPending() > 0 {
		mb.flushPendingWrites()
	}

	// Load in the whole block.
	buf, err := ioutil.ReadFile(mfn)
	if err != nil {
		return err
	}

	mb.mu.Lock()

	// Make sure this is cleared in case we had a partial.
	mb.clearCache()

	if err := mb.indexCacheBuf(buf); err != nil {
		mb.mu.Unlock()
		return err
	}

	if len(buf) > 0 {
		mb.cloads++
		mb.startCacheExpireTimer()
	}
	mb.mu.Unlock()

	return nil
}

// Fetch a message from this block, possibly reading in and caching the messages.
// We assume the block was selected and is correct, so we do not do range checks.
func (mb *msgBlock) fetchMsg(seq uint64) (*fileStoredMsg, error) {
	var sm *fileStoredMsg

	sm, err := mb.cacheLookup(seq)
	if err == nil || (err != errNoCache && err != errPartialCache) {
		return sm, err
	}

	// We have a cache miss here.
	if err := mb.loadMsgs(); err != nil {
		return nil, err
	}
	return mb.cacheLookup(seq)
}

var (
	errNoCache      = errors.New("no message cache")
	errBadMsg       = errors.New("malformed or corrupt message")
	errDeletedMsg   = errors.New("deleted message")
	errPartialCache = errors.New("partial cache")
	errNoPending    = errors.New("message block does not have pending data")
	errNotReadable  = errors.New("storage directory not readable")
	errFlushRunning = errors.New("flush is already running")
)

// Used for marking messages that have had their checksums checked.
// Used to signal a message record with headers.
const hbit = 1 << 31

// Used for marking erased messages sequences.
const ebit = 1 << 63

// Will do a lookup from the cache.
func (mb *msgBlock) cacheLookup(seq uint64) (*fileStoredMsg, error) {
	// Currently grab the write lock for optional use of mb.hh. Prefer this for now
	// vs read lock and promote. Also defer based on 1.14 performance.
	mb.mu.Lock()
	defer mb.mu.Unlock()
	return mb.cacheLookupLocked(seq)
}

// Will do a lookup from cache.
// lock should be held.
func (mb *msgBlock) cacheLookupLocked(seq uint64) (*fileStoredMsg, error) {
	if mb.cache == nil {
		return nil, errNoCache
	}

	if seq < mb.first.seq || seq < mb.cache.fseq || (seq-mb.cache.fseq) >= uint64(len(mb.cache.idx)) {
		return nil, ErrStoreMsgNotFound
	}

	// If we have a delete map check it.
	if mb.dmap != nil {
		if _, ok := mb.dmap[seq]; ok {
			return nil, errDeletedMsg
		}
	}

	// Update cache activity.
	mb.llts = time.Now().UnixNano()

	bi, _, hashChecked, _ := mb.slotInfo(int(seq - mb.cache.fseq))

	// We use the high bit to denote we have already checked the checksum.
	var hh hash.Hash64
	if !hashChecked {
		hh = mb.hh // This will force the hash check in msgFromBuf.
		mb.cache.idx[seq-mb.cache.fseq] = (bi | hbit)
	}

	// Check if partial
	if mb.cache.off > 0 && bi < uint32(mb.cache.off) {
		buf := mb.cache.buf
		mb.cache.buf = nil
		mb.cache.buf = buf
		return nil, errPartialCache
	}
	li := int(bi) - mb.cache.off
	buf := mb.cache.buf[li:]

	// Parse from the raw buffer.
	subj, hdr, msg, mseq, ts, err := msgFromBuf(buf, hh)
	if err != nil {
		return nil, err
	}
	if seq != mseq {
		return nil, fmt.Errorf("sequence numbers for cache load did not match, %d vs %d", seq, mseq)
	}
	sm := &fileStoredMsg{
		subj: subj,
		hdr:  hdr,
		msg:  msg,
		seq:  seq,
		ts:   ts,
		mb:   mb,
		off:  int64(bi),
	}
	return sm, nil
}

// Will return message for the given sequence number.
func (fs *fileStore) msgForSeq(seq uint64) (*fileStoredMsg, error) {
	fs.mu.RLock()
	if fs.closed {
		fs.mu.RUnlock()
		return nil, ErrStoreClosed
	}
	// Indicates we want first msg.
	if seq == 0 {
		seq = fs.state.FirstSeq
	}
	mb := fs.selectMsgBlock(seq)
	fs.mu.RUnlock()

	if mb == nil {
		var err = ErrStoreEOF
		fs.mu.RLock()
		if seq <= fs.state.LastSeq {
			err = ErrStoreMsgNotFound
		}
		fs.mu.RUnlock()
		return nil, err
	}
	// TODO(dlc) - older design had a check to prefetch when we knew we were
	// loading in order and getting close to end of current mb. Should add
	// something like it back in.
	return mb.fetchMsg(seq)
}

// Internal function to return msg parts from a raw buffer.
func msgFromBuf(buf []byte, hh hash.Hash64) (string, []byte, []byte, uint64, int64, error) {
	if len(buf) < msgHdrSize {
		return _EMPTY_, nil, nil, 0, 0, errBadMsg
	}
	var le = binary.LittleEndian

	hdr := buf[:msgHdrSize]
	rl := le.Uint32(hdr[0:])
	hasHeaders := rl&hbit != 0
	rl &^= hbit // clear header bit
	dlen := int(rl) - msgHdrSize
	slen := int(le.Uint16(hdr[20:]))
	// Simple sanity check.
	if dlen < 0 || slen > dlen || int(rl) > len(buf) {
		return _EMPTY_, nil, nil, 0, 0, errBadMsg
	}
	data := buf[msgHdrSize : msgHdrSize+dlen]
	// Do checksum tests here if requested.
	if hh != nil {
		hh.Reset()
		hh.Write(hdr[4:20])
		hh.Write(data[:slen])
		if hasHeaders {
			hh.Write(data[slen+4 : dlen-8])
		} else {
			hh.Write(data[slen : dlen-8])
		}
		if !bytes.Equal(hh.Sum(nil), data[len(data)-8:]) {
			return _EMPTY_, nil, nil, 0, 0, errBadMsg
		}
	}
	seq := le.Uint64(hdr[4:])
	if seq&ebit != 0 {
		seq = 0
	}
	ts := int64(le.Uint64(hdr[12:]))
	// FIXME(dlc) - We need to not allow appends to the underlying buffer, so we will
	// fix the capacity. This will cause a copy though in stream:internalSendLoop when
	// we append CRLF but this was causing a race. Need to rethink more to avoid this copy.
	end := dlen - 8
	var mhdr, msg []byte
	if hasHeaders {
		hl := le.Uint32(data[slen:])
		bi := slen + 4
		li := bi + int(hl)
		mhdr = data[bi:li:li]
		msg = data[li:end:end]
	} else {
		msg = data[slen:end:end]
	}
	return string(data[:slen]), mhdr, msg, seq, ts, nil
}

// LoadMsg will lookup the message by sequence number and return it if found.
func (fs *fileStore) LoadMsg(seq uint64) (string, []byte, []byte, int64, error) {
	sm, err := fs.msgForSeq(seq)
	if sm != nil {
		return sm.subj, sm.hdr, sm.msg, sm.ts, nil
	}
	return "", nil, nil, 0, err
}

// State returns the current state of the stream.
func (fs *fileStore) State() StreamState {
	fs.mu.RLock()
	state := fs.state
	state.Consumers = len(fs.cfs)
	fs.mu.RUnlock()
	return state
}

const emptyRecordLen = 22 + 8

func fileStoreMsgSize(subj string, hdr, msg []byte) uint64 {
	if len(hdr) == 0 {
		// length of the message record (4bytes) + seq(8) + ts(8) + subj_len(2) + subj + msg + hash(8)
		return uint64(22 + len(subj) + len(msg) + 8)
	}
	// length of the message record (4bytes) + seq(8) + ts(8) + subj_len(2) + subj + hdr_len(4) + hdr + msg + hash(8)
	return uint64(22 + len(subj) + 4 + len(hdr) + len(msg) + 8)
}

func fileStoreMsgSizeEstimate(slen, maxPayload int) uint64 {
	return uint64(emptyRecordLen + slen + 4 + maxPayload)
}

// Lock should not be held.
func (fs *fileStore) flushPendingWritesUnlocked() error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.flushPendingWrites()
}

// Lock should be held.
func (fs *fileStore) flushPendingWrites() error {
	mb := fs.lmb
	if mb == nil || mb.mfd == nil {
		return errNoPending
	}
	err := mb.flushPendingWrites()
	if err != nil {
		switch err {
		case errNoCache, errNoPending:
			err = nil
		default:
			return err
		}
	}
	// Now index info
	return mb.writeIndexInfo()
}

// Write index info to the appropriate file.
func (mb *msgBlock) writeIndexInfo() error {
	// HEADER: magic version msgs bytes fseq fts lseq lts checksum
	var hdr [indexHdrSize]byte

	// Write header
	hdr[0] = magic
	hdr[1] = version

	mb.mu.Lock()
	n := hdrLen
	n += binary.PutUvarint(hdr[n:], mb.msgs)
	n += binary.PutUvarint(hdr[n:], mb.bytes)
	n += binary.PutUvarint(hdr[n:], mb.first.seq)
	n += binary.PutVarint(hdr[n:], mb.first.ts)
	n += binary.PutUvarint(hdr[n:], mb.last.seq)
	n += binary.PutVarint(hdr[n:], mb.last.ts)
	n += binary.PutUvarint(hdr[n:], uint64(len(mb.dmap)))
	buf := append(hdr[:n], mb.lchk[:]...)

	// Append a delete map if needed
	if len(mb.dmap) > 0 {
		buf = append(buf, mb.genDeleteMap()...)
	}
	var err error
	if mb.ifd == nil {
		ifd, err := os.OpenFile(mb.ifn, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			mb.mu.Unlock()
			return err
		}
		mb.ifd = ifd
	}
	// TODO(dlc) - don't hold lock here.
	n, err = mb.ifd.WriteAt(buf, 0)
	if err == nil {
		mb.liwsz = int64(n)
	}
	mb.mu.Unlock()

	return err
}

func (mb *msgBlock) readIndexInfo() error {
	buf, err := ioutil.ReadFile(mb.ifn)
	if err != nil {
		return err
	}

	if err := checkHeader(buf); err != nil {
		defer os.Remove(mb.ifn)
		return fmt.Errorf("bad index file")
	}

	bi := hdrLen

	// Helpers, will set i to -1 on error.
	readSeq := func() uint64 {
		if bi < 0 {
			return 0
		}
		seq, n := binary.Uvarint(buf[bi:])
		if n <= 0 {
			bi = -1
			return 0
		}
		bi += n
		return seq
	}
	readCount := readSeq
	readTimeStamp := func() int64 {
		if bi < 0 {
			return 0
		}
		ts, n := binary.Varint(buf[bi:])
		if n <= 0 {
			bi = -1
			return -1
		}
		bi += n
		return ts
	}
	mb.msgs = readCount()
	mb.bytes = readCount()
	mb.first.seq = readSeq()
	mb.first.ts = readTimeStamp()
	mb.last.seq = readSeq()
	mb.last.ts = readTimeStamp()
	dmapLen := readCount()

	// Checksum
	copy(mb.lchk[0:], buf[bi:bi+checksumSize])
	bi += checksumSize

	// Now check for presence of a delete map
	if dmapLen > 0 {
		mb.dmap = make(map[uint64]struct{}, dmapLen)
		for i := 0; i < int(dmapLen); i++ {
			seq := readSeq()
			if seq == 0 {
				break
			}
			mb.dmap[seq+mb.first.seq] = struct{}{}
		}
	}

	return nil
}

func (mb *msgBlock) genDeleteMap() []byte {
	if len(mb.dmap) == 0 {
		return nil
	}
	buf := make([]byte, len(mb.dmap)*binary.MaxVarintLen64)
	// We use first seq as an offset to cut down on size.
	fseq, n := uint64(mb.first.seq), 0
	for seq := range mb.dmap {
		// This is for lazy cleanup as the first sequence moves up.
		if seq <= fseq {
			delete(mb.dmap, seq)
		} else {
			n += binary.PutUvarint(buf[n:], seq-fseq)
		}
	}
	return buf[:n]
}

func syncAndClose(mfd, ifd *os.File) {
	if mfd != nil {
		mfd.Sync()
		mfd.Close()
	}
	if ifd != nil {
		ifd.Sync()
		ifd.Close()
	}
}

// Will return total number of cache loads.
func (fs *fileStore) cacheLoads() uint64 {
	var tl uint64
	fs.mu.RLock()
	for _, mb := range fs.blks {
		tl += mb.cloads
	}
	fs.mu.RUnlock()
	return tl
}

// Will return total number of cached bytes.
func (fs *fileStore) cacheSize() uint64 {
	var sz uint64
	fs.mu.RLock()
	for _, mb := range fs.blks {
		mb.mu.RLock()
		if mb.cache != nil {
			sz += uint64(len(mb.cache.buf))
		}
		mb.mu.RUnlock()
	}
	fs.mu.RUnlock()
	return sz
}

// Will return total number of dmapEntries for all msg blocks.
func (fs *fileStore) dmapEntries() int {
	var total int
	fs.mu.RLock()
	for _, mb := range fs.blks {
		total += len(mb.dmap)
	}
	fs.mu.RUnlock()
	return total
}

// Purge will remove all messages from this store.
// Will return the number of purged messages.
func (fs *fileStore) Purge() uint64 {
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		return 0
	}

	purged := fs.state.Msgs
	rbytes := int64(fs.state.Bytes)

	fs.state.FirstSeq = fs.state.LastSeq + 1
	fs.state.FirstTime = time.Time{}

	fs.state.Bytes = 0
	fs.state.Msgs = 0

	for _, mb := range fs.blks {
		mb.dirtyClose()
	}

	fs.blks = nil
	fs.lmb = nil

	// Move the msgs directory out of the way, will delete out of band.
	// FIXME(dlc) - These can error and we need to change api above to propagate?
	mdir := path.Join(fs.fcfg.StoreDir, msgDir)
	pdir := path.Join(fs.fcfg.StoreDir, purgeDir)
	// If purge directory still exists then we need to wait
	// in place and remove since rename would fail.
	if _, err := os.Stat(pdir); err == nil {
		os.RemoveAll(pdir)
	}
	os.Rename(mdir, pdir)
	go os.RemoveAll(pdir)
	// Create new one.
	os.MkdirAll(mdir, 0755)

	// Make sure we have a lmb to write to.
	fs.newMsgBlockForWrite()

	fs.lmb.first.seq = fs.state.FirstSeq
	fs.lmb.last.seq = fs.state.LastSeq
	fs.lmb.writeIndexInfo()

	cb := fs.scb
	fs.mu.Unlock()

	if cb != nil {
		cb(-int64(purged), -rbytes, 0)
	}

	return purged
}

// Returns number of msg blks.
func (fs *fileStore) numMsgBlocks() int {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return len(fs.blks)
}

// Lock should be held.
func (mb *msgBlock) removeIndex() {
	if mb.ifd != nil {
		mb.ifd.Close()
		mb.ifd = nil
	}
	os.Remove(mb.ifn)
}

// Removes the msgBlock
// Both locks should be held.
func (fs *fileStore) removeMsgBlock(mb *msgBlock) {
	mb.removeIndex()
	if mb.mfd != nil {
		mb.mfd.Close()
		mb.mfd = nil
	}
	os.Remove(mb.mfn)

	for i, omb := range fs.blks {
		if mb == omb {
			fs.blks = append(fs.blks[:i], fs.blks[i+1:]...)
			break
		}
	}
	// Check for us being last message block
	if mb == fs.lmb {
		fs.lmb = nil
		fs.newMsgBlockForWrite()
		fs.lmb.first = mb.first
		fs.lmb.last = mb.last
		fs.lmb.writeIndexInfo()
	}
	go mb.close(true)
}

// Called by purge to simply get rid of the cache and close and fds.
// FIXME(dlc) - Merge with below func.
func (mb *msgBlock) dirtyClose() {
	if mb == nil {
		return
	}
	mb.mu.Lock()
	// Close cache
	mb.clearCache()
	// Quit our loops.
	if mb.qch != nil {
		close(mb.qch)
		mb.qch = nil
	}
	if mb.mfd != nil {
		mb.mfd.Close()
		mb.mfd = nil
	}
	if mb.ifd != nil {
		mb.ifd.Close()
		mb.ifd = nil
	}
	mb.mu.Unlock()
}

func (mb *msgBlock) close(sync bool) {
	if mb == nil {
		return
	}
	mb.mu.Lock()
	// Close cache
	mb.clearCache()
	// Quit our loops.
	if mb.qch != nil {
		close(mb.qch)
		mb.qch = nil
	}
	if sync {
		syncAndClose(mb.mfd, mb.ifd)
	} else {
		go syncAndClose(mb.mfd, mb.ifd)
	}
	mb.mfd = nil
	mb.ifd = nil
	mb.mu.Unlock()
}

func (fs *fileStore) closeAllMsgBlocks(sync bool) {
	for _, mb := range fs.blks {
		mb.close(sync)
	}
}

func (fs *fileStore) closeLastMsgBlock(sync bool) {
	fs.lmb.close(sync)
}

func (fs *fileStore) Delete() error {
	if fs.isClosed() {
		return ErrStoreClosed
	}
	// TODO(dlc) - check error here?
	fs.Purge()
	if err := fs.Stop(); err != nil {
		return err
	}
	return os.RemoveAll(fs.fcfg.StoreDir)
}

func (fs *fileStore) Stop() error {
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		return ErrStoreClosed
	}
	fs.closed = true
	close(fs.qch)

	err := fs.flushPendingWrites()
	fs.lmb = nil

	fs.closeAllMsgBlocks(true)

	if fs.syncTmr != nil {
		fs.syncTmr.Stop()
		fs.syncTmr = nil
	}
	if fs.ageChk != nil {
		fs.ageChk.Stop()
		fs.ageChk = nil
	}

	var _cfs [256]*consumerFileStore
	cfs := append(_cfs[:0], fs.cfs...)
	fs.cfs = nil
	fs.mu.Unlock()

	for _, o := range cfs {
		o.Stop()
	}

	return err
}

const errFile = "errors.txt"

// Stream our snapshot through gzip and tar.
func (fs *fileStore) streamSnapshot(w io.WriteCloser, blks []*msgBlock, includeConsumers bool) {
	defer w.Close()

	bw := bufio.NewWriter(w)
	defer bw.Flush()

	gzw, _ := gzip.NewWriterLevel(bw, gzip.BestSpeed)
	defer gzw.Close()

	tw := tar.NewWriter(gzw)
	defer tw.Close()

	defer func() {
		fs.mu.Lock()
		fs.sips--
		fs.mu.Unlock()
	}()

	modTime := time.Now().UTC()

	writeFile := func(name string, buf []byte) error {
		hdr := &tar.Header{
			Name:    name,
			Mode:    0600,
			ModTime: modTime,
			Uname:   "nats",
			Gname:   "nats",
			Size:    int64(len(buf)),
			Format:  tar.FormatPAX,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if _, err := tw.Write(buf); err != nil {
			return err
		}
		return nil
	}

	writeErr := func(err string) {
		writeFile(errFile, []byte(err))
	}

	fs.mu.Lock()
	// Write our general meta data.
	if err := fs.writeStreamMeta(); err != nil {
		fs.mu.Unlock()
		writeErr(fmt.Sprintf("Could not write stream meta file: %v", err))
		return
	}
	meta, err := ioutil.ReadFile(path.Join(fs.fcfg.StoreDir, JetStreamMetaFile))
	if err != nil {
		fs.mu.Unlock()
		writeErr(fmt.Sprintf("Could not read stream meta file: %v", err))
		return
	}
	sum, err := ioutil.ReadFile(path.Join(fs.fcfg.StoreDir, JetStreamMetaFileSum))
	if err != nil {
		fs.mu.Unlock()
		writeErr(fmt.Sprintf("Could not read stream checksum file: %v", err))
		return
	}
	fs.mu.Unlock()

	// Meta first.
	if writeFile(JetStreamMetaFile, meta) != nil {
		return
	}
	if writeFile(JetStreamMetaFileSum, sum) != nil {
		return
	}

	// Now do messages themselves.
	lmb := fs.lastMsgBlock()

	// Can't use join path here, zip only recognizes relative paths with forward slashes.
	msgPre := msgDir + "/"

	for _, mb := range blks {
		if mb == lmb {
			fs.flushPendingWritesUnlocked()
		}
		mb.mu.Lock()
		buf, err := ioutil.ReadFile(mb.ifn)
		if err != nil {
			mb.mu.Unlock()
			writeErr(fmt.Sprintf("Could not read message block [%d] meta file: %v", mb.index, err))
			return
		}
		if writeFile(msgPre+fmt.Sprintf(indexScan, mb.index), buf) != nil {
			mb.mu.Unlock()
			return
		}
		// We could stream but don't want to hold the lock and prevent changes, so just read in and
		// release the lock for now.
		// TODO(dlc) - Maybe reuse buffer?
		buf, err = ioutil.ReadFile(mb.mfn)
		if err != nil {
			mb.mu.Unlock()
			writeErr(fmt.Sprintf("Could not read message block [%d]: %v", mb.index, err))
			return
		}
		mb.mu.Unlock()
		// Do this one unlocked.
		if writeFile(msgPre+fmt.Sprintf(blkScan, mb.index), buf) != nil {
			return
		}
	}

	// Bail if no consumers requested.
	if !includeConsumers {
		return
	}

	// Do consumers' state last.
	fs.mu.Lock()
	cfs := fs.cfs
	fs.mu.Unlock()

	for _, o := range cfs {
		o.syncStateFile()
		o.mu.Lock()
		meta, err := ioutil.ReadFile(path.Join(o.odir, JetStreamMetaFile))
		if err != nil {
			o.mu.Unlock()
			writeErr(fmt.Sprintf("Could not read consumer meta file for %q: %v", o.name, err))
			return
		}
		sum, err := ioutil.ReadFile(path.Join(o.odir, JetStreamMetaFileSum))
		if err != nil {
			o.mu.Unlock()
			writeErr(fmt.Sprintf("Could not read consumer checksum file for %q: %v", o.name, err))
			return
		}
		state, err := ioutil.ReadFile(path.Join(o.odir, consumerState))
		if err != nil {
			o.mu.Unlock()
			writeErr(fmt.Sprintf("Could not read consumer state for %q: %v", o.name, err))
			return
		}
		odirPre := consumerDir + "/" + o.name
		o.mu.Unlock()

		// Write all the consumer files.
		if writeFile(path.Join(odirPre, JetStreamMetaFile), meta) != nil {
			return
		}
		if writeFile(path.Join(odirPre, JetStreamMetaFileSum), sum) != nil {
			return
		}
		writeFile(path.Join(odirPre, consumerState), state)
	}
}

// Create a snapshot of this stream and its consumer's state along with messages.
func (fs *fileStore) Snapshot(deadline time.Duration, checkMsgs, includeConsumers bool) (*SnapshotResult, error) {
	fs.mu.Lock()
	if fs.closed {
		fs.mu.Unlock()
		return nil, ErrStoreClosed
	}
	// Only allow one at a time.
	if fs.sips > 0 {
		fs.mu.Unlock()
		return nil, ErrStoreSnapshotInProgress
	}
	// Mark us as snapshotting
	fs.sips += 1
	blks := fs.blks
	blkSize := int(fs.fcfg.BlockSize)
	fs.mu.Unlock()

	if checkMsgs {
		bad := fs.checkMsgs()
		if len(bad) > 0 {
			return nil, fmt.Errorf("snapshot check detected %d bad messages", len(bad))
		}
	}

	pr, pw := net.Pipe()

	// Set a write deadline here to protect ourselves.
	if deadline > 0 {
		pw.SetWriteDeadline(time.Now().Add(deadline))
	}
	// Stream in separate Go routine.
	go fs.streamSnapshot(pw, blks, includeConsumers)

	return &SnapshotResult{pr, blkSize, len(blks)}, nil
}

// Helper to return the config.
func (fs *fileStore) fileStoreConfig() FileStoreConfig {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.fcfg
}

////////////////////////////////////////////////////////////////////////////////
// Consumers
////////////////////////////////////////////////////////////////////////////////

type consumerFileStore struct {
	mu     sync.Mutex
	fs     *fileStore
	cfg    *FileConsumerInfo
	name   string
	odir   string
	ifn    string
	ifd    *os.File
	lwsz   int64
	hh     hash.Hash64
	fch    chan struct{}
	qch    chan struct{}
	closed bool
}

func (fs *fileStore) ConsumerStore(name string, cfg *ConsumerConfig) (ConsumerStore, error) {
	if fs == nil {
		return nil, fmt.Errorf("filestore is nil")
	}
	if fs.isClosed() {
		return nil, ErrStoreClosed
	}
	if cfg == nil || name == "" {
		return nil, fmt.Errorf("bad consumer config")
	}
	odir := path.Join(fs.fcfg.StoreDir, consumerDir, name)
	if err := os.MkdirAll(odir, 0755); err != nil {
		return nil, fmt.Errorf("could not create consumer directory - %v", err)
	}
	csi := &FileConsumerInfo{ConsumerConfig: *cfg}
	o := &consumerFileStore{
		fs:   fs,
		cfg:  csi,
		name: name,
		odir: odir,
		ifn:  path.Join(odir, consumerState),
		fch:  make(chan struct{}),
		qch:  make(chan struct{}),
	}
	key := sha256.Sum256([]byte(fs.cfg.Name + "/" + name))
	hh, err := highwayhash.New64(key[:])
	if err != nil {
		return nil, fmt.Errorf("could not create hash: %v", err)
	}
	o.hh = hh

	// Write our meta data iff does not exist.
	meta := path.Join(odir, JetStreamMetaFile)
	if _, err := os.Stat(meta); err != nil && os.IsNotExist(err) {
		csi.Created = time.Now().UTC()
		if err := o.writeConsumerMeta(); err != nil {
			return nil, err
		}
	}

	fs.mu.Lock()
	fs.cfs = append(fs.cfs, o)
	fs.mu.Unlock()

	return o, nil
}

const seqsHdrSize = 6*binary.MaxVarintLen64 + hdrLen

func (o *consumerFileStore) Update(state *ConsumerState) error {
	// Sanity checks.
	if state.Delivered.ConsumerSeq < 1 || state.Delivered.StreamSeq < 1 {
		return fmt.Errorf("bad delivered sequences")
	}
	if state.AckFloor.ConsumerSeq > state.Delivered.ConsumerSeq {
		return fmt.Errorf("bad ack floor for consumer")
	}
	if state.AckFloor.StreamSeq > state.Delivered.StreamSeq {
		return fmt.Errorf("bad ack floor for stream")
	}

	var hdr [seqsHdrSize]byte

	// Write header
	hdr[0] = magic
	hdr[1] = version

	n := hdrLen
	n += binary.PutUvarint(hdr[n:], state.AckFloor.ConsumerSeq)
	n += binary.PutUvarint(hdr[n:], state.AckFloor.StreamSeq)
	n += binary.PutUvarint(hdr[n:], state.Delivered.ConsumerSeq-state.AckFloor.ConsumerSeq)
	n += binary.PutUvarint(hdr[n:], state.Delivered.StreamSeq-state.AckFloor.StreamSeq)
	n += binary.PutUvarint(hdr[n:], uint64(len(state.Pending)))
	buf := hdr[:n]

	// These are optional, but always write len. This is to avoid truncate inline.
	// If these get big might make more sense to do writes directly to the file.

	if len(state.Pending) > 0 {
		mbuf := make([]byte, len(state.Pending)*(2*binary.MaxVarintLen64)+binary.MaxVarintLen64)
		aflr := state.AckFloor.StreamSeq
		maxd := state.Delivered.StreamSeq

		// To save space we select the smallest timestamp.
		var mints int64
		for k, v := range state.Pending {
			if mints == 0 || v < mints {
				mints = v
			}
			if k <= aflr || k > maxd {
				return fmt.Errorf("bad pending entry, sequence [%d] out of range", k)
			}
		}

		// Downsample the minimum timestamp.
		mints /= int64(time.Second)
		var n int
		// Write minimum timestamp we found from above.
		n += binary.PutVarint(mbuf[n:], mints)

		for k, v := range state.Pending {
			n += binary.PutUvarint(mbuf[n:], k-aflr)
			// Downsample to seconds to save on space. Subsecond resolution not
			// needed for recovery etc.
			n += binary.PutVarint(mbuf[n:], (v/int64(time.Second))-mints)
		}
		buf = append(buf, mbuf[:n]...)
	}

	var lenbuf [binary.MaxVarintLen64]byte
	n = binary.PutUvarint(lenbuf[0:], uint64(len(state.Redelivered)))
	buf = append(buf, lenbuf[:n]...)

	// We expect these to be small so will not do anything too crazy here to
	// keep the size small. Trick could be to offset sequence like above, but
	// we would need to know low sequence number for redelivery, can't depend on ackfloor etc.
	if len(state.Redelivered) > 0 {
		mbuf := make([]byte, len(state.Redelivered)*(2*binary.MaxVarintLen64))
		var n int
		for k, v := range state.Redelivered {
			n += binary.PutUvarint(mbuf[n:], k)
			n += binary.PutUvarint(mbuf[n:], v)
		}
		buf = append(buf, mbuf[:n]...)
	}

	// Check if we have the index file open.
	o.mu.Lock()

	err := o.ensureStateFileOpen()
	if err == nil {
		n, err = o.ifd.WriteAt(buf, 0)
		o.lwsz = int64(n)
	}
	o.mu.Unlock()

	return err
}

// Will upodate the config. Only used when recovering ephemerals.
func (o *consumerFileStore) updateConfig(cfg ConsumerConfig) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cfg = &FileConsumerInfo{ConsumerConfig: cfg}
	return o.writeConsumerMeta()
}

// Write out the consumer meta data, i.e. state.
// Lock should be held.
func (cfs *consumerFileStore) writeConsumerMeta() error {
	meta := path.Join(cfs.odir, JetStreamMetaFile)
	if _, err := os.Stat(meta); (err != nil && !os.IsNotExist(err)) || err == nil {
		return err
	}
	b, err := json.MarshalIndent(cfs.cfg, _EMPTY_, "  ")
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(meta, b, 0644); err != nil {
		return err
	}
	cfs.hh.Reset()
	cfs.hh.Write(b)
	checksum := hex.EncodeToString(cfs.hh.Sum(nil))
	sum := path.Join(cfs.odir, JetStreamMetaFileSum)
	if err := ioutil.WriteFile(sum, []byte(checksum), 0644); err != nil {
		return err
	}
	return nil
}

func (o *consumerFileStore) syncStateFile() {
	// FIXME(dlc) - Hold last error?
	o.mu.Lock()
	if o.ifd != nil {
		o.ifd.Sync()
		o.ifd.Truncate(o.lwsz)
	}
	o.mu.Unlock()
}

// Lock should be held.
func (o *consumerFileStore) ensureStateFileOpen() error {
	if o.ifd == nil {
		ifd, err := os.OpenFile(o.ifn, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return err
		}
		o.ifd = ifd
	}
	return nil
}

func checkHeader(hdr []byte) error {
	if hdr == nil || len(hdr) < 2 || hdr[0] != magic || hdr[1] != version {
		return fmt.Errorf("corrupt state file")
	}
	return nil
}

// State retrieves the state from the state file.
// This is not expected to be called in high performance code, only on startup.
func (o *consumerFileStore) State() (*ConsumerState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()

	buf, err := ioutil.ReadFile(o.ifn)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	var state *ConsumerState

	if len(buf) == 0 {
		return state, nil
	}

	if err := checkHeader(buf); err != nil {
		return nil, err
	}

	bi := hdrLen

	// Helpers, will set i to -1 on error.
	readSeq := func() uint64 {
		if bi < 0 {
			return 0
		}
		seq, n := binary.Uvarint(buf[bi:])
		if n <= 0 {
			bi = -1
			return 0
		}
		bi += n
		return seq
	}
	readTimeStamp := func() int64 {
		if bi < 0 {
			return 0
		}
		ts, n := binary.Varint(buf[bi:])
		if n <= 0 {
			bi = -1
			return -1
		}
		bi += n
		return ts
	}
	// Just for clarity below.
	readLen := readSeq
	readCount := readSeq

	state = &ConsumerState{}
	state.AckFloor.ConsumerSeq = readSeq()
	state.AckFloor.StreamSeq = readSeq()
	state.Delivered.ConsumerSeq = readSeq()
	state.Delivered.StreamSeq = readSeq()

	if bi == -1 {
		return nil, fmt.Errorf("corrupt state file")
	}
	// Adjust back.
	state.Delivered.ConsumerSeq += state.AckFloor.ConsumerSeq
	state.Delivered.StreamSeq += state.AckFloor.StreamSeq

	numPending := readLen()

	// We have additional stuff.
	if numPending > 0 {
		mints := readTimeStamp()
		state.Pending = make(map[uint64]int64, numPending)
		for i := 0; i < int(numPending); i++ {
			seq := readSeq()
			ts := readTimeStamp()
			if seq == 0 || ts == -1 {
				return nil, fmt.Errorf("corrupt state file")
			}
			// Adjust seq back.
			seq += state.AckFloor.StreamSeq
			// Adjust the timestamp back.
			ts = (ts + mints) * int64(time.Second)
			// Store in pending.
			state.Pending[seq] = ts
		}
	}

	numRedelivered := readLen()

	// We have redelivered entries here.
	if numRedelivered > 0 {
		state.Redelivered = make(map[uint64]uint64, numRedelivered)
		for i := 0; i < int(numRedelivered); i++ {
			seq := readSeq()
			n := readCount()
			if seq == 0 || n == 0 {
				return nil, fmt.Errorf("corrupt state file")
			}
			state.Redelivered[seq] = n
		}
	}
	return state, nil
}

// Stop the processing of the consumers's state.
func (o *consumerFileStore) Stop() error {
	o.mu.Lock()
	if o.closed {
		o.mu.Unlock()
		return nil
	}
	o.closed = true
	if o.ifd != nil {
		o.ifd.Sync()
		o.ifd.Close()
		o.ifd = nil
	}
	fs := o.fs
	o.mu.Unlock()
	fs.removeConsumer(o)
	return nil
}

// Delete the consumer.
func (o *consumerFileStore) Delete() error {
	// Call stop first. OK if already stopped.
	o.Stop()
	o.mu.Lock()
	var err error
	if o.odir != "" {
		err = os.RemoveAll(o.odir)
	}
	o.mu.Unlock()
	return err
}

func (fs *fileStore) removeConsumer(cfs *consumerFileStore) {
	fs.mu.Lock()
	for i, o := range fs.cfs {
		if o == cfs {
			fs.cfs = append(fs.cfs[:i], fs.cfs[i+1:]...)
			break
		}
	}
	fs.mu.Unlock()
}

// Templates
type templateFileStore struct {
	dir string
	hh  hash.Hash64
}

func newTemplateFileStore(storeDir string) *templateFileStore {
	tdir := path.Join(storeDir, tmplsDir)
	key := sha256.Sum256([]byte("templates"))
	hh, err := highwayhash.New64(key[:])
	if err != nil {
		return nil
	}
	return &templateFileStore{dir: tdir, hh: hh}
}

func (ts *templateFileStore) Store(t *StreamTemplate) error {
	dir := path.Join(ts.dir, t.Name)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("could not create templates storage directory for %q- %v", t.Name, err)
	}
	meta := path.Join(dir, JetStreamMetaFile)
	if _, err := os.Stat(meta); (err != nil && !os.IsNotExist(err)) || err == nil {
		return err
	}
	t.mu.Lock()
	b, err := json.MarshalIndent(t, _EMPTY_, "  ")
	t.mu.Unlock()
	if err != nil {
		return err
	}
	if err := ioutil.WriteFile(meta, b, 0644); err != nil {
		return err
	}
	// FIXME(dlc) - Do checksum
	ts.hh.Reset()
	ts.hh.Write(b)
	checksum := hex.EncodeToString(ts.hh.Sum(nil))
	sum := path.Join(dir, JetStreamMetaFileSum)
	if err := ioutil.WriteFile(sum, []byte(checksum), 0644); err != nil {
		return err
	}
	return nil
}

func (ts *templateFileStore) Delete(t *StreamTemplate) error {
	return os.RemoveAll(path.Join(ts.dir, t.Name))
}
