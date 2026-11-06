package main

import (
	"net"
	"net/http"
	"testing"
)

func TestHealthcheck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = server.Serve(listener) }()

	t.Setenv("ROAST_BROKER_LISTEN_ADDRESS", listener.Addr().String())
	if err := healthcheck(); err != nil {
		t.Fatalf("healthcheck: %v", err)
	}
}

func TestHealthcheckRejectsInvalidAddress(t *testing.T) {
	t.Setenv("ROAST_BROKER_LISTEN_ADDRESS", "not-an-address")
	if err := healthcheck(); err == nil {
		t.Fatal("healthcheck unexpectedly accepted an invalid listen address")
	}
}
