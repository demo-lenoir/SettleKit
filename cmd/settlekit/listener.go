package main

import (
	"net"
	"sync"
)

const maxHTTPConnections = 64

type boundedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func newBoundedListener(listener net.Listener, capacity int) *boundedListener {
	return &boundedListener{Listener: listener, slots: make(chan struct{}, capacity), done: make(chan struct{})}
}

func (l *boundedListener) Accept() (net.Conn, error) {
	select {
	case l.slots <- struct{}{}:
	case <-l.done:
		return nil, net.ErrClosed
	}
	connection, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &boundedConn{Conn: connection, release: func() { <-l.slots }}, nil
}

func (l *boundedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

type boundedConn struct {
	net.Conn
	release func()
	once    sync.Once
}

func (c *boundedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
