package main

import (
	"log"
	"os"

	iscsi "github.com/joyent/libiscsi-go"
	"github.com/sanity-io/litter"
)

func main() {
	if len(os.Args) != 3 {
		panic("missing required args")
	}
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: os.Args[1],
		TargetURL:    os.Args[2],
	})

	err := device.Connect()
	if err != nil {
		log.Fatalln(err)
	}

	defer func() {
		_ = device.Disconnect()
	}()

	capacity, err := device.ReadCapacity10()
	if err != nil {
		log.Fatalln(err)
	}

	data := []byte("hello iscsi")
	// TODO: (willgorman) handle data > block size or just let it truncate?
	if len(data) < capacity.BlockSize {
		dataCopy := make([]byte, capacity.BlockSize)
		copy(dataCopy, data)
		data = dataCopy
	}

	litter.Dump(string(data))

	err = device.Write16(iscsi.Write16{
		LBA:       0,
		Data:      data,
		BlockSize: capacity.BlockSize,
	})
	if err != nil {
		log.Fatalln(err)
	}

	dataread, err := device.Read16(iscsi.Read16{
		LBA:       0,
		Blocks:    1,
		BlockSize: 512,
	})
	if err != nil {
		log.Fatalln(err)
	}
	litter.Dump("hey!", string(dataread))
}
