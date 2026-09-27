// Command mockbackend is a tiny backend used to manually verify the
// gateway: it answers /healthz with 200, echoes its name via
// X-Backend-Name, and honors ?delay= for simulating slow responses.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"
)

func main() {
	port := flag.Int("port", 9001, "listen port")
	name := flag.String("name", "backend", "backend name reported in headers/body")
	flag.Parse()

	addr := fmt.Sprintf("127.0.0.1:%d", *port)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if d := r.URL.Query().Get("delay"); d != "" {
			if ms, err := strconv.Atoi(d); err == nil {
				select {
				case <-time.After(time.Duration(ms) * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
		}
		w.Header().Set("X-Backend-Name", *name)
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, "hello from %s (%s) path=%s\n", *name, addr, r.URL.Path)
	})

	log.Printf("mock backend %q listening on %s", *name, addr)
	log.Fatal(http.ListenAndServe(addr, mux))
}
