// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package pzlib

import (
	"bufio"
	"compress/flate"
	"errors"
	"hash"
	"hash/adler32"
	"io"
	"sync"
)

const zlibDeflate = 8

func makeReader(r io.Reader) flate.Reader {
	if rr, ok := r.(flate.Reader); ok {
		return rr
	}
	return bufio.NewReader(r)
}

var (
	// ErrChecksum is returned when reading ZLIB data that has an invalid checksum.
	ErrChecksum = errors.New("zlib: invalid checksum")
	// ErrDictionary is returned when reading ZLIB data that has an invalid dictionary.
	ErrDictionary = errors.New("zlib: invalid dictionary")
	// ErrHeader is returned when reading ZLIB data that has an invalid header.
	ErrHeader = errors.New("zlib: invalid header")
)

type reader struct {
	r            flate.Reader
	decompressor io.ReadCloser
	digest       hash.Hash32
	err          error
	closeErr     chan error
	scratch      [4]byte

	readAhead   chan read
	roff        int // read offset
	current     []byte
	closeReader chan struct{}
	lastBlock   bool
	blockSize   int
	blocks      int

	activeRA bool       // Indication if readahead is active
	mu       sync.Mutex // Lock for above

	blockPool chan []byte
}

// Resetter resets a ReadCloser returned by NewReader or NewReaderDict
// to switch to a new underlying Reader. This permits reusing a ReadCloser
// instead of allocating a new one.
type Resetter interface {
	// Reset discards any buffered data and resets the Resetter as if it was
	// newly initialized with the given reader.
	Reset(r io.Reader, dict []byte) error
}

type read struct {
	b   []byte
	err error
}

// NewReader creates a new ReadCloser.
// Reads from the returned ReadCloser read and decompress data from r.
// If r does not implement io.ByteReader, the decompressor may read more
// data than necessary from r.
// It is the caller's responsibility to call Close on the ReadCloser when done.
//
// The ReadCloser returned by NewReader also implements Resetter.
func NewReader(r io.Reader) (io.ReadCloser, error) {
	return NewReaderDict(r, nil)
}

// NewReaderDict is like NewReader but uses a preset dictionary.
// NewReaderDict ignores the dictionary if the compressed data does not refer to it.
// If the compressed data refers to a different dictionary, NewReaderDict returns ErrDictionary.
//
// The ReadCloser returned by NewReaderDict also implements Resetter.
func NewReaderDict(r io.Reader, dict []byte) (io.ReadCloser, error) {
	z := new(reader)
	err := z.Reset(r, dict)
	if err != nil {
		return nil, err
	}
	return z, nil
}

func (z *reader) killReadAhead() error {
	z.mu.Lock()
	defer z.mu.Unlock()
	if z.activeRA {
		if z.closeReader != nil {
			close(z.closeReader)
		}

		// Wait for decompressor to be closed and return error, if any.
		e, ok := <-z.closeErr
		z.activeRA = false
		if !ok {
			// Channel is closed, so if there was any error it has already been returned.
			return nil
		}
		return e
	}
	return nil
}

// Starts readahead.
// Will return on error (including io.EOF)
// or when z.closeReader is closed.
func (z *reader) doReadAhead() {
	z.mu.Lock()
	defer z.mu.Unlock()
	z.activeRA = true

	if z.blocks <= 0 {
		z.blocks = defaultBlocks
	}
	if z.blockSize <= 512 {
		z.blockSize = defaultBlockSize
	}
	ra := make(chan read, z.blocks)
	z.readAhead = ra
	closeReader := make(chan struct{}, 0)
	z.closeReader = closeReader
	z.lastBlock = false
	closeErr := make(chan error, 1)
	z.closeErr = closeErr
	z.roff = 0
	z.current = nil
	decomp := z.decompressor

	go func() {
		defer func() {
			closeErr <- decomp.Close()
			close(closeErr)
			close(ra)
		}()

		// We hold a local reference to digest, since
		// it way be changed by reset.
		digest := z.digest
		var wg sync.WaitGroup
		for {
			var buf []byte
			select {
			case buf = <-z.blockPool:
			case <-closeReader:
				return
			}
			buf = buf[0:z.blockSize]
			// Try to fill the buffer
			n, err := io.ReadFull(decomp, buf)
			if err == io.ErrUnexpectedEOF {
				if n > 0 {
					err = nil
				} else {
					// If we got zero bytes, we need to establish if
					// we reached end of stream or truncated stream.
					_, err = decomp.Read([]byte{})
					if err == io.EOF {
						err = nil
					}
				}
			}
			if n < len(buf) {
				buf = buf[0:n]
			}
			wg.Wait()
			wg.Add(1)
			go func() {
				digest.Write(buf)
				wg.Done()
			}()

			// If we return any error, out digest must be ready
			if err != nil {
				wg.Wait()
			}
			select {
			case z.readAhead <- read{b: buf, err: err}:
			case <-closeReader:
				// Sent on close, we don't care about the next results
				return
			}
			if err != nil {
				return
			}
		}
	}()
}

