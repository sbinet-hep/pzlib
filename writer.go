// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pzlib

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"hash/adler32"
	"io"
	"runtime"
	"sync"

	"github.com/klauspost/compress/flate"
)

const (
	defaultBlockSize = 1 << 20
	tailSize         = 16384
	defaultBlocks    = 4
)

const (
	NoCompression      = flate.NoCompression
	BestSpeed          = flate.BestSpeed
	BestCompression    = flate.BestCompression
	DefaultCompression = flate.DefaultCompression
	HuffmanOnly        = flate.HuffmanOnly
)

type Writer struct {
	w             io.Writer
	level         int
	dict          []byte
	blockSize     int
	blocks        int
	currentBuffer []byte
	prevTail      []byte
	digest        hash.Hash32
	closed        bool // Do we need closed?
	err           error
	errMu         sync.RWMutex
	scratch       [4]byte
	wroteHeader   bool
	pushedErr     chan struct{}
	results       chan result
	dictFlatePool sync.Pool
	dstPool       sync.Pool
	wg            sync.WaitGroup
}

type result struct {
	result        chan []byte
	notifyWritten chan struct{}
}

func (z *Writer) SetConcurrency(blockSize, blocks int) error {
	if blockSize <= tailSize {
		return fmt.Errorf("zlib: block size cannot be less than or equal to %d", tailSize)
	}
	if blocks <= 0 {
		return errors.New("zlib: blocks cannot be zero or less")
	}
	if blockSize == z.blockSize && blocks == z.blocks {
		return nil
	}
	z.blockSize = blockSize
	z.results = make(chan result, blocks)
	z.blocks = blocks
	z.dstPool.New = func() interface{} { return make([]byte, 0, blockSize+(blockSize)>>4) }
	return nil
}

func NewWriter(w io.Writer) *Writer {
	z, _ := NewWriterLevelDict(w, DefaultCompression, nil)
	return z
}

func NewWriterLevel(w io.Writer, level int) (*Writer, error) {
	return NewWriterLevelDict(w, level, nil)
}

func NewWriterLevelDict(w io.Writer, level int, dict []byte) (*Writer, error) {
	if level < HuffmanOnly || level > BestCompression {
		return nil, fmt.Errorf("zlib: invalid compression level: %d", level)
	}
	z := new(Writer)
	z.SetConcurrency(defaultBlockSize, runtime.GOMAXPROCS(0))
	z.init(w, level, dict)
	return z, nil
}

func (z *Writer) pushError(err error) {
	z.errMu.Lock()
	if z.err != nil {
		z.errMu.Unlock()
		return
	}
	z.err = err
	close(z.pushedErr)
	z.errMu.Unlock()
}

func (z *Writer) init(w io.Writer, level int, dict []byte) {
	z.wg.Wait()
	digest := z.digest
	if digest != nil {
		digest.Reset()
	} else {
		digest = adler32.New()
	}
	z.w = w
	z.level = level
	z.dict = dict // dict?
	z.digest = digest
	z.pushedErr = make(chan struct{}, 0)
	z.results = make(chan result, z.blocks)
	z.err = nil
	z.closed = false
	z.wroteHeader = false
	z.currentBuffer = nil
	z.scratch = [4]byte{}
	z.prevTail = nil
	if z.dictFlatePool.New == nil {
		z.dictFlatePool.New = func() interface{} {
			f, _ := flate.NewWriterDict(w, level, dict) // dict?
			return f
		}
	}
}

func (z *Writer) Reset(w io.Writer) {
	if z.results != nil && !z.closed {
		close(z.results)
	}
	z.SetConcurrency(defaultBlockSize, runtime.GOMAXPROCS(0))
	z.init(w, z.level, z.dict)
}

// writeHeader writes the ZLIB header.
func (z *Writer) writeHeader() (err error) {
	z.wroteHeader = true
	// ZLIB has a two-byte header (as documented in RFC 1950).
	// The first four bits is the CINFO (compression info), which is 7 for the default deflate window size.
	// The next four bits is the CM (compression method), which is 8 for deflate.
	z.scratch[0] = 0x78
	// The next two bits is the FLEVEL (compression level). The four values are:
	// 0=fastest, 1=fast, 2=default, 3=best.
	// The next bit, FDICT, is set if a dictionary is given.
	// The final five FCHECK bits form a mod-31 checksum.
	switch z.level {
	case -2, 0, 1:
		z.scratch[1] = 0 << 6
	case 2, 3, 4, 5:
		z.scratch[1] = 1 << 6
	case 6, -1:
		z.scratch[1] = 2 << 6
	case 7, 8, 9:
		z.scratch[1] = 3 << 6
	default:
		panic("unreachable")
	}
	if z.dict != nil {
		z.scratch[1] |= 1 << 5
	}
	z.scratch[1] += uint8(31 - (uint16(z.scratch[0])<<8+uint16(z.scratch[1]))%31)
	if _, err = z.w.Write(z.scratch[0:2]); err != nil {
		return err
	}
	if z.dict != nil {
		// The next four bytes are the Adler-32 checksum of the dictionary.
		binary.BigEndian.PutUint32(z.scratch[:], adler32.Checksum(z.dict))
		if _, err = z.w.Write(z.scratch[0:4]); err != nil {
			return err
		}
	}
	return nil
}

// compressCurrent will compress the data currently buffered
// This should only be called from the main writer/flush/closer
func (z *Writer) compressCurrent(flush bool) {
	c := z.currentBuffer
	if len(c) > z.blockSize {
		// This can never happen through the public interface.
		panic("len(z.currentBuffer) > z.blockSize (most likely due to concurrent Write race)")
	}

	r := result{}
	r.result = make(chan []byte, 1)
	r.notifyWritten = make(chan struct{}, 0)
	// Reserve a result slot
	select {
	case z.results <- r:
	case <-z.pushedErr:
		return
	}

	z.wg.Add(1)
	tail := z.prevTail
	if len(c) > tailSize {
		buf := z.dstPool.Get().([]byte) // Put in .compressBlock
		// Copy tail from current buffer before handing the buffer over to the
		// compressBlock goroutine.
		buf = append(buf[:0], c[len(c)-tailSize:]...)
		z.prevTail = buf
	} else {
		z.prevTail = nil
	}
	go z.compressBlock(c, tail, r, z.closed)

	z.currentBuffer = z.dstPool.Get().([]byte) // Put in .compressBlock
	z.currentBuffer = z.currentBuffer[:0]

	// Wait if flushing
	if flush {
		<-r.notifyWritten
	}
}

// Returns an error if it has been set.
// Cannot be used by functions that are from internal goroutines.
func (z *Writer) checkError() error {
	z.errMu.RLock()
	err := z.err
	z.errMu.RUnlock()
	return err
}

func (z *Writer) Write(p []byte) (int, error) {
	if err := z.checkError(); err != nil {
		return 0, err
	}
	// Write the ZLIB header lazily.
	if !z.wroteHeader {
		err := z.writeHeader()
		if err != nil {
			z.pushError(err)
			return 0, err
		}
		// Start receiving data from compressors
		go func() {
			listen := z.results
			var failed bool
			for {
				r, ok := <-listen
				// If closed, we are finished.
				if !ok {
					return
				}
				if failed {
					close(r.notifyWritten)
					continue
				}
				buf := <-r.result
				n, err := z.w.Write(buf)
				if err != nil {
					z.pushError(err)
					close(r.notifyWritten)
					failed = true
					continue
				}
				if n != len(buf) {
					z.pushError(fmt.Errorf("zlib: short write %d should be %d", n, len(buf)))
					failed = true
					close(r.notifyWritten)
					continue
				}
				z.dstPool.Put(buf)
				close(r.notifyWritten)
			}
		}()
		z.currentBuffer = z.dstPool.Get().([]byte)
		z.currentBuffer = z.currentBuffer[:0]
	}
	q := p
	for len(q) > 0 {
		length := len(q)
		if length+len(z.currentBuffer) > z.blockSize {
			length = z.blockSize - len(z.currentBuffer)
		}
		z.digest.Write(q[:length])
		z.currentBuffer = append(z.currentBuffer, q[:length]...)
		if len(z.currentBuffer) > z.blockSize {
			panic("z.currentBuffer too large (most likely due to concurrent Write race)")
		}
		if len(z.currentBuffer) == z.blockSize {
			z.compressCurrent(false)
			if err := z.checkError(); err != nil {
				return len(p) - len(q) - length, err
			}
		}
		q = q[length:]
	}
	return len(p), z.checkError()
}

// Step 1: compresses buffer to buffer
// Step 2: send writer to channel
// Step 3: Close result channel to indicate we are done
func (z *Writer) compressBlock(p, prevTail []byte, r result, closed bool) {
	defer func() {
		close(r.result)
		z.wg.Done()
	}()
	buf := z.dstPool.Get().([]byte) // Corresponding Put in .Write's result writer
	dest := bytes.NewBuffer(buf[:0])

	compressor := z.dictFlatePool.Get().(*flate.Writer) // Put below
	compressor.ResetDict(dest, prevTail)
	compressor.Write(p)
	z.dstPool.Put(p) // Corresponding Get in .Write and .compressCurrent

	err := compressor.Flush()
	if err != nil {
		z.pushError(err)
		return
	}
	if closed {
		err = compressor.Close()
		if err != nil {
			z.pushError(err)
			return
		}
	}
	z.dictFlatePool.Put(compressor) // Get above

	if prevTail != nil {
		z.dstPool.Put(prevTail) // Get in .compressCurrent
	}

	// Read back buffer
	buf = dest.Bytes()
	r.result <- buf
}

// Flush flushes the Writer to its underlying io.Writer.
func (z *Writer) Flush() error {
	if err := z.checkError(); err != nil {
		return err
	}
	if z.closed {
		return nil
	}
	if !z.wroteHeader {
		_, err := z.Write(nil)
		if err != nil {
			return err
		}
	}
	// We send current block to compression
	z.compressCurrent(true)

	return z.checkError()
}

// Close closes the Writer, flushing any unwritten data to the underlying
// io.Writer, but does not close the underlying io.Writer.
func (z *Writer) Close() error {
	if err := z.checkError(); err != nil {
		return err
	}
	if z.closed {
		return nil
	}

	z.closed = true
	if !z.wroteHeader {
		_, err := z.Write(nil)
		if err != nil {
			return err
		}
	}
	z.compressCurrent(true)
	if err := z.checkError(); err != nil {
		return err
	}
	close(z.results)
	checksum := z.digest.Sum32()
	// ZLIB (RFC 1950) is big-endian, unlike GZIP (RFC 1952).
	binary.BigEndian.PutUint32(z.scratch[:], checksum)
	_, err := z.w.Write(z.scratch[0:4])
	if err != nil {
		z.pushError(err)
		return err
	}
	return nil
}
