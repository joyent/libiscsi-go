package iscsi_test

import (
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gostor/gotgt/pkg/config"
	_ "github.com/gostor/gotgt/pkg/port/iscsit"
	"github.com/gostor/gotgt/pkg/scsi"
	_ "github.com/gostor/gotgt/pkg/scsi/backingstore"
	"github.com/hashicorp/consul/sdk/freeport"
	iscsi "github.com/joyent/libiscsi-go"
	"gotest.tools/assert"
)

const (
	_ = 1 << (10 * iota)
	KiB
	MiB
	GiB
	TiB
)

var deviceID atomic.Int32

func init() {
	deviceID.Add(1000)
}

func createTargetTempfile(t testing.TB, size int64) string {
	file, err := os.CreateTemp("", strings.ReplaceAll(t.Name(), "/", "_"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	t.Cleanup(func() { _ = os.Remove(file.Name()) })
	if size > 0 {
		err = file.Truncate(size)
		if err != nil {
			t.Fatal(err)
		}
	}

	return file.Name()
}

func writeTargetTempfile(t testing.TB, rnd *rand.Rand, size int64) string {
	fileName := createTargetTempfile(t, 0)
	file, err := os.OpenFile(fileName, os.O_APPEND|os.O_WRONLY, os.ModeAppend)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// it'd be better have to a reader than can generate random data on the fly
	// instead of allocating the whole thing
	data := make([]byte, size)

	if _, err := rnd.Read(data); err != nil {
		t.Fatal(err)
	}

	if _, err := file.Write(data); err != nil {
		t.Fatal(err)
	}

	return fileName
}

// createAndRunTestTarget creates a sparse file backed iscsi target of the given
// size and runs an iscsi server on a random ephmeral port.  The url
// returned can be used by iscsi.ConnectionDetails
func createAndRunTestTarget(t testing.TB, size int64) (url string) {
	imgFile := createTargetTempfile(t, size)
	return runTestTarget(t, imgFile)
}

func runTestTarget(t testing.TB, targetFile string) (url string) {
	port := freeport.GetOne(t)
	targetIQN := "iqn.2024-10.com.example:0:0"
	c := &config.Config{
		Storages: []config.BackendStorage{
			{
				DeviceID:         uint64(deviceID.Add(1)),
				Path:             fmt.Sprintf("file:%s", targetFile),
				Online:           true,
				ThinProvisioning: true,
			},
		},
		ISCSIPortals: []config.ISCSIPortalInfo{
			{ID: 0, Portal: fmt.Sprintf("127.0.0.1:%d", port)},
		},
		ISCSITargets: map[string]config.ISCSITarget{
			targetIQN: {
				TPGTs: map[string][]uint64{
					"1": {0},
				},
				LUNs: map[string]uint64{
					"0": uint64(deviceID.Load()),
				},
			},
		},
	}
	err := scsi.InitSCSILUMap(c)
	if err != nil {
		t.Fatal(err)
	}
	tgtsvc := scsi.NewSCSITargetService()
	targetDriver, err := scsi.NewTargetDriver("iscsi", tgtsvc)
	if err != nil {
		t.Fatal(err)
	}
	for tgtname := range c.ISCSITargets {
		err = targetDriver.NewTarget(tgtname, c)
		if err != nil {
			t.Fatal(err)
		}
	}
	go targetDriver.Run(port)
	t.Cleanup(func() { _ = targetDriver.Close() })
	return fmt.Sprintf("iscsi://127.0.0.1:%d/%s/0", port, targetIQN)
}

func TestWithGoTGT(t *testing.T) {
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    createAndRunTestTarget(t, 10*MiB),
	})
	err := device.Connect()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = device.Disconnect()
	}()
	capacity, err := device.ReadCapacity10()
	if err != nil {
		t.Fatal(err)
	}

	write := make([]byte, capacity.BlockSize)
	copy(write, []byte("hello!"))

	err = device.Write16(iscsi.Write16{LBA: 0, Data: write, BlockSize: capacity.BlockSize})
	if err != nil {
		t.Fatal(err)
	}

	data, err := device.Read16(iscsi.Read16{LBA: 0, Blocks: 1, BlockSize: capacity.BlockSize})
	if err != nil {
		t.Fatal(err)
	}
	t.Log("data", string(data))
}

func TestReadCapacity16(t *testing.T) {
	testCases := []struct {
		desc     string
		size     uint64
		expected iscsi.Capacity
	}{
		{
			desc: "1 MiB",
			size: 1 * MiB,
			expected: iscsi.Capacity{
				MaxLBA:    (1 * MiB / 512) - 1,
				BlockSize: 512,
			},
		},
		{
			desc: "3 TiB",
			size: 3 * TiB,
			expected: iscsi.Capacity{
				MaxLBA:    (3 * TiB / 512) - 1,
				BlockSize: 512,
			},
		},
	}
	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			device := iscsi.New(iscsi.ConnectionDetails{
				InitiatorIQN: "iqn.2024-10.libiscsi:go",
				TargetURL:    createAndRunTestTarget(t, int64(tC.size)),
			})
			err := device.Connect()
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
			assert.Equal(t, cap, tC.expected)
		})
	}
}

func TestReadCapacity10(t *testing.T) {
	testCases := []struct {
		desc     string
		size     uint64
		expected iscsi.Capacity
	}{
		{
			desc: "1 MiB",
			size: 1 * MiB,
			expected: iscsi.Capacity{
				MaxLBA:    (1 * MiB / 512) - 1,
				BlockSize: 512,
			},
		},
		{
			desc: "3 TiB",
			size: 3 * TiB,
			expected: iscsi.Capacity{
				MaxLBA:    (2 * TiB / 512) - 1,
				BlockSize: 512,
			},
		},
	}
	for _, tC := range testCases {
		t.Run(tC.desc, func(t *testing.T) {
			device := iscsi.New(iscsi.ConnectionDetails{
				InitiatorIQN: "iqn.2024-10.libiscsi:go",
				TargetURL:    createAndRunTestTarget(t, int64(tC.size)),
			})
			err := device.Connect()
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = device.Disconnect()
			}()

			cap, err := device.ReadCapacity10()
			if err != nil {
				t.Fatal(err)
			}
			assert.Equal(t, cap, tC.expected)
		})
	}
}
