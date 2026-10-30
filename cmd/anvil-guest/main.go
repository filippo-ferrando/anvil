//go:build linux

// anvil-guest runs inside an anvil VM and runs commands the host sends over vsock, one per
// connection (internal/guestexec). Built static, shipped on a read-only drive.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
)

func main() {
	port := flag.Uint("port", guestexec.Port, "vsock port to listen on")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	v, _ := version.Get()
	if *showVersion {
		fmt.Println(v)
		return
	}
	log.SetFlags(0)
	l, err := guestexec.ListenVsock(uint32(*port))
	if err != nil {
		log.Fatalf("anvil-guest: %v", err)
	}
	log.Printf("anvil-guest %s listening on vsock port %d", v, *port)
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Printf("anvil-guest: accept: %v", err)
			os.Exit(1)
		}
		go func() {
			if err := guestexec.Serve(conn); err != nil {
				log.Printf("anvil-guest: %v", err)
			}
		}()
	}
}
