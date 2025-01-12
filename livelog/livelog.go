// Copyright 2019 Drone.IO Inc. All rights reserved.
// Use of this source code is governed by the Polyform License
// that can be found in the LICENSE file.

// Package livelog provides a Writer that collects pipeline
// output and streams to the central server.
package livelog

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	"github.com/drone/drone-go/drone"
	"github.com/drone/runner-go/client"
)

// defaultLimit is the default maximum log size in bytes.
const defaultLimit = 5242880 // 5MB

// Writer is an io.Writer that sends logs to the server.
type Writer struct {
	sync.Mutex

	client client.Client

	id    int64
	num   int
	now   time.Time
	size  int
	limit int

	interval time.Duration
	pending  []*drone.Line
	history  []*drone.Line

	closed bool
	close  chan struct{}
	ready  chan struct{}
}

// New returns a new Wrtier.
func New(client client.Client, id int64) *Writer {
	b := &Writer{
		client:   client,
		id:       id,
		now:      time.Now(),
		limit:    defaultLimit,
		interval: time.Second,
		close:    make(chan struct{}),
		ready:    make(chan struct{}, 1),
	}
	go b.start()
	return b
}

// SetLimit sets the Writer limit.
func (b *Writer) SetLimit(limit int) {
	b.limit = limit
}

// SetInterval sets the Writer flusher interval.
func (b *Writer) SetInterval(interval time.Duration) {
	b.interval = interval
}

// Write uploads the live log stream to the server.
func (b *Writer) Write(p []byte) (n int, err error) {
	for _, part := range split(p) {
		line := &drone.Line{
			Number:    b.num,
			Message:   part,
			Timestamp: int64(time.Since(b.now).Seconds()),
		}

		for b.size+len(p) > b.limit {
			b.stop() // buffer is full, step streaming data
			b.size -= len(b.history[0].Message)
			b.history = b.history[1:]
		}

		b.size = b.size + len(part)
		b.num++

		if !b.stopped() {
			b.Lock()
			b.pending = append(b.pending, line)
			b.Unlock()
		}

		b.Lock()
		b.history = append(b.history, line)
		b.Unlock()
	}

	select {
	case b.ready <- struct{}{}:
	default:
	}

	return len(p), nil
}

// Close closes the writer and uploads the full contents to
// the server.
func (b *Writer) Close() error {
	if b.stop() {
		b.flush()
	}
	return b.upload()
}

// upload uploads the full log history to the server.
func (b *Writer) upload() error {
	return b.client.Upload(
		context.Background(), b.id, b.history)
}

// flush batch uploads all buffered logs to the server.
func (b *Writer) flush() error {
	b.Lock()
	lines := b.copy()
	b.clear()
	b.Unlock()
	if len(lines) == 0 {
		return nil
	}
	return b.client.Batch(
		context.Background(), b.id, lines)
}

// copy returns a copy of the buffered lines.
func (b *Writer) copy() []*drone.Line {
	return append(b.pending[:0:0], b.pending...)
}

// clear clears the buffer.
func (b *Writer) clear() {
	b.pending = b.pending[:0]
}

func (b *Writer) stop() bool {
	b.Lock()
	var closed bool
	if !b.closed {
		close(b.close)
		closed = true
		b.closed = true
	}
	if linebuf.Len() != 0 {
		b.Write(linebuf.Bytes())
		linebuf.Reset()
	}

	b.Unlock()
	return closed
}

func (b *Writer) stopped() bool {
	b.Lock()
	closed := b.closed
	b.Unlock()
	return closed
}

func (b *Writer) start() error {
	for {
		select {
		case <-b.close:
			return nil
		case <-b.ready:
			select {
			case <-b.close:
				return nil
			case <-time.After(b.interval):
				// we intentionally ignore errors. log streams
				// are ephemeral and are considered low prioirty
				// because they are not required for drone to
				// operator, and the impact of failure is minimal
				b.flush()
			}
		}
	}
}

var linebuf bytes.Buffer

func split(p []byte) []string {
	linebuf.Write(p)
	s := linebuf.String()
	lastLF := strings.LastIndex(s, "\n")
	if lastLF == -1 {
		return nil
	}

	v := strings.SplitAfter(string(linebuf.Next(lastLF+1)), "\n")
	v = v[:len(v)-1]

	return v
}
