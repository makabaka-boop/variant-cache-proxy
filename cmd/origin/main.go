// Command origin runs the controllable origin server.
//
// Environment:
//
//	PORT  listen port (default 9000)
package main

import (
	"log"
	"net/http"
	"os"

	"assetproxy/internal/origin"
)

func main() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "9000"
	}
	log.Printf("origin listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, origin.New().Handler()))
}
