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

	iscsi "github.com/willgorman/libiscsi-go"
	"gotest.tools/assert"
)

func TestWrite(t *testing.T) {
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
		t.Fatalf("wrong number of bytes written: want=%d, got=%d", deviceSize, n)
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
