# goh2

A toy HTTP/2 server implementation written in Go

## usage, so far

Example:
```go
listener, err := net.Listen("tcp4", ":8080")
if err != nil {
    log.Fatal(err)
}
defer listener.Close()
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
            fmt.Fprintf(w, "Hello, %v, method: %v", r.Authority, r.Method)
        }}
    go c.Handle()
}
```

This will allow you to send requests from cURL with prio knowledge:

```sh
> curl --http2-prior-knowledge localhost:8080
Hello, localhost:8080, method: GET
```

## TODO

- [ ] Support `POST` requests (decoding Data frames)
- [ ] Better HTTP/1.1 Upgrade support (currently does handshake, but doesn't create `0x1` stream)
- [ ] Error handling
- [ ] Support flow control
- [ ] Support stream priotization
- [ ] Implement API for sending `PUSH_PROMISE` frames
- [ ] Implement Listener API