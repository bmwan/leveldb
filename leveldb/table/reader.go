// Copyright 2011 The LevelDB-Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package table

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"

	"leveldb-go.googlecode.com/hg/leveldb/crc"
	"leveldb-go.googlecode.com/hg/leveldb/db"
	"snappy-go.googlecode.com/hg/snappy"
	"snappy-go.googlecode.com/hg/varint"
)

// blockHandle is the file offset and length of a block.
type blockHandle struct {
	offset, length uint64
}

// readBlockHandle returns the block handle encoded at the start of src, as
// well as the number of bytes it occupies. It returns zero if given invalid
// input.
func readBlockHandle(src []byte) (blockHandle, int) {
	offset, n := varint.Decode(src)
	length, m := varint.Decode(src[n:])
	if n == 0 || m == 0 {
		return blockHandle{}, 0
	}
	return blockHandle{offset, length}, n + m
}

// block is a []byte that holds a sequence of key/value pairs plus an index
// over those pairs.
type block []byte

// seek returns a blockIter positioned at the first key/value pair whose key is
// >= the given key. If there is no such key, the blockIter returned is done.
func (b block) seek(c db.Comparer, key []byte) (*blockIter, os.Error) {
	numRestarts := int(binary.LittleEndian.Uint32(b[len(b)-4:]))
	if numRestarts == 0 {
		return nil, os.NewError("leveldb/table: invalid table (block has no restart points)")
	}
	n := len(b) - 4*(1+numRestarts)
	var offset int
	if len(key) > 0 {
		// Find the index of the smallest restart point whose key is > the key
		// sought; index will be numRestarts if there is no such restart point.
		index := sort.Search(numRestarts, func(i int) bool {
			o := int(binary.LittleEndian.Uint32(b[n+4*i:]))
			// For a restart point, there are 0 bytes shared with the previous key.
			// The varint encoding of 0 occupies 1 byte.
			o++
			// Decode the key at that restart point, and compare it to the key sought.
			v1, n1 := varint.Decode(b[o:])
			_, n2 := varint.Decode(b[o+n1:])
			m := o + n1 + n2
			s := b[m : m+int(v1)]
			return c.Compare(s, key) > 0
		})
		// Since keys are strictly increasing, if index > 0 then the restart
		// point at index-1 will be the largest whose key is <= the key sought.
		// If index == 0, then all keys in this block are larger than the key
		// sought, and offset remains at zero.
		if index > 0 {
			offset = int(binary.LittleEndian.Uint32(b[n+4*(index-1):]))
		}
	}
	// Initialize the blockIter to the restart point.
	i := &blockIter{
		data: b[offset:n],
		key:  make([]byte, 0, 256),
	}
	// Iterate from that restart point to somewhere >= the key sought.
	for i.Next() && c.Compare(i.key, key) < 0 {
	}
	if i.err != nil {
		return nil, i.err
	}
	i.soi = !i.eoi
	return i, nil
}

// blockIter is an iterator over a single block of data.
type blockIter struct {
	data     []byte
	key, val []byte
	err      os.Error
	// soi and eoi mark the start and end of iteration.
	// Both cannot simultaneously be true.
	soi, eoi bool
}

// blockIter implements the db.Iterator interface.
var _ db.Iterator = &blockIter{}

// Next implements Iterator.Next, as documented in the leveldb/db package.
func (i *blockIter) Next() bool {
	if i.eoi || i.err != nil {
		return false
	}
	if i.soi {
		i.soi = false
		return true
	}
	if len(i.data) == 0 {
		i.Close()
		return false
	}
	v0, n0 := varint.Decode(i.data)
	v1, n1 := varint.Decode(i.data[n0:])
	v2, n2 := varint.Decode(i.data[n0+n1:])
	n := n0 + n1 + n2
	i.key = append(i.key[:v0], i.data[n:n+int(v1)]...)
	i.val = i.data[n+int(v1) : n+int(v1+v2)]
	i.data = i.data[n+int(v1+v2):]
	return true
}

// Key implements Iterator.Key, as documented in the leveldb/db package.
func (i *blockIter) Key() []byte {
	if i.soi {
		return nil
	}
	return i.key
}

// Value implements Iterator.Value, as documented in the leveldb/db package.
func (i *blockIter) Value() []byte {
	if i.soi {
		return nil
	}
	return i.val
}

// Close implements Iterator.Close, as documented in the leveldb/db package.
func (i *blockIter) Close() os.Error {
	i.key = nil
	i.val = nil
	i.eoi = true
	return i.err
}

// tableIter is an iterator over an entire table of data. It is a two-level
// iterator: to seek for a given key, it first looks in the index for the
// block that contains that key, and then looks inside that block.
type tableIter struct {
	reader *Reader
	data   *blockIter
	index  *blockIter
	err    os.Error
}

// tableIter implements the db.Iterator interface.
var _ db.Iterator = &tableIter{}

// nextBlock loads the next block and positions i.data at the first key in that
// block which is >= the given key. If unsuccessful, it sets i.err to any error
// encountered, which may be nil if we have simply exhausted the entire table.
func (i *tableIter) nextBlock(key []byte) bool {
	if !i.index.Next() {
		i.err = i.index.err
		return false
	}
	// Load the next block.
	v := i.index.Value()
	h, n := readBlockHandle(v)
	if n == 0 || n != len(v) {
		i.err = os.NewError("leveldb/table: corrupt index entry")
		return false
	}
	k, err := i.reader.readBlock(h)
	if err != nil {
		i.err = err
		return false
	}
	// Look for the key inside that block.
	data, err := k.seek(i.reader.comparer, key)
	if err != nil {
		i.err = err
		return false
	}
	i.data = data
	return true
}

