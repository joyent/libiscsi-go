package iscsi_test

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"os"
	"testing"
	"time"

	iscsi "github.com/joyent/libiscsi-go"
	"gotest.tools/assert"
)

// the block size gotgt always reports (api.DefaultBlockShift)
const testBlockSize = 512

type testReader interface {
	io.Reader
	io.ReaderAt
	io.Seeker
	io.Closer
}

// openTestReader runs a target backed by fileName and returns a reader for it.
// the device is disconnected when the test finishes.
func openTestReader(t *testing.T, fileName string) testReader {
	t.Helper()
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    runTestTarget(t, fileName),
	})
	if err := device.Connect(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = device.Disconnect() })

	sreader, err := iscsi.Reader(device)
	if err != nil {
		t.Fatal(err)
	}
	return sreader
}

func TestReadAt(t *testing.T) {
	const deviceSize = 4 * KiB // 8 blocks

	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, deviceSize)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	sreader := openTestReader(t, fileName)

	testCases := []struct {
		desc    string
		off     int64
		length  int
		wantN   int
		wantEOF bool
	}{
		{desc: "aligned within one block", off: 0, length: 100, wantN: 100},
		{desc: "unaligned within one block", off: 100, length: 100, wantN: 100},
		{desc: "unaligned ending on a block boundary", off: 412, length: 100, wantN: 100},
		{desc: "unaligned crossing a block boundary", off: 412, length: 200, wantN: 200},
		{desc: "unaligned with an aligned end offset", off: 412, length: 612, wantN: 612},
		{desc: "aligned across multiple blocks", off: 512, length: 1536, wantN: 1536},
		{desc: "unaligned across multiple blocks", off: 500, length: 1600, wantN: 1600},
		{desc: "the last block", off: deviceSize - testBlockSize, length: 512, wantN: 512},
		{desc: "past the last block", off: deviceSize - testBlockSize, length: 1024, wantN: 512, wantEOF: true},
		{desc: "unaligned past the last block", off: deviceSize - 100, length: 512, wantN: 100, wantEOF: true},
		{desc: "at the end of the device", off: deviceSize, length: 512, wantEOF: true},
		{desc: "past the end of the device", off: 2 * deviceSize, length: 512, wantEOF: true},
		{desc: "empty buffer", off: 0, length: 0},
		{desc: "empty buffer at an unaligned offset", off: 412, length: 0},
	}
	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			got := make([]byte, tC.length)
			n, err := sreader.ReadAt(got, tC.off)
			if tC.wantEOF {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("want io.EOF, got %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, tC.wantN, n)

			want := make([]byte, tC.wantN)
			if tC.wantN > 0 {
				if _, err := file.ReadAt(want, tC.off); err != nil {
					t.Fatal(err)
				}
			}
			assert.Assert(t, bytes.Equal(want, got[:n]))
		})
	}
}

// TestReadAtSweep reads every offset of the device at a handful of lengths to
// cover the block boundary conditions exhaustively.
func TestReadAtSweep(t *testing.T) {
	const deviceSize = 4 * KiB

	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, deviceSize)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	sreader := openTestReader(t, fileName)

	for _, length := range []int{1, 100, testBlockSize - 1, testBlockSize, testBlockSize + 1, 1000} {
		for off := int64(0); off < deviceSize; off++ {
			wantN := min(int64(length), deviceSize-off)

			got := make([]byte, length)
			n, err := sreader.ReadAt(got, off)
			if wantN < int64(length) {
				if !errors.Is(err, io.EOF) {
					t.Fatalf("ReadAt(%d bytes, %d): want io.EOF, got %v", length, off, err)
				}
			} else if err != nil {
				t.Fatalf("ReadAt(%d bytes, %d): %v", length, off, err)
			}
			if int64(n) != wantN {
				t.Fatalf("ReadAt(%d bytes, %d): read %d bytes, want %d", length, off, n, wantN)
			}

			want := make([]byte, wantN)
			if _, err := file.ReadAt(want, off); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(want, got[:n]) {
				t.Fatalf("ReadAt(%d bytes, %d): data mismatch", length, off)
			}
		}
	}
}

// TestReadSequentialUnaligned drives Read with buffer sizes that leave the
// reader at an unaligned offset, where reads that span an extra block used to
// come back short.
func TestReadSequentialUnaligned(t *testing.T) {
	const deviceSize = 64 * KiB

	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, deviceSize)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	sreader := openTestReader(t, fileName)

	// 700%512 == 188, so all but the first read starts at an unaligned offset
	sizes := []int{700, 700, 700, 300, 512, 513, 1, 511, 1000}
	var total int64
	for i := 0; ; i++ {
		if total > deviceSize {
			t.Fatalf("read %d bytes from a %d byte device", total, int64(deviceSize))
		}
		size := sizes[i%len(sizes)]
		got := make([]byte, size)
		n, err := sreader.Read(got)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("read %d bytes at offset %d: %v", size, total, err)
		}

		want := make([]byte, n)
		if _, err := file.ReadAt(want, total); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(want, got[:n]) {
			t.Fatalf("data mismatch at offset %d", total)
		}

		if errors.Is(err, io.EOF) {
			total += int64(n)
			break
		}
		if n != size {
			t.Fatalf("read %d bytes at offset %d, want %d", n, total, size)
		}
		total += int64(n)
	}
	if total != deviceSize {
		t.Fatalf("read %d bytes, want %d", total, int64(deviceSize))
	}
}

