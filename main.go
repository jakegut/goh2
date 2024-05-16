package main

import (
	"fmt"
	"log"
	"net"
	"net/http"

	"github.com/jakegut/goh2/http2"
	gohttp2 "golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func exampleServer() {
	h2 := &gohttp2.Server{}

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "Hello, %v, http: %v", r.URL.Path, r.TLS == nil)

	})

	server := &http.Server{
		Addr:    "0.0.0.0:1010",
		Handler: h2c.NewHandler(handler, h2),
	}

	go server.ListenAndServe()
}

func main() {
	listener, err := net.Listen("tcp4", ":8080")
	if err != nil {
		log.Fatal(err)
	}
	defer listener.Close()

	exampleServer()

	log.Printf("listening on 8080")

	for {
		conn, err := listener.Accept()
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("accepted from %s", conn.RemoteAddr().String())
		c := &http2.Connection{Conn: conn, Handler: func(w http.ResponseWriter, r http2.Request) {
			fmt.Fprintf(w, "Hello, %v, method: %v", r.Authority, r.Method)
			for {
				fmt.Fprintf(w, "hello weee")
			}
		}}
		go c.Handle()
	}
}
