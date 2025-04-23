package iscsi_test

import (
	"bytes"
	"crypto/sha256"
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