// Next implements Iterator.Next, as documented in the leveldb/db package.
func (i *tableIter) Next() bool {
	if i.data == nil {
		return false
	}
	for {
		if i.data.Next() {
			return true
		}
		if i.data.err != nil {
			i.err = i.data.err
			break
		}
		if !i.nextBlock(nil) {
			break
		}
	}
	i.Close()
	return false
}

// Key implements Iterator.Key, as documented in the leveldb/db package.
func (i *tableIter) Key() []byte {
	if i.data == nil {
		return nil
	}
	return i.data.Key()
}

// Value implements Iterator.Value, as documented in the leveldb/db package.
func (i *tableIter) Value() []byte {
	if i.data == nil {
		return nil
	}
	return i.data.Value()
}

// Close implements Iterator.Close, as documented in the leveldb/db package.
func (i *tableIter) Close() os.Error {
	i.data = nil
	return i.err
}

// Reader is a table reader. It implements the DB interface, as documented
// in the leveldb/db package.
type Reader struct {
	file            File
	index           block
	comparer        db.Comparer
	verifyChecksums bool
	// TODO: add a (goroutine-safe) LRU block cache.
}

// Reader implements the db.DB interface.
var _ db.DB = &Reader{}

// Close implements DB.Close, as documented in the leveldb/db package.
func (r *Reader) Close() os.Error {
	return r.file.Close()
}

// Get implements DB.Get, as documented in the leveldb/db package.
func (r *Reader) Get(key []byte) (value []byte, err os.Error) {
	i := r.Find(key)
	if !i.Next() || !bytes.Equal(key, i.Key()) {
		err := i.Close()
		if err == nil {
			err = db.ErrNotFound
		}
		return nil, err
	}
	return i.Value(), i.Close()
}

// Set is provided to implement the DB interface, but returns an error, as a
// Reader cannot write to a table.
func (r *Reader) Set(key, value []byte) os.Error {
	return os.NewError("leveldb/table: cannot Set into a read-only table")
}

// Delete is provided to implement the DB interface, but returns an error, as a
// Reader cannot write to a table.
func (r *Reader) Delete([]byte) os.Error {
	return os.NewError("leveldb/table: cannot Delete from a read-only table")
}

// Find implements DB.Find, as documented in the leveldb/db package.
func (r *Reader) Find(key []byte) db.Iterator {
	index, err := r.index.seek(r.comparer, key)
	if err != nil {
		return &tableIter{err: err}
	}
	i := &tableIter{
		reader: r,
		index:  index,
	}
	i.nextBlock(key)
	return i
}

// readBlock reads and decompresses a block from disk into memory.
func (r *Reader) readBlock(bh blockHandle) (block, os.Error) {
	b := make([]byte, bh.length+blockTrailerLen)
	if _, err := r.file.ReadAt(b, int64(bh.offset)); err != nil {
		return nil, err
	}
	if r.verifyChecksums {
		checksum0 := binary.LittleEndian.Uint32(b[bh.length+1:])
		checksum1 := crc.New(b[:bh.length+1]).Value()
		if checksum0 != checksum1 {
			return nil, os.NewError("leveldb/table: invalid table (checksum mismatch)")
		}
	}
	switch b[bh.length] {
	case noCompressionBlockType:
		return b[:bh.length], nil
	case snappyCompressionBlockType:
		b, err := snappy.Decode(nil, b[:bh.length])
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return nil, fmt.Errorf("leveldb/table: unknown block compression: %d", b[bh.length])
}

// File provides the raw bytes for a table reader.
type File interface {
	io.Closer
	io.ReaderAt
	Stat() (*os.FileInfo, os.Error)
}

// Open opens the file for reading a table. If the returned reader is not nil,
// closing the reader will close the file. If the returned reader is nil, the
// file is not closed.
func Open(f File, o *db.Options) (*Reader, os.Error) {
	stat, err := f.Stat()
	if err != nil {
		return nil, err
	}
	var footer [footerLen]byte
	if stat.Size < int64(len(footer)) {
		return nil, os.NewError("leveldb/table: invalid table (file size is too small)")
	}
	_, err = f.ReadAt(footer[:], stat.Size-int64(len(footer)))
	if err != nil && err != os.EOF {
		return nil, err
	}
	if string(footer[footerLen-len(magic):footerLen]) != magic {
		return nil, os.NewError("leveldb/table: invalid table (bad magic number)")
	}
	r := &Reader{
		file:            f,
		comparer:        o.GetComparer(),
		verifyChecksums: o.GetVerifyChecksums(),
	}
	// Ignore the metaindex.
	_, n := readBlockHandle(footer[:])
	if n == 0 {
		return nil, os.NewError("leveldb/table: invalid table (bad metaindex block handle)")
	}
	// Read the index into memory.
	indexBH, n := readBlockHandle(footer[n:])
	if n == 0 {
		return nil, os.NewError("leveldb/table: invalid table (bad index block handle)")
	}
	r.index, err = r.readBlock(indexBH)
	if err != nil {
		return nil, err
	}
	return r, nil
}