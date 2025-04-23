package iscsi_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"testing"
	"time"

	iscsi "github.com/joyent/libiscsi-go"
	"gotest.tools/assert"
)

func TestWriter_Write(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))

	// given data
	deviceSize := 4 * KiB
	dataSize := 2 * KiB
	data := make([]byte, deviceSize)
	if _, err := rnd.Read(data[:dataSize]); err != nil {
		t.Fatal(err)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, bytes.NewBuffer(data)); err != nil {
		log.Fatal(err)
	}
	wantChecksum := fmt.Sprintf("%x", hash.Sum(nil))
	t.Log("DATA CHECKSUM", wantChecksum)

	// given device
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    createAndRunTestTarget(t, int64(deviceSize)),
	})
	if err := device.Connect(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = device.Disconnect() }()

	// when
	swriter, err := iscsi.Writer(device)
	if err != nil {
		t.Fatal(err)
	}
	log.Printf("%#v", swriter)

	n, err := swriter.Write(data)
	if err != nil {
		t.Fatal(err)
	}
	if n != deviceSize {
		t.Fatalf("wrote wrong number of bytes: want=%d, got=%d", deviceSize, n)
	}

	// then
	sreader, err := iscsi.Reader(device)
	if err != nil {
		t.Fatal(err)
	}
	log.Printf("%#v", sreader)
	hash = sha256.New()
	if _, err := io.Copy(hash, sreader); err != nil {
		log.Fatal(err)
	}
	iscsiChecksum := fmt.Sprintf("%x", hash.Sum(nil))
	t.Log("ISCSI CHECKSUM ", iscsiChecksum)
	assert.Equal(t, wantChecksum, iscsiChecksum)
}

func TestWriter_WriteAt(t *testing.T) {
	var (
		deviceSize = 4 * KiB
		blockSize  = 512
	)

	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))

	type args struct {
		dataSize int
		off      int64
	}

	tests := []struct {
		name string
		args
		wantErr error
	}{
		{
			name: "lba-first-block",
			args: args{
				dataSize: 512,
				off:      0,
			},
		},
		{
			name: "lba-middle-block",
			args: args{
				dataSize: 512,
				off:      1 * KiB,
			},
		},
		{
			name: "lba-last-block",
			args: args{
				dataSize: 512,
				off:      int64(deviceSize - blockSize),
			},
		},
		{
			name: "err-unaligned",
			args: args{
				dataSize: 512,
				off:      1,
			},
			wantErr: errors.New("unaligned write: offset 1 is not divisible by block size 512"),
		},
		{
			name: "err-wrong-data-size",
			args: args{
				dataSize: 511,
				off:      0,
			},
			wantErr: errors.New("number of bytes 511 not divisible by block size 512"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// given
			data := make([]byte, test.dataSize)
			if _, err := rnd.Read(data); err != nil {
				t.Fatal(err)
			}

			hash := sha256.New()
			if _, err := io.Copy(hash, bytes.NewBuffer(data)); err != nil {
				t.Fatal(err)
			}
			wantChecksum := fmt.Sprintf("%x", hash.Sum(nil))
			t.Log("DATA CHECKSUM", wantChecksum)

			device := iscsi.New(iscsi.ConnectionDetails{
				InitiatorIQN: "iqn.2024-10.libiscsi:go",
				TargetURL:    createAndRunTestTarget(t, int64(deviceSize)),
			})
			if err := device.Connect(); err != nil {
				t.Fatal(err)
			}

			// when
			swriter, err := iscsi.Writer(device)
			if err != nil {
				t.Fatal(err)
			}
			log.Printf("%#v", swriter)
			defer swriter.Close()

			n, err := swriter.WriteAt(data, test.off)
			if test.wantErr != nil {
				assert.ErrorContains(t, err, test.wantErr.Error())
				return
			}
			assert.NilError(t, err)

			if n != test.dataSize {
				t.Fatalf("wrote wrong number of bytes: want=%d, got=%d", test.dataSize, n)
			}

			// then
			sreader, err := iscsi.Reader(device)
			if err != nil {
				t.Fatal(err)
			}
			log.Printf("%#v", sreader)
			hash = sha256.New()

			n, err = sreader.ReadAt(data, test.off)
			if err != nil && !errors.Is(err, io.EOF) {
				t.Fatal(err)
			}
			if n != len(data) {
				t.Fatalf("read wrong number of bytes: want=%d, got=%d", len(data), n)
			}

			if _, err := io.Copy(hash, bytes.NewBuffer(data)); err != nil {
				t.Fatal(err)
			}
			iscsiChecksum := fmt.Sprintf("%x", hash.Sum(nil))
			t.Log("ISCSI CHECKSUM ", iscsiChecksum)
			assert.Equal(t, wantChecksum, iscsiChecksum)
		})
	}
}

