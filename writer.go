package iscsi

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
)

type writer struct {
	dev       *device
	lba       int64
	blocksize int64
	offset    int64

	closed atomic.Bool
	waiter sync.WaitGroup
}

func Writer(dev *device) (*writer, error) {
	c, err := dev.ReadCapacity16()
	if err != nil {
		return nil, fmt.Errorf("failed to get capacity of device: %w", err)
	}

	return &writer{
		dev:       dev,
		lba:       int64(c.MaxLBA) + 1,
		blocksize: int64(c.BlockSize),
	}, nil
}

func (w *writer) Close() error {
	w.closed.Store(true)
	w.waiter.Wait()
	return w.dev.Disconnect()
}

func (w *writer) Write(p []byte) (n int, err error) {
	writeLen, err := w.WriteAt(p, w.offset)
	w.offset += int64(writeLen)
	return writeLen, err
}

func (w *writer) WriteAt(p []byte, off int64) (n int, err error) {
	if w.closed.Load() {
		return 0, ErrDeviceClosed
	}

	w.waiter.Add(1)
	defer w.waiter.Done()

	size := int64(len(p))
	endOffset := off + size
	if endOffset > w.blocksize*w.lba {
		return 0, fmt.Errorf("%w: final offset %d past max offset %d",
			io.EOF, endOffset, w.blocksize*w.lba,
		)
	}
	if len(p)%int(w.blocksize) != 0 {
		return 0, fmt.Errorf("number of bytes %d not divisible by block size %d",
			len(p), w.blocksize,
		)
	}

	logger().Debug("WriteAt", slog.Int("bytes", len(p)), slog.Int("offset", int(off)))

	writeErr := w.dev.Write16(Write16{
		LBA:       int(off / w.blocksize),
		BlockSize: int(w.blocksize),
		Data:      p,
	})
	if writeErr != nil {
		return 0, fmt.Errorf("iscsi device write error: %w", writeErr)
	}

	logger().Debug("finished write", slog.Int("length", len(p)))
	return len(p), err
}

func (w *writer) Seek(offset int64, whence int) (int64, error) {
	if w.closed.Load() {
		return 0, ErrDeviceClosed
	}

	w.waiter.Add(1)
	defer w.waiter.Done()

	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = w.offset + offset
	case io.SeekEnd:
		abs = w.lba*w.blocksize + offset
	default:
		return 0, errors.New("iscsi.Writer.Seek: invalid whence")
	}
	if abs < 0 {
		return 0, errors.New("iscsi.Writer.Seek: negative position")
	}
	w.offset = abs
	return abs, nil
}