func (z *reader) Read(p []byte) (n int, err error) {
	if z.err != nil {
		return 0, z.err
	}
	if len(p) == 0 {
		return 0, nil
	}

	for {
		if len(z.current) == 0 && !z.lastBlock {
			read := <-z.readAhead

			if read.err != nil {
				// If not nil, the reader will have exited
				z.closeReader = nil

				if read.err != io.EOF {
					z.err = read.err
					return
				}
				if read.err == io.EOF {
					z.lastBlock = true
					err = nil
				}
			}
			z.current = read.b
			z.roff = 0
		}
		avail := z.current[z.roff:]
		if len(p) >= len(avail) {
			// If len(p) >= len(current), return all content of current
			n = copy(p, avail)
			z.blockPool <- z.current
			z.current = nil
			if z.lastBlock {
				err = io.EOF
				break
			}
		} else {
			// We copy as much as there is space for
			n = copy(p, avail)
			z.roff += n
		}
		return
	}

	// Finished file; check checksum.
	if _, err := io.ReadFull(z.r, z.scratch[0:4]); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		z.err = err
		return n, z.err
	}
	// ZLIB (RFC 1950) is big-endian, unlike GZIP (RFC 1952).
	checksum := uint32(z.scratch[0])<<24 | uint32(z.scratch[1])<<16 | uint32(z.scratch[2])<<8 | uint32(z.scratch[3])
	if checksum != z.digest.Sum32() {
		z.err = ErrChecksum
		return n, z.err
	}
	return n, io.EOF
}

// Close closes the Reader. It does not close the underlying io.Reader.
func (z *reader) Close() error {
	return z.killReadAhead()
}

// Reset discards the Reader z's state and makes it equivalent to the
// result of its original state from NewReader, but reading from r instead.
// This permits reusing a Reader rather than allocating a new one.
func (z *reader) Reset(r io.Reader, dict []byte) error {
	z.killReadAhead()

	if fr, ok := r.(flate.Reader); ok {
		z.r = fr
	} else {
		z.r = bufio.NewReader(r)
	}

	// Read the header (RFC 1950 section 2.2.).
	_, z.err = io.ReadFull(z.r, z.scratch[0:2])
	if z.err != nil {
		if z.err == io.EOF {
			z.err = io.ErrUnexpectedEOF
		}
		return z.err
	}
	h := uint(z.scratch[0])<<8 | uint(z.scratch[1])
	if (z.scratch[0]&0x0f != zlibDeflate) || (h%31 != 0) {
		z.err = ErrHeader
		return z.err
	}
	haveDict := z.scratch[1]&0x20 != 0
	if haveDict {
		_, z.err = io.ReadFull(z.r, z.scratch[0:4])
		if z.err != nil {
			if z.err == io.EOF {
				z.err = io.ErrUnexpectedEOF
			}
			return z.err
		}
		checksum := uint32(z.scratch[0])<<24 | uint32(z.scratch[1])<<16 | uint32(z.scratch[2])<<8 | uint32(z.scratch[3])
		if checksum != adler32.Checksum(dict) {
			z.err = ErrDictionary
			return z.err
		}
	}

	if z.decompressor == nil {
		if haveDict {
			z.decompressor = flate.NewReaderDict(z.r, dict)
		} else {
			z.decompressor = flate.NewReader(z.r)
		}
	} else {
		z.decompressor.(flate.Resetter).Reset(z.r, dict)
	}
	z.digest = adler32.New()

	// Account for uninitialized values
	if z.blocks <= 0 {
		z.blocks = defaultBlocks
	}
	if z.blockSize <= 512 {
		z.blockSize = defaultBlockSize
	}

	if z.blockPool == nil {
		z.blockPool = make(chan []byte, z.blocks)
		for i := 0; i < z.blocks; i++ {
			z.blockPool <- make([]byte, z.blockSize)
		}
	}

	z.doReadAhead()

	return nil
}
