//go:build ignore

// serve serves a directory over HTTP, for the L3 trial test's releases.json.
//
//	go run tests/vm/serve.go -addr 127.0.0.1:8088 -dir DIR
package main

import (
	"flag"
	"log"
	"net/http"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8088", "listen address")
	dir := flag.String("dir", ".", "directory to serve")
	flag.Parse()
	log.Fatal(http.ListenAndServe(*addr, http.FileServer(http.Dir(*dir))))
}
