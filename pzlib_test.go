// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pzlib

import (
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"math/rand"
	"strconv"
	"sync"
	"testing"
)

// TestEmpty tests that an empty payload still forms a valid ZLIB stream.
func TestEmpty(t *testing.T) {
	buf := new(bytes.Buffer)

	if err := NewWriter(buf).Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	b, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if len(b) != 0 {
		t.Fatalf("got %d bytes, want 0", len(b))
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Reader.Close: %v", err)
	}
}

// TestRoundTrip tests that zlib compressing and then decompressing
// returns an identical payload
func TestRoundTrip(t *testing.T) {
	buf := new(bytes.Buffer)

	w := NewWriter(buf)
	if _, err := w.Write([]byte("payload")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Writer.Close: %v", err)
	}

	r, err := NewReader(buf)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	b, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if string(b) != "payload" {
		t.Fatalf("payload is %q, want %q", string(b), "payload")
	}
	if err := r.Close(); err != nil {
		t.Fatalf("Reader.Close: %v", err)
	}
}

func TestWriterFlush(t *testing.T) {
	buf := new(bytes.Buffer)

	w := NewWriter(buf)

	n0 := buf.Len()
	if n0 != 0 {
		t.Fatalf("buffer size = %d before writes; want 0", n0)
	}

	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	n1 := buf.Len()
	if n1 == 0 {
		t.Fatal("no data after first flush")
	}

	w.Write([]byte("x"))

	n2 := buf.Len()
	if n1 != n2 {
		t.Fatalf("after writing a single byte, size changed from %d to %d; want no change", n1, n2)
	}

	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	n3 := buf.Len()
	if n2 == n3 {
		t.Fatal("Flush didn't flush any data")
	}
}

func TestWriterReset(t *testing.T) {
	buf := new(bytes.Buffer)
	buf2 := new(bytes.Buffer)
	z := NewWriter(buf)
	msg := []byte("hello world")
	z.Write(msg)
	z.Close()
	z.Reset(buf2)
	z.Write(msg)
	z.Close()
	if buf.String() != buf2.String() {
		t.Errorf("buf2 %q != original buf of %q", buf2.String(), buf.String())
	}
}

var testbuf []byte

func testFile(i int, t *testing.T) {
	dat, _ := ioutil.ReadFile("testdata/test.json")
	dl := len(dat)
	if len(testbuf) != i*dl {
		// Make results predictable
		testbuf = make([]byte, i*dl)
		for j := 0; j < i; j++ {
			copy(testbuf[j*dl:j*dl+dl], dat)
		}
	}

	br := bytes.NewBuffer(testbuf)
	var buf bytes.Buffer
	w, _ := NewWriterLevel(&buf, 6)
	io.Copy(w, br)
	w.Close()
	r, err := NewReader(&buf)
	if err != nil {
		t.Fatal(err.Error())
	}
	decoded, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !bytes.Equal(testbuf, decoded) {
		t.Errorf("decoded content does not match.")
	}
}

func TestFile1(t *testing.T)  { testFile(1, t) }
func TestFile10(t *testing.T) { testFile(10, t) }

func TestFile50(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping during short test")
	}
	testFile(50, t)
}

func TestFile200(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping during short test")
	}
	testFile(200, t)
}

func testBigZlib(i int, t *testing.T) {
	if len(testbuf) != i {
		// Make results predictable
		rand.Seed(1337)
		testbuf = make([]byte, i)
		for idx := range testbuf {
			testbuf[idx] = byte(65 + rand.Intn(32))
		}
	}

	br := bytes.NewBuffer(testbuf)
	var buf bytes.Buffer
	w, _ := NewWriterLevel(&buf, 6)
	io.Copy(w, br)
	err := w.Close()
	if err != nil {
		t.Fatal(err.Error())
	}

	r, err := NewReader(&buf)
	if err != nil {
		t.Fatal(err.Error())
	}
	decoded, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatal(err.Error())
	}
	if !bytes.Equal(testbuf, decoded) {
		t.Errorf("decoded content does not match.")
	}
}

func TestZlib1K(t *testing.T)   { testBigZlib(1000, t) }
func TestZlib100K(t *testing.T) { testBigZlib(100000, t) }
func TestZlib1M(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping during short test")
	}

	testBigZlib(1000000, t)
}
func TestZlib10M(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping during short test")
	}
	testBigZlib(10000000, t)
}

func BenchmarkZlibL1(b *testing.B) { benchmarkZlibN(b, 1) }
func BenchmarkZlibL2(b *testing.B) { benchmarkZlibN(b, 2) }
func BenchmarkZlibL3(b *testing.B) { benchmarkZlibN(b, 3) }
func BenchmarkZlibL4(b *testing.B) { benchmarkZlibN(b, 4) }
func BenchmarkZlibL5(b *testing.B) { benchmarkZlibN(b, 5) }
func BenchmarkZlibL6(b *testing.B) { benchmarkZlibN(b, 6) }
func BenchmarkZlibL7(b *testing.B) { benchmarkZlibN(b, 7) }
func BenchmarkZlibL8(b *testing.B) { benchmarkZlibN(b, 8) }
func BenchmarkZlibL9(b *testing.B) { benchmarkZlibN(b, 9) }

