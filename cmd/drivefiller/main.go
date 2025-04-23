package main

import (
	"crypto/rand"
	"log"
	"os"
	"strconv"

	iscsi "github.com/joyent/libiscsi-go"
)

func main() {
	if len(os.Args) != 4 {
		panic("missing required args")
	}
	device := iscsi.New(iscsi.ConnectionDetails{
		InitiatorIQN: os.Args[1],
		TargetURL:    os.Args[2],
	})

	percentage, err := strconv.Atoi(os.Args[3])
	if err != nil {
		panic(err)
	}

	if percentage < 1 || percentage > 100 {
		panic("percentage must be between 1 and 100")
	}

	err = device.Connect()
	if err != nil {
		log.Fatalln(err)
	}
	log.Println("connected")

	defer func() {
		_ = device.Disconnect()
	}()

	capacity, err := device.ReadCapacity10()
	if err != nil {
		log.Fatalln(err)
	}

	// TODO: (willgorman) what's max throughput here?  can we split up and run chunks in parallel?
	log.Println(percentage)
	log.Println(float64(percentage) / 100.0)
	blocksToWrite := int(float64(float64(capacity.MaxLBA)) * float64(percentage) / 100.0)
	log.Println(blocksToWrite)
	currentBlock := 0
	for currentBlock < blocksToWrite {
		// fixme: handle case where blocksToWrite - currentBlock < 1024
		data := make([]byte, 1024*capacity.BlockSize)
		_, err := rand.Read(data)
		if err != nil {
			panic(err)
		}
		err = device.Write16(iscsi.Write16{
			LBA:       currentBlock,
			Data:      data,
			BlockSize: capacity.BlockSize,
		})
		if err != nil {
			log.Fatalln(err)
		}
		log.Printf("wrote 1024 blocks starting at %d", currentBlock)
		// write 1024 blocks at once?
		currentBlock = currentBlock + 1024
	}
}
