package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFixtureStopsWithAnUnfinishedRequest(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(entered)
		_, _ = io.Copy(io.Discard, r.Body)
	}))
	defer server.Close()
	body, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, io.MultiReader(strings.NewReader("{"), body))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		if response, err := server.Client().Do(request); err == nil {
			_ = response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("handler did not receive the request")
	}

	stopped := make(chan struct{})
	go func() {
		stopFixture(server)
		close(stopped)
	}()
	// Keep the client open until shutdown: closing it first would itself cause the handler to exit.
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("fixture waited for the rest of an unfinished request body")
	}
}
