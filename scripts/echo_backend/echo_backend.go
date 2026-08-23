// Command echo_backend is a tiny manual-testing helper for the L4/TCP
// data plane: a plain TCP server that echoes each line it receives back
// prefixed with its own port, so you can see which backend a raw TCP
// connection actually reached when proxied through the L4 data plane.
//
// Not part of the main build — run directly with `go run`.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"log"
	"net"
)

func main() {
	port := flag.String("port", "7001", "port to listen on")
	flag.Parse()

	ln, err := net.Listen("tcp", ":"+*port)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("echo_backend: listening on :%s", *port)
	for {
		conn, err := ln.Accept()
		if err != nil {
			continue
		}
		go func(c net.Conn) {
			defer func() { _ = c.Close() }()
			scanner := bufio.NewScanner(c)
			for scanner.Scan() {
				if _, err := fmt.Fprintf(c, "backend-%s:%s\n", *port, scanner.Text()); err != nil {
					return
				}
			}
		}(conn)
	}
}