func TestWriteLoop(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)

	// given
	rnd := rand.New(rand.NewSource(seed))
	deviceSize := 10 * MiB
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    createAndRunTestTarget(t, int64(deviceSize)),
	})

	if err := device.Connect(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = device.Disconnect() }()

	// when
	swriter, err := iscsi.Writer(device)
	if err != nil {
		t.Fatal(err)
	}
	n := writeAll(t, swriter, rnd)

	// then
	if n != deviceSize {
		t.Fatalf("did not fill device: want=%d, got=%d", deviceSize, n)
	}
}

func writeAll(t *testing.T, swriter io.Writer, rnd *rand.Rand) int {
	var scsiErr error
	var written int
	for !errors.Is(scsiErr, io.EOF) {
		n := 32 * KiB
		scsiBytes := make([]byte, n)
		if _, err := rnd.Read(scsiBytes); err != nil {
			t.Fatal(err)
		}

		n, scsiErr = swriter.Write(scsiBytes)
		if scsiErr != nil && !errors.Is(scsiErr, io.EOF) {
			// something in this path causes a segfault on disconnect
			// immediately after a poll failed, but it seems like it might
			// be an issue in libiscsi (happens with 1.19, can't reproduce on 1.20)
			t.Fatal(scsiErr)
		}
		written += n
	}

	return written
}

func TestWriter_Close(t *testing.T) {
	// given
	const (
		dataSize   = 512
		deviceSize = 1 * MiB
	)

	target := iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    createAndRunTestTarget(t, int64(deviceSize)),
	}
	device := iscsi.New(target)
	if err := device.Connect(); err != nil {
		t.Fatal(err)
	}

	// when
	swriter, err := iscsi.Writer(device)
	if err != nil {
		t.Fatal(err)
	}

	// write data until the device is closed
	lastWritten := make(chan int, 1)
	errWrite := make(chan error, 1)
	go func() {
		data := make([]byte, dataSize)

		var i int
		for {
			b := []byte(fmt.Sprintf("%d", i))
			copy(data, bytes.Repeat(b, dataSize))

			n, err := swriter.WriteAt(data, 0)
			if errors.Is(err, iscsi.ErrDeviceClosed) {
				// happy path
				t.Logf("detected closed device: last wrote %d", i)
				break
			}

			if err != nil {
				errWrite <- err
				return
			}
			if n != len(data) {
				errWrite <- fmt.Errorf("wrote wrong number of bytes: want=%d, got=%d", len(data), n)
				return
			}

			i++
		}

		lastWritten <- i - 1 // the last write won't go through
	}()

	// and close after a little while
	time.Sleep(1 * time.Second)

	t.Log("closing device")
	t0 := time.Now()
	if err := swriter.Close(); err != nil {
		t.Fatal(err)
	}
	closeTime := time.Since(t0)

	t.Log("waiting for last written integer to arrive")
	last := <-lastWritten

	// then
	device = iscsi.New(target)
	if err := device.Connect(); err != nil {
		t.Fatal(err)
	}
	sreader, err := iscsi.Reader(device)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sreader.Close() })

	data := make([]byte, dataSize)
	n, err := sreader.Read(data)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if n != len(data) {
		t.Fatalf("read wrong number of bytes: want=%d, got=%d", len(data), n)
	}

	lastStr := fmt.Sprintf("%d", last)
	var start, end int
	for i := 0; i <= len(data); i += len(lastStr) {
		want := lastStr

		start = i
		end = start + len(want)
		if end >= len(data)-1 {
			end = len(data) - 1
			want = want[:end-start]
		}

		got := string(data[start:end])
		if got != want {
			t.Fatalf("value stored at off=%d does not match expected: want=%s, got=%s", i, want, got)
		}
	}

	// arbitrary-ish duration, just want to make sure this doesn't take a long time
	if closeTime > time.Second {
		t.Fatalf("device took longer than 1 second to close (%s)", closeTime)
	}
}
