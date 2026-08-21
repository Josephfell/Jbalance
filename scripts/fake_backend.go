// Command fake_backend is a tiny helper for manual testing: it starts a
// plain HTTP server on the given port that responds with its own port
// number, so you can see requests being distributed across backends when
// hitting the data plane proxy.
//
// Not part of the main build — run directly with `go run`.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"time"
)

func main() {
	port := flag.String("port", "8081", "port to listen on")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if _, err := fmt.Fprintf(w, "hello from backend on port %s\n", *port); err != nil {
			log.Printf("fake_backend: failed to write response: %v", err)
		}
	})

	addr := ":" + *port
	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("fake_backend: listening on %s", addr)
	log.Fatal(server.ListenAndServe())
}
