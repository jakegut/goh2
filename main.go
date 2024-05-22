package main

import (
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

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
		c := &http2.Connection{
			Conn: conn,
			Handler: func(w http.ResponseWriter, r http2.Request) {
				time.Sleep(time.Second)
				fmt.Fprintf(w, "Hello, %v, method: %v\n", r.Authority, r.Method)

				if r.Method == "POST" {
					hash := sha256.New()
					if _, err := io.Copy(hash, r.Body); err != nil {
						log.Fatal(err)
					}
					sum := hash.Sum(nil)
					fmt.Fprintf(w, "1 sum: %x\n", sum)

				}

				// bs, err := io.ReadAll(r.Body)
				// if err != nil {
				// 	log.Printf("error reading body: %s", err)
				// }

				// if len(bs) > 0 {
				// 	fmt.Fprintf(w, "received data %d bytes long\n", len(bs))
				// 	sum := sha256.Sum256(bs)
				// 	fmt.Fprintf(w, "2 sum: %x\n", sum)
				// }
			}}
		go c.Handle()
	}
}
