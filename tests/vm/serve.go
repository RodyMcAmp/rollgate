//go:build ignore

// serve serves a directory over HTTP, for the L3 trial test's releases.json,
// or over HTTPS with -cert and -key, for the blocklist test's lists.
//
//	go run tests/vm/serve.go -addr 127.0.0.1:8088 -dir DIR [-cert C -key K]
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	cert := flag.String("cert", "", "with -key: serve HTTPS with this certificate")
	key := flag.String("key", "", "the certificate's key")
	flag.Parse()
	h := http.FileServer(http.Dir(*dir))
	if *cert != "" {
		log.Fatal(http.ListenAndServeTLS(*addr, *cert, *key, h))
	}
	log.Fatal(http.ListenAndServe(*addr, h))
}
