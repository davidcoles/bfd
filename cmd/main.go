package main

import (
	"fmt"
	"log"
	"net/netip"
	"os"
	"time"

	"github.com/davidcoles/bfd"
)

func main() {
	ip := netip.MustParseAddr(os.Args[1])

	bfd := bfd.BFD{}
	err := bfd.Start()

	if err != nil {
		log.Fatal("Start: ", err)
	}

	for {
		fmt.Println(ip, bfd.IsUp(ip))
		time.Sleep(time.Second)
	}
}
