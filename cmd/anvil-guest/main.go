//go:build linux

// anvil-guest runs inside an anvil VM and runs commands the host sends over vsock, one per
// connection (internal/guestexec). Built static, shipped on a read-only drive.
package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"syscall"
	"time"

	"git.fferrando.cc/rdfilippo/anvil/internal/guestexec"
	"git.fferrando.cc/rdfilippo/anvil/internal/version"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "reap" {
		argv := os.Args[2:]
		if len(argv) > 0 && argv[0] == "--" {
			argv = argv[1:]
		}
		os.Exit(guestexec.Reap(argv))
	}
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
		if err != nil && temporary(err) {
			log.Printf("anvil-guest: accept: %v (trying again)", err)
			time.Sleep(100 * time.Millisecond)
			continue
		}
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

// temporary reports whether an accept error can pass, like running out of fds for a moment
// or a connection the host dropped before it was accepted.
func temporary(err error) bool {
	for _, e := range []syscall.Errno{syscall.EMFILE, syscall.ENFILE, syscall.ENOBUFS, syscall.ENOMEM,
		syscall.ECONNABORTED, syscall.EPROTO, syscall.EAGAIN, syscall.EPERM} {
		if errors.Is(err, e) {
			return true
		}
	}
	return false
}
