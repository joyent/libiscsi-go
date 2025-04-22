package iscsi_test

import (
	"context"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"sync"
	"testing"
	"time"

	iscsi "github.com/joyent/libiscsi-go"
	"gotest.tools/assert"
)

type delayWriter struct {
	io.Writer
	delay time.Duration
}

func (d *delayWriter) Write(p []byte) (n int, err error) {
	time.Sleep(d.delay)
	return d.Writer.Write(p)
}

func BenchmarkSingleAsyncReaderWithParallelConsumers(b *testing.B) {
	// parameters
	// size of the iscsi lun
	deviceSize := 100 * MiB
	// number of concurrent iscsi sessions
	nconsumers := 20
	// how long for each consumer of the reader to wait after each read
	consumerDelay := 100 * time.Millisecond
	// after this many reads are queued, start polling to drive data through
	maxQueue := 20
	// stop polling and resume reading once the queue drops to this length
	minQueue := 4

	// TODO: option for running against a remote iscsi target instead of file backed local target

	iscsi.SetLogger(slog.Default())
	seed := time.Now().UnixNano()
	b.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(b, rnd, int64(deviceSize))
	file, err := os.Open(fileName)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: "iqn.2024-10.libiscsi:go",
		TargetURL:    runTestTarget(b, fileName),
	})

	err = device.Connect()
	if err != nil {
		b.Fatal(err)
	}
	defer func() {
		_ = device.Disconnect()
	}()

	for i := 0; i < b.N; i++ {
		output := make(chan iscsi.TaskResult, 20)
		totalBlocks := deviceSize / 512
		// read 1MiB at a time
		blockChunk := MiB / 512
		wait := sync.WaitGroup{}
		wait.Add(totalBlocks / blockChunk)
		for i := 0; i < nconsumers; i++ {
			go func() {
				for r := range output {
					if r.Err != nil {
						b.Fail()
						b.Log(err)
					}
					assert.Assert(b, len(r.Task.DataIn) > 0)
					// simulate a slow consumer
					time.Sleep(consumerDelay)
					wait.Done()
				}
			}()
		}
		for i := 0; i < totalBlocks; i = i + blockChunk {
			err := device.Read16Async(iscsi.Read16{
				LBA:       i,
				Blocks:    blockChunk,
				BlockSize: 512,
			}, output)
			if err != nil {
				b.Fatal(err)
			}

			// once there are enough reads queued up,
			// take a break from reading to process
			// them and feed output to the consumers
			if device.GetQueueLength() > maxQueue {
				for device.GetQueueLength() > minQueue {
					_ = device.ProcessAsyncN(1)
				}
			}
		}

		// handle any remaining requests in the queue
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		go func() {
			wait.Wait()
			// once all consumers are done, halt ProcessAsync
			cancel()
		}()
		err = device.ProcessAsync(ctx)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// This benchmark creates N iscsi sessions to a single lun
// where each session will only read 1/Nth of the lun.  Each
// reader sends the data to a consumer that will pause
// for some time to simulate something like a writer that may
// be slower than the reader
func BenchmarkParallelSyncReaders(b *testing.B) {
	// parameters
	// size of the iscsi lun
	deviceSize := 100 * MiB
	// number of concurrent iscsi sessions
	// MUST BE A FACTOR OF THE BLOCK SIZE
	nreaders := 16
	// block size of the lun
	blockSize := 512
	// how long for each consumer of the reader to wait after each read
	consumerDelay := 100 * time.Millisecond

	// TODO: option for running against a remote iscsi target instead of file backed local target

	iscsi.SetLogger(slog.Default())
	seed := time.Now().UnixNano()
	b.Logf("using seed %d", seed)
	rnd := rand.New(rand.NewSource(seed))
	fileName := writeTargetTempfile(b, rnd, int64(deviceSize))
	file, err := os.Open(fileName)
	if err != nil {
		b.Fatal(err)
	}
	defer file.Close()

	url := runTestTarget(b, fileName)
	// iscsi.SetLogger(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelDebug})))

	blocks := deviceSize / blockSize

	var sectionReaders []io.Reader
	for i := 0; i < blocks; i = i + (blocks / nreaders) {
		device := iscsi.New(iscsi.ConnectionDetails{
			InitiatorIQN: "iqn.2024-10.libiscsi:go",
			TargetURL:    url,
		})

		err = device.Connect()
		if err != nil {
			b.Fail()
			b.Log(err)
		}
		defer func() {
			_ = device.Disconnect()
		}()

		cap, err := device.ReadCapacity16()
		if err != nil {
			b.Fail()
			b.Log(err)
		}

		reader, err := iscsi.Reader(device)
		if err != nil {
			b.Fail()
			b.Log(err)
		}
		start := i * blockSize
		readLen := ((cap.MaxLBA + 1) * blockSize) / nreaders
		rdr := io.NewSectionReader(reader, int64(start), int64(readLen))
		b.Logf("Created section reader starting at %d, reading %d bytes", start, readLen)
		sectionReaders = append(sectionReaders, rdr)
	}
	for i := 0; i < b.N; i++ {

		w := sync.WaitGroup{}
		w.Add(len(sectionReaders))
		for _, rdr := range sectionReaders {
			go func() {
				wtr := delayWriter{io.Discard, consumerDelay}
				buf := make([]byte, MiB)
				for j := 0; j < deviceSize/len(sectionReaders)/len(buf); j++ {
					_, err := rdr.Read(buf)
					if err != nil && err != io.EOF {
						b.Fail()
						b.Log("read err ", err)
					}
					if err == io.EOF {
						break
					}
					_, err = wtr.Write(buf)
					if err != nil {
						b.Fail()
						b.Log("write err ", err)
					}
				}
				w.Done()
			}()
		}
		// for i := 0; i < blocks; i = i + (blocks / nreaders) {
		// 	go func(so int) {
		// 		device := iscsi.New(iscsi.ConnectionDetails{
		// 			InitiatorIQN: "iqn.2024-10.libiscsi:go",
		// 			TargetURL:    url,
		// 		})

		// 		err = device.Connect()
		// 		if err != nil {
		// 			b.Fail()
		// 			b.Log(err)
		// 		}
		// 		defer func() {
		// 			_ = device.Disconnect()
		// 		}()

		// 		cap, err := device.ReadCapacity16()
		// 		if err != nil {
		// 			b.Fail()
		// 			b.Log(err)
		// 		}

		// 		reader, err := iscsi.Reader(device)
		// 		if err != nil {
		// 			b.Fail()
		// 			b.Log(err)
		// 		}
		// 		start := so * blockSize
		// 		readLen := (cap.LBA * blockSize) / nreaders
		// 		rdr := io.NewSectionReader(reader, int64(start), int64(readLen))
		// 		b.Logf("Starting at %d, reading to %d", start, readLen)
		// 		wtr := delayWriter{io.Discard, consumerDelay}
		// 		buf := make([]byte, MiB)
		// 		for j := 0; j < deviceSize/len(buf); j++ {
		// 			_, err := rdr.Read(buf)
		// 			if err != nil && err != io.EOF {
		// 				b.Fail()
		// 				b.Log("read err ", err)
		// 			}
		// 			if err == io.EOF {
		// 				break
		// 			}
		// 			_, err = wtr.Write(buf)
		// 			if err != nil {
		// 				b.Fail()
		// 				b.Log("write err ", err)
		// 			}
		// 		}
		// 		w.Done()
		// 	}(i)
		// }
		w.Wait()
	}
}
