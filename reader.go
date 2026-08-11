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

	closed atomic.Bool
	waiter sync.WaitGroup
}

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

	return &reader{
		dev:       dev,
		lba:       int64(c.MaxLBA) + 1,
		offset:    0,
		blocksize: int64(c.BlockSize),
	}, nil
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

// ReadAt reads len(p) bytes from the device starting at byte offset off.  Reads
// that are not block aligned are satisfied by reading the blocks that cover the
// requested range.  It always reads len(p) bytes unless the request extends past
// the last block of the device, in which case it returns the bytes up to the end
// of the device along with io.EOF.
func (r *reader) ReadAt(p []byte, off int64) (n int, err error) {
	if r.closed.Load() {
		return 0, ErrDeviceClosed
	}

	r.waiter.Add(1)
	defer r.waiter.Done()

	deviceSize := r.blocksize * r.lba
	if off >= deviceSize {
		logger().Debug("offset past at EOF", slog.Int64("offset", off))
		return 0, io.EOF
	}
	if len(p) == 0 {
		return 0, nil
	}
	logger().Debug("ReadAt", slog.Int("bytes", len(p)), slog.Int64("offset", off))

	// clamp the request to the end of the device
	want := int64(len(p))
	if want > deviceSize-off {
		want = deviceSize - off
		err = io.EOF
		logger().Debug("reached EOF", slog.Int64("lba", r.lba), slog.Int64("bytes", want))
	}

	// the blocks covering [off, off+want), including the partial blocks at
	// either end
	startBlock := off / r.blocksize
	blockOffset := off % r.blocksize
	blocks := blocksToCover(blockOffset+want, r.blocksize)
	logger().Debug(fmt.Sprintf("offset %d into block %d", blockOffset, startBlock))

	readBytes, readErr := r.dev.Read16(Read16{
		LBA:       int(startBlock),
		BlockSize: int(r.blocksize),
		Blocks:    int(blocks),
	})
	if readErr != nil {
		return 0, fmt.Errorf("iscsi device read error: %w", readErr)
	}
	if int64(len(readBytes)) < blockOffset+want {
		// the target under-delivered.  returning what did arrive would be a
		// short read with a nil error, which io.ReaderAt forbids
		return 0, fmt.Errorf("read %d blocks at lba %d: got %d bytes, want %d: %w",
			blocks, startBlock, len(readBytes), blockOffset+want, io.ErrUnexpectedEOF)
	}

	n = copy(p[:want], readBytes[blockOffset:])
	logger().Debug("finished read", slog.Int("bytes", n))
	return n, err
}

// blocksToCover returns how many whole blocks are needed to hold n bytes,
// rounding up when the last block is only partly used.  ie. with 512 byte
// blocks, 512 bytes need 1 block and 513 bytes need 2.
func blocksToCover(n, blocksize int64) int64 {
	// integer division truncates, so adding blocksize-1 first rounds any
	// partial block up to a whole one: ceil(n/blocksize)
	return (n + blocksize - 1) / blocksize
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
