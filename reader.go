package iscsi

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

type reader struct {
	dev       *device
	lba       int64
	offset    int64
	blocksize int64

	// lbprz indicates the target guarantees deallocated/anchored blocks
	// read back as zero. SkipSparseRegions relies on this for correctness.
	lbprz      bool
	skipSparse bool

	closed atomic.Bool
	waiter sync.WaitGroup
}

// lbaStatusAllocLen requests a single LBA status descriptor: 8 bytes of
// header plus one 16 byte descriptor. That's enough to learn the
// provisioning status of the run of blocks starting at the queried LBA.
const lbaStatusAllocLen = 24

// ErrDeviceClosed indicates that an operation cannot be performed because
// the device has been closed.
var ErrDeviceClosed = errors.New("device is closed")

// Reader implements io.Reader, io.ReaderAt, io.Closer, and io.Seeker for
// an underlying ISCSI device. The device must be already connected.
func Reader(dev *device) (*reader, error) {
	c, err := dev.ReadCapacity16()
	if err != nil {
		return nil, fmt.Errorf("failed to get capacity of device: %w", err)
	}

	_, lbprz, err := dev.getProvisioningInfo()
	if err != nil {
		logger().Debug("failed to query logical block provisioning info, sparse reads will be unavailable", slog.Any("error", err))
	}

	return &reader{
		dev:       dev,
		lba:       int64(c.MaxLBA) + 1,
		offset:    0,
		blocksize: int64(c.BlockSize),
		lbprz:     lbprz,
	}, nil
}

// SkipSparseRegions enables an optimization for subsequent reads: before
// issuing a SCSI READ, the reader first queries the target for the
// logical block provisioning status of the requested range, and if the
// entire range is reported as deallocated or anchored, it fills the
// caller's buffer with zeroes locally instead of transferring the
// (nonexistent) data over the wire.
//
// This is a no-op if the underlying LUN doesn't report LBPRZ (deallocated
// blocks guaranteed to read back as zero) in its READ CAPACITY(16)
// response, since it would otherwise be unsafe to synthesize the read
// locally.
func (r *reader) SkipSparseRegions() {
	if !r.lbprz {
		logger().Debug("SkipSparseRegions requested but target does not report LBPRZ; ignoring")
		return
	}
	r.skipSparse = true
}

// Close is a concurrency-safe method that disconnects the underlying ISCSI device
// after waiting for any in-flight reads to complete. However, as a result, if
// the read(s) take a long time to complete for any reason, this method may take a
// while to finish and return, so calls can be wrapped in a goroutine if needed.
func (r *reader) Close() error {
	r.closed.Store(true)
	r.waiter.Wait()
	return r.dev.Disconnect()
}

func (r *reader) Read(p []byte) (n int, err error) {
	if r.closed.Load() {
		return 0, ErrDeviceClosed
	}

	r.waiter.Add(1)
	defer r.waiter.Done()

	readLen, err := r.ReadAt(p, r.offset)
	r.offset += int64(readLen)
	return readLen, err
}

func (r *reader) ReadAt(p []byte, off int64) (n int, err error) {
	if r.closed.Load() {
		return 0, ErrDeviceClosed
	}

	r.waiter.Add(1)
	defer r.waiter.Done()

	if off >= r.blocksize*r.lba {
		logger().Debug("offset past at EOF", slog.Int("offset", int(off)))
		return 0, io.EOF
	}
	logger().Debug("ReadAt", slog.Int("bytes", len(p)), slog.Int("offset", int(off)))
	// find our starting lba
	startBlock := off / r.blocksize
	endOffset := len(p) + int(off)
	blocks := (endOffset-int(off))/int(r.blocksize)
	blocks = min(blocks, int(r.lba)-int(startBlock))

	// handle EOF
	if (endOffset/int(r.blocksize)) > int(r.lba) || blocks == int(r.lba-startBlock) {
		err = io.EOF
		logger().Debug("reached EOF", slog.Int("lba", int(r.lba)), slog.Int("endOffset", endOffset))
		blocks = int(r.lba - startBlock)
	} else if endOffset%int(r.blocksize) != 0 {
		// if endoffset is not block aligned then we need to read one more block
		blocks++
	}

	blocks = min(blocks, int(r.lba)-int(startBlock))
	readBytes, readErr := r.readBlocks(startBlock, blocks)
	if readErr != nil {
		return 0, fmt.Errorf("iscsi device read error: %w", readErr)
	}

	blockOffset := off % r.blocksize
	logger().Debug(fmt.Sprintf("offset %d into block %d", blockOffset, startBlock))

	var result []byte
	l := min(len(p), len(readBytes))
	if err == io.EOF {
		result = readBytes[blockOffset:]
	} else if blockOffset > 0 {
		// sometimes we get fewer than the number of requested blocks
		// even when not near the max lba? (at least when testing with gotgt)
		// unclear yet if this is acceptable for iscsi or a flaw in gotgt
		// make sure not to overshoot length of readBytes in that case
		result = readBytes[blockOffset:min(l+int(blockOffset), len(readBytes))]
	} else {
		result = readBytes[:l]
	}

	logger().Debug("finished read", slog.Int("bytes", len(result)))
	return copy(p, result), err
}

// readBlocks returns the requested blocks, either by reading them from the
// device or, if SkipSparseRegions is enabled and the target confirms the
// whole range is deallocated, by synthesizing zeroes locally.
func (r *reader) readBlocks(startBlock int64, blocks int) ([]byte, error) {
	if r.skipSparse && r.rangeIsDeallocated(startBlock, blocks) {
		logger().Debug("skipping read of deallocated range", slog.Int64("startBlock", startBlock), slog.Int("blocks", blocks))
		return make([]byte, blocks*int(r.blocksize)), nil
	}
	return r.dev.Read16(Read16{
		LBA:       int(startBlock),
		BlockSize: int(r.blocksize),
		Blocks:    blocks,
	})
}

// rangeIsDeallocated reports whether the target confirms every block in
// [startBlock, startBlock+blocks) is deallocated or anchored. It falls
// back to false (treat as mapped) on any error or ambiguous response,
// since an unnecessary real read is always safe.
func (r *reader) rangeIsDeallocated(startBlock int64, blocks int) bool {
	descriptors, err := r.dev.GetLBAStatus(startBlock, lbaStatusAllocLen)
	if err != nil {
		logger().Debug("get lba status failed, falling back to reading device", slog.Any("error", err))
		return false
	}
	if len(descriptors) == 0 {
		return false
	}
	d := descriptors[0]
	if d.Provisioning == ProvisioningStatusMapped {
		return false
	}
	return d.LBA+d.NumBlocks-startBlock >= int64(blocks)
}

// TODO: (willgorman) tests
func (r *reader) Seek(offset int64, whence int) (int64, error) {
	if r.closed.Load() {
		return 0, ErrDeviceClosed
	}

	r.waiter.Add(1)
	defer r.waiter.Done()

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.offset + offset
	case io.SeekEnd:
		abs = r.lba*r.blocksize + offset
	default:
		return 0, errors.New("iscsi.Reader.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("iscsi.Reader.Seek: negative position")
	}
	r.offset = abs
	return abs, nil
}
