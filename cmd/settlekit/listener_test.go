package main

import (
	"net"
	"testing"
	"time"
)

func TestBoundedListenerReleasesSlotAndStopsOnClose(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listener := newBoundedListener(raw, 1)
	defer listener.Close()
	firstClient, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer firstClient.Close()
	firstServer, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	secondClient, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer secondClient.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, _ := listener.Accept()
		accepted <- connection
	}()
	select {
	case connection := <-accepted:
		if connection != nil {
			connection.Close()
		}
		t.Fatal("second connection passed one-slot bound")
	case <-time.After(30 * time.Millisecond):
	}
	if err := firstServer.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case connection := <-accepted:
		if connection == nil {
			t.Fatal("second connection was rejected after slot release")
		}
		connection.Close()
	case <-time.After(time.Second):
		t.Fatal("slot was not released")
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); err == nil {
		t.Fatal("closed listener accepted a connection")
	}
}