func benchmarkZlibN(b *testing.B, level int) {
	dat, _ := ioutil.ReadFile("testdata/test.json")
	dat = append(dat, dat...)
	dat = append(dat, dat...)
	dat = append(dat, dat...)
	dat = append(dat, dat...)
	dat = append(dat, dat...)

	b.SetBytes(int64(len(dat)))
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		w, _ := NewWriterLevel(ioutil.Discard, level)
		w.Write(dat)
		w.Flush()
		w.Close()
	}
}

type errorWriter struct {
	mu          sync.RWMutex
	returnError bool
}

func (e *errorWriter) ErrorNow() {
	e.mu.Lock()
	e.returnError = true
	e.mu.Unlock()
}

func (e *errorWriter) Reset() {
	e.mu.Lock()
	e.returnError = false
	e.mu.Unlock()
}

func (e *errorWriter) Write(b []byte) (int, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.returnError {
		return 0, fmt.Errorf("Intentional Error")
	}
	return len(b), nil
}

// TestErrors tests that errors are returned and that
// error state is maintained and reset by Reset.
func TestErrors(t *testing.T) {
	ew := &errorWriter{}
	w := NewWriter(ew)
	dat, _ := ioutil.ReadFile("testdata/test.json")
	n := 0
	ew.ErrorNow()
	for {
		_, err := w.Write(dat)
		if err != nil {
			break
		}
		if n > 1000 {
			t.Fatal("did not get error before 1000 iterations")
		}
		n++
	}
	if err := w.Close(); err == nil {
		t.Fatal("Writer.Close: Should have returned error")
	}
	ew.Reset()
	w.Reset(ew)
	_, err := w.Write(dat)
	if err != nil {
		t.Fatal("Writer after Reset, unexpected error:", err)
	}
	ew.ErrorNow()
	if err = w.Flush(); err == nil {
		t.Fatal("Writer.Flush: Should have returned error")
	}
	if err = w.Close(); err == nil {
		t.Fatal("Writer.Close: Should have returned error")
	}
	// Test Sync only
	w.Reset(ew)
	if err = w.Flush(); err == nil {
		t.Fatal("Writer.Flush: Should have returned error")
	}
	if err = w.Close(); err == nil {
		t.Fatal("Writer.Close: Should have returned error")
	}
	// Test Close only
	w.Reset(ew)
	if err = w.Close(); err == nil {
		t.Fatal("Writer.Close: Should have returned error")
	}

}

// A writer that fails after N bytes.
type errorWriter2 struct {
	N int
}

func (e *errorWriter2) Write(b []byte) (int, error) {
	e.N -= len(b)
	if e.N <= 0 {
		return 0, io.ErrClosedPipe
	}
	return len(b), nil
}

// Test if errors from the underlying writer is passed upwards.
func TestWriteError(t *testing.T) {
	n := defaultBlockSize + 1024
	if !testing.Short() {
		n *= 4
	}
	// Make it incompressible...
	in := make([]byte, n+1<<10)
	io.ReadFull(rand.New(rand.NewSource(0xabad1dea)), in)

	// We create our own buffer to control number of writes.
	copyBuf := make([]byte, 128)
	for l := 0; l < 10; l++ {
		t.Run("level-"+strconv.Itoa(l), func(t *testing.T) {
			for fail := 1; fail < n; fail *= 10 {
				// Fail after 'fail' writes
				ew := &errorWriter2{N: fail}
				w, err := NewWriterLevel(ew, l)
				if err != nil {
					t.Fatalf("NewWriter: level %d: %v", l, err)
				}
				// Set concurrency low enough that errors should propagate.
				w.SetConcurrency(128<<10, 4)
				_, err = copyBuffer(w, bytes.NewBuffer(in), copyBuf)
				if err == nil {
					t.Errorf("Level %d: Expected an error, writer was %#v", l, ew)
				}
				n2, err := w.Write([]byte{1, 2, 2, 3, 4, 5})
				if n2 != 0 {
					t.Error("Level", l, "Expected 0 length write, got", n2)
				}
				if err == nil {
					t.Error("Level", l, "Expected an error")
				}
				err = w.Flush()
				if err == nil {
					t.Error("Level", l, "Expected an error on flush")
				}
				err = w.Close()
				if err == nil {
					t.Error("Level", l, "Expected an error on close")
				}

				w.Reset(ioutil.Discard)
				n2, err = w.Write([]byte{1, 2, 3, 4, 5, 6})
				if err != nil {
					t.Error("Level", l, "Got unexpected error after reset:", err)
				}
				if n2 == 0 {
					t.Error("Level", l, "Got 0 length write, expected > 0")
				}
				if testing.Short() {
					return
				}
			}
		})
	}
}

// copyBuffer is a copy of io.CopyBuffer, since we want to support older go versions.
// This is modified to never use io.WriterTo or io.ReaderFrom interfaces.
func copyBuffer(dst io.Writer, src io.Reader, buf []byte) (written int64, err error) {
	if buf == nil {
		buf = make([]byte, 32*1024)
	}
	for {
		nr, er := src.Read(buf)
		if nr > 0 {
			nw, ew := dst.Write(buf[0:nr])
			if nw > 0 {
				written += int64(nw)
			}
			if ew != nil {
				err = ew
				break
			}
			if nr != nw {
				err = io.ErrShortWrite
				break
			}
		}
		if er == io.EOF {
			break
		}
		if er != nil {
			err = er
			break
		}
	}
	return written, err
}