func TestRead(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, 4*KiB)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		log.Fatal(err)
	}
	fileChecksum := fmt.Sprintf("%x", hash.Sum(nil))
	t.Log("FILE CHECKSUM", fileChecksum)

	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    runTestTarget(t, fileName),
	})

	err = device.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = device.Disconnect()
	}()

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
	assert.Equal(t, fileChecksum, iscsiChecksum)
}

// in order to test non block aligned reads
// we can have a file io.Reader and iscsi io.Reader and
// read randomly sized []byte from them and assert that we always get
// the same values from each
func TestReadRandom(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, 10*MiB)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    runTestTarget(t, fileName),
	})

	err = device.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = device.Disconnect()
	}()

	sreader, err := iscsi.Reader(device)
	if err != nil {
		t.Fatal(err)
	}

	var fileErr, scsiErr error
	var fileN, scsiN int
	for fileErr != io.EOF && scsiErr != io.EOF {
		n := rnd.Intn(32 * KiB)
		fileBytes := make([]byte, n)
		scsiBytes := make([]byte, n)
		fileN, fileErr = file.Read(fileBytes)
		if fileErr != nil && fileErr != io.EOF {
			t.Fatal(fileErr)
		}

		scsiN, scsiErr = sreader.Read(scsiBytes)

		if scsiErr != nil && scsiErr != io.EOF {
			t.Fatal(scsiErr)
		}
		if scsiErr == nil && scsiN != n {
			t.Fatalf("read %d bytes, want %d", scsiN, n)
		}
		assert.Equal(t, fileN, scsiN)
		assert.Assert(t, bytes.Equal(fileBytes, scsiBytes))
	}
}

func TestReadLoop(t *testing.T) {
	// seed := time.Now().UnixNano()
	seed := int64(1732045254519287895)
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, 10*MiB)
	targetURL := runTestTarget(t, fileName)
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    targetURL,
	})

	err := device.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = device.Disconnect()
	}()

	for i := 0; i < 1000; i++ {
		t.Log("LOOP ", i)
		sreader, err := iscsi.Reader(device)
		if err != nil {
			t.Fatal(err)
		}
		readAll(t, sreader, rnd)
	}
}

func readAll(t *testing.T, sreader io.Reader, rnd *rand.Rand) {
	var scsiErr error
	for scsiErr != io.EOF {
		n := rnd.Intn(32 * KiB)
		scsiBytes := make([]byte, n)
		_, scsiErr = sreader.Read(scsiBytes)
		if scsiErr != nil && scsiErr != io.EOF {
			// something in this path causes a segfault on disconnect
			// immediately after a poll failed, but it seems like it might
			// be an issue in libiscsi (happens with 1.19, can't reproduce on 1.20)
			t.Fatal(scsiErr)
		}

	}
}

func TestSectionRead(t *testing.T) {
	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, 4*KiB)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    runTestTarget(t, fileName),
	})

	err = device.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = device.Disconnect()
	}()

	cap, err := device.ReadCapacity16()
	if err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 2*cap.BlockSize)
	_, err = file.ReadAt(data, int64(2*cap.BlockSize))
	if err != nil {
		t.Fatal(err)
	}

	hash := sha256.New()
	if _, err := io.Copy(hash, bytes.NewBuffer(data)); err != nil {
		log.Fatal(err)
	}
	sectionChecksum := fmt.Sprintf("%x", hash.Sum(nil))
	t.Log("SECTION CHECKSUM", sectionChecksum)

	reader, err := iscsi.Reader(device)
	if err != nil {
		t.Fatal(err)
	}

	sreader := io.NewSectionReader(reader, int64(2*cap.BlockSize), int64(2*cap.BlockSize))
	log.Printf("%#v", sreader)
	hash = sha256.New()
	if _, err := io.Copy(hash, sreader); err != nil {
		log.Fatal(err)
	}
	iscsiChecksum := fmt.Sprintf("%x", hash.Sum(nil))
	t.Log("ISCSI CHECKSUM ", iscsiChecksum)
	assert.Equal(t, sectionChecksum, iscsiChecksum)
}

func TestReader_Close(t *testing.T) {
	// given
	seed := time.Now().UnixNano()
	t.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(t, rnd, 4*KiB)
	file, err := os.Open(fileName)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    runTestTarget(t, fileName),
	})

	err = device.Connect()
	if err != nil {
		t.Fatal(err)
	}

	// when
	sreader, err := iscsi.Reader(device)
	if err != nil {
		t.Fatal(err)
	}

	errRead := make(chan error, 1)
	go func() {
		data := make([]byte, 512)

		for {
			n, err := sreader.ReadAt(data, 0)
			if errors.Is(err, iscsi.ErrDeviceClosed) {
				// happy path
				t.Logf("detected closed device: exiting")
				break
			}

			if err != nil {
				errRead <- err
				return
			}
			if n != len(data) {
				errRead <- fmt.Errorf("read wrong number of bytes: want=%d, got=%d", len(data), n)
				return
			}
		}
	}()

	// and close after a little while
	time.Sleep(1 * time.Second)

	t.Log("closing device")
	t0 := time.Now()
	if err := sreader.Close(); err != nil {
		t.Fatal(err)
	}
	closeTime := time.Since(t0)

	// arbitrary-ish duration, just want to make sure this doesn't take a long time
	if closeTime > time.Second {
		t.Fatalf("device took longer than 1 second to close (%s)", closeTime)
	}
}
