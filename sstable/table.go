// Copyright 2011 The LevelDB-Go and Pebble Authors. All rights reserved. Use
// of this source code is governed by a BSD-style license that can be found in
// the LICENSE file.

// Package sstable implements readers and writers of pebble tables.
//
// Tables are either opened for reading or created for writing but not both.
//
// A reader can create iterators, which allow seeking and next/prev
// iteration. There may be multiple key/value pairs that have the same key and
// different sequence numbers.
//
// A reader can be used concurrently. Multiple goroutines can call NewIter
// concurrently, and each iterator can run concurrently with other iterators.
// However, any particular iterator should not be used concurrently, and iterators
// should not be used once a reader is closed.
//
// A writer writes key/value pairs in increasing key order, and cannot be used
// concurrently. A table cannot be read until the writer has finished.
//
// Readers and writers can be created with various options. Passing a nil
// Options pointer is valid and means to use the default values.
//
// One such option is to define the 'less than' ordering for keys. The default
// Comparer uses the natural ordering consistent with bytes.Compare. The same
// ordering should be used for reading and writing a table.
//
// To return the value for a key:
//
//	r := table.NewReader(file, options)
//	defer r.Close()
//	i := r.NewIter(nil, nil)
//	defer i.Close()
//	ikey, value := r.SeekGE(key)
//	if options.Comparer.Compare(ikey.UserKey, key) != 0 {
//	  // not found
//	} else {
//	  // value is the first record containing key
//	}
//
// To count the number of entries in a table:
//
//	i, n := r.NewIter(nil, nil), 0
//	for key, value := i.First(); key != nil; key, value = i.Next() {
//		n++
//	}
//	if err := i.Close(); err != nil {
//		return 0, err
//	}
//	return n, nil
//
// To write a table with three entries:
//
//	w := table.NewWriter(file, options)
//	if err := w.Set([]byte("apple"), []byte("red")); err != nil {
//		w.Close()
//		return err
//	}
//	if err := w.Set([]byte("banana"), []byte("yellow")); err != nil {
//		w.Close()
//		return err
//	}
//	if err := w.Set([]byte("cherry"), []byte("red")); err != nil {
//		w.Close()
//		return err
//	}
//	return w.Close()
package sstable // import "github.com/cockroachdb/pebble/sstable"

import (
	"context"
	"encoding/binary"

	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/pebble/internal/base"
	"github.com/cockroachdb/pebble/internal/crc"
	"github.com/cockroachdb/pebble/objstorage"
	"github.com/cockroachdb/pebble/sstable/block"
)

/*
The table file format looks like:

<start_of_file>
[data block 0]
[data block 1]
...
[data block N-1]
[meta filter block] (optional)
[index block] (for single level index)
[meta rangedel block] (optional)
[meta range key block] (optional)
[value block 0] (optional)
[value block M-1] (optional)
[blob reference value liveness index block] (optional)
[meta value index block] (optional)
[meta properties block]
[metaindex block]
[footer]
<end_of_file>

A Reader eagerly loads the footer, metaindex block and meta properties block,
because the data contained in those blocks is needed on every read, and even
before reading. For example, the meta properties block is used to verify the
comparer and merger are compatible, and the metaindex block contains the
location of the meta properties (and other meta blocks). In situations where
file system locality matters, or one wants to minimize number of read
requests when eagerly loading these blocks, having these three as a suffix
of the file is convenient.

The interleaving of the index block(s) between the meta blocks is done to
match RocksDB/LevelDB behavior.

Each block consists of some data and a 5 byte trailer: a 1 byte block type and a
4 byte checksum. The checksum is computed over the compressed data and the first
byte of the trailer (i.e. the block type), and is serialized as little-endian.
The block type gives the per-block compression used; each block is compressed
independently. The checksum algorithm is described in the pebble/crc package.

Most blocks, other than the meta filter block, value blocks and meta value
index block, contain key/value pairs. The remainder of this comment refers to
the decompressed block, containing key/value pairs, which has its 5 byte
trailer stripped. The decompressed block data consists of a sequence of such
key/value entries followed by a block suffix. Each key is encoded as a shared
prefix length and a remainder string. For example, if two adjacent keys are
"tweedledee" and "tweedledum", then the second key would be encoded as {8,
"um"}. The shared prefix length is varint encoded. The remainder string and the
value are encoded as a varint-encoded length followed by the literal contents.
To continue the example, suppose that the key "tweedledum" mapped to the value
"socks". The encoded key/value entry would be: "\x08\x02\x05umsocks".

Every block has a restart interval I. Every I'th key/value entry in that block
is called a restart point, and shares no key prefix with the previous entry.
Continuing the example above, if the key after "tweedledum" was "two", but was
part of a restart point, then that key would be encoded as {0, "two"} instead
of {2, "o"}. If a block has P restart points, then the block suffix consists
of (P+1)*4 bytes: (P+1) little-endian uint32 values. The first P of these
uint32 values are the block offsets of each restart point. The final uint32
value is P itself. Thus, when seeking for a particular key, one can use binary
search to find the largest restart point whose key is <= the key sought.

An index block is a block with N key/value entries. The i'th value is the
encoded block handle of the i'th data block. The i'th key is a separator for
i < N-1, and a successor for i == N-1. The separator between blocks i and i+1
is a key that is >= every key in block i and is < every key i block i+1. The
successor for the final block is a key that is >= every key in block N-1. The
index block restart interval is 1: every entry is a restart point.

A block handle is an offset, a length, and optional block properties (for data
blocks and first/lower level index blocks); the length does not include the 5
byte trailer. All numbers are varint-encoded, with no padding between the two
values. The maximum size of an encoded block handle without properties is 20
bytes. It is not advised to have properties that accumulate to be longer than
100 bytes.

Instead of a single index block, the sstable can have a two-level index (this
is used to prevent a single huge index block). A two-level index consists of a
sequence of lower-level index blocks with block handles for data blocks
followed by a single top-level index block with block handles for the
lower-level index blocks.

The metaindex block also contains block handles as values, with keys being
the names of the meta blocks.

For a description of value blocks and the meta value index block, see
value_block.go.

Data blocks have some additional features:
- For TableFormatPebblev3 onwards:
  - For SETs, the value has a 1 byte value prefix, which indicates whether the
    value is inline, or in a separate value block, and indicates whether the
    prefix of the userkey (as defined by split) has changed or not. See
    value_block.go for details.
  - The most significant bit of the restart points is used to indicate whether
    userkey prefix has changed since the last restart point. See the detailed
    comment in blockWriter.
  - The maximum length of the "shared prefix" when encoding the key, is the
    length of the prefix of the userkey (as defined by split) of the previous
    key.

- For TableFormatPebblev4 onwards:
  - The key kinds may be altered to set the
    InternalKeyKindSSTableInternalObsoleteBit if the key-value pair is obsolete
    in the context of that sstable (for a reader that reads at a higher seqnum
    than the highest seqnum in the sstable). For details, see the comment in
    format.go.
*/

const (
	// blockHandleMaxLenWithoutProperties is the maximum length of a block
	// handle that does not encode any block properties. It consists of just
	// the offset and length, each of which is a varint-encoded uint64.
	blockHandleMaxLenWithoutProperties = 2 * binary.MaxVarintLen64
	// blockHandleLikelyMaxLen can be used for pre-allocating buffers to
	// reduce memory copies. It is not guaranteed that a block handle will not
	// exceed this length.
	blockHandleLikelyMaxLen = blockHandleMaxLenWithoutProperties + 100

	checksumLen   = 4
	magicLen      = 8
	versionLen    = 4
	attributesLen = 4

	levelDBFooterLen = 48
	levelDBMagic     = "\x57\xfb\x80\x8b\x24\x75\x47\xdb"

	rocksDBFooterLen             = 1 + 2*blockHandleMaxLenWithoutProperties + versionLen + magicLen
	rocksDBMagic                 = "\xf7\xcf\xf4\x85\xb7\x41\xe2\x88"
	rocksDBExternalFormatVersion = 2

	pebbleDBMagic = "\xf0\x9f\xaa\xb3\xf0\x9f\xaa\xb3" // 🪳🪳

	checkedPebbleDBFooterLen      = rocksDBFooterLen + checksumLen
	checkedPebbleDBMagicOffset    = checkedPebbleDBFooterLen - magicLen
	checkedPebbleDBVersionOffset  = checkedPebbleDBMagicOffset - versionLen
	checkedPebbleDBChecksumOffset = checkedPebbleDBVersionOffset - checksumLen

	// TableFormatPebblev7 footer introduces the Attributes bitset.
	pebbleDBv7FooterLen              = checkedPebbleDBFooterLen + attributesLen
	pebbleDBv7FooterChecksumOffset   = pebbleDBv7FooterLen - magicLen - versionLen - checksumLen
	pebbleDBV7FooterAttributesOffset = pebbleDBv7FooterChecksumOffset - attributesLen

	minFooterLen = levelDBFooterLen
	maxFooterLen = pebbleDBv7FooterLen

	levelDBFormatVersion  = 0
	rocksDBFormatVersion2 = 2

	metaRangeKeyName        = "pebble.range_key"
	metaValueIndexName      = "pebble.value_index"
	metaPropertiesName      = "rocksdb.properties"
	metaRangeDelV1Name      = "rocksdb.range_del"
	metaRangeDelV2Name      = "rocksdb.range_del2"
	metaBlobRefIndexName    = "pebble.blob_ref_index"
	metaBlobRefLivenessName = "pebble.blob_ref_liveness"

	// Index Types.
	// A space efficient index block that is optimized for binary-search-based
	// index.
	binarySearchIndex = 0
	// hashSearchIndex               = 1
	// A two-level index implementation. Both levels are binary search indexes.
	twoLevelIndex = 2
	// binarySearchWithFirstKeyIndex = 3

	// RocksDB always includes this in the properties block. Since Pebble
	// doesn't use zstd compression, the string will always be the same.
	// This should be removed if we ever decide to diverge from the RocksDB
	// properties block.
	rocksDBCompressionOptions = "window_bits=-14; level=32767; strategy=0; max_dict_bytes=0; zstd_max_train_bytes=0; enabled=0; "
)

// footer formats. Note that much of the existing footer parsing code assumes
// that the version (for non-legacy formats) and magic number are at the end.
//
// legacy (LevelDB) footer format:
//
//	metaindex handle (varint64 offset, varint64 size)
//	index handle     (varint64 offset, varint64 size)
//	<padding> to make the total size 2 * BlockHandle::kMaxEncodedLength
//	table_magic_number (8 bytes)
//
// (RocksDB) footer format:
//
//	checksum type (char, 1 byte)
//	metaindex handle (varint64 offset, varint64 size)
//	index handle     (varint64 offset, varint64 size)
//	<padding> to make the total size 2 * BlockHandle::kMaxEncodedLength + 1
//	footer version (4 bytes)
//	table_magic_number (8 bytes)
//
// (RocksDB + checksum) footer format [applies to TableFormatPebblev6 and later]
//
//	checksum type (char, 1 byte)
//	metaindex handle (varint64 offset, varint64 size)
//	index handle     (varint64 offset, varint64 size)
//	<padding> to make the total size 2 * BlockHandle::kMaxEncodedLength + 1
//	checksum: CRC over footer data (4 bytes)
//	footer version (4 bytes)
//	table_magic_number (8 bytes)
//
// (RocksDB + checksum + attributes) footer format [applies to TableFormatPebblev7 and later]
//
//	checksum type (char, 1 byte)
//	metaindex handle (varint64 offset, varint64 size)
//	index handle     (varint64 offset, varint64 size)
//	<padding> to make the total size 2 * BlockHandle::kMaxEncodedLength + 1
//	attributes: feature bitset (4 bytes)
//	checksum: CRC over footer data (4 bytes)
//	footer version (4 bytes)
//	table_magic_number (8 bytes)
type footer struct {
	format      TableFormat
	attributes  Attributes
	checksum    block.ChecksumType
	metaindexBH block.Handle
	indexBH     block.Handle
	footerBH    block.Handle
}

// readFooter reads the footer from the end of the file.
//
// readHandle is optional.
func readFooter(
	ctx context.Context,
	f objstorage.Readable,
	readHandle objstorage.ReadHandle,
	logger base.LoggerAndTracer,
	fileNum base.DiskFileNum,
) (footer, error) {
	var err error
	size := f.Size()
	if size < minFooterLen {
		return footer{}, base.CorruptionErrorf("pebble/table: invalid table %s (file size is too small)",
			errors.Safe(fileNum))
	}
	buf := make([]byte, maxFooterLen)
	off := size - maxFooterLen
	if off < 0 {
		off = 0
		buf = buf[:size]
	}
	buf, err = block.ReadRaw(ctx, f, readHandle, logger, fileNum, buf, off)
	if err != nil {
		return footer{}, err
	}
	foot, err := parseFooter(buf, off, size)
	if err != nil {
		return footer{}, errors.Wrapf(err, "pebble/table: invalid table %s", errors.Safe(fileNum))
	}
	return foot, nil
}

func parseFooter(buf []byte, off, size int64) (footer, error) {
	var footer footer
	switch magic := buf[len(buf)-magicLen:]; string(magic) {
	case levelDBMagic:
		if len(buf) < levelDBFooterLen {
			return footer, base.CorruptionErrorf("(footer too short): %d", errors.Safe(len(buf)))
		}
		footer.footerBH.Offset = uint64(off+int64(len(buf))) - levelDBFooterLen
		buf = buf[len(buf)-levelDBFooterLen:]
		footer.footerBH.Length = uint64(len(buf))
		footer.format = TableFormatLevelDB
		footer.checksum = block.ChecksumTypeCRC32c

	case rocksDBMagic, pebbleDBMagic:
		// NOTE: The Pebble magic string implies the same footer format as that used
		// by the RocksDBv2 table format.
		if len(buf) < rocksDBFooterLen {
			return footer, base.CorruptionErrorf("(footer too short): %d", errors.Safe(len(buf)))
		}

		// Parse the table format to see if there is a checksum or not. v6+ has a checksum before version encoding.
		version := binary.LittleEndian.Uint32(buf[len(buf)-magicLen-versionLen:])
		format, err := parseTableFormat(magic, version)
		if err != nil {
			return footer, err
		}

		footerLen := format.FooterSize()
		if len(buf) < footerLen {
			return footer, base.CorruptionErrorf("(footer too short): %d", errors.Safe(len(buf)))
		}
		footer.footerBH.Offset = uint64(off+int64(len(buf))) - uint64(footerLen)
		buf = buf[len(buf)-footerLen:]
		footer.footerBH.Length = uint64(len(buf))
		footer.format = format

		switch block.ChecksumType(buf[0]) {
		case block.ChecksumTypeCRC32c:
			footer.checksum = block.ChecksumTypeCRC32c
		case block.ChecksumTypeXXHash64:
			footer.checksum = block.ChecksumTypeXXHash64
		default:
			return footer, base.CorruptionErrorf("(unsupported checksum type %d)", errors.Safe(footer.checksum))
		}

		if format >= TableFormatPebblev6 {
			checksumOffset := checkedPebbleDBChecksumOffset
			if format >= TableFormatPebblev7 {
				checksumOffset = pebbleDBv7FooterChecksumOffset
				footer.attributes = Attributes(binary.LittleEndian.Uint32(buf[pebbleDBV7FooterAttributesOffset:]))
			}
			encodedChecksum := binary.LittleEndian.Uint32(buf[checksumOffset:])
			computedChecksum := crc.CRC(0).
				Update(buf[:checksumOffset]).
				Update(buf[checksumOffset+checksumLen:]).
				Value()
			if encodedChecksum != computedChecksum {
				return footer, base.CorruptionErrorf("(footer corrupted, checksum mismatch)")
			}
		}

		buf = buf[1:]
	default:
		return footer, base.CorruptionErrorf("(bad magic number: 0x%x)", magic)
	}

	{
		end := uint64(size)
		var n int
		footer.metaindexBH, n = block.DecodeHandle(buf)
		if n == 0 || footer.metaindexBH.Offset+footer.metaindexBH.Length > end {
			return footer, base.CorruptionErrorf("(bad metaindex block handle)")
		}
		buf = buf[n:]

		footer.indexBH, n = block.DecodeHandle(buf)
		if n == 0 || footer.indexBH.Offset+footer.indexBH.Length > end {
			return footer, base.CorruptionErrorf("(bad index block handle)")
		}
	}

	return footer, nil
}

func (f footer) encode(buf []byte) []byte {
	footerLen := f.format.FooterSize()
	switch magic, version := f.format.AsTuple(); magic {
	case levelDBMagic:
		buf = buf[:footerLen]
		clear(buf)
		n := f.metaindexBH.EncodeVarints(buf[0:])
		f.indexBH.EncodeVarints(buf[n:])
		copy(buf[len(buf)-len(levelDBMagic):], levelDBMagic)

	case rocksDBMagic, pebbleDBMagic:
		buf = buf[:footerLen]
		clear(buf)
		switch f.checksum {
		case block.ChecksumTypeNone:
			buf[0] = byte(block.ChecksumTypeNone)
		case block.ChecksumTypeCRC32c:
			buf[0] = byte(block.ChecksumTypeCRC32c)
		case block.ChecksumTypeXXHash:
			buf[0] = byte(block.ChecksumTypeXXHash)
		case block.ChecksumTypeXXHash64:
			buf[0] = byte(block.ChecksumTypeXXHash64)
		default:
			panic("unknown checksum type")
		}
		n := 1
		n += f.metaindexBH.EncodeVarints(buf[n:])
		n += f.indexBH.EncodeVarints(buf[n:])

		binary.LittleEndian.PutUint32(buf[len(buf)-magicLen-versionLen:], version)
		copy(buf[len(buf)-magicLen:], magic)

		if f.format >= TableFormatPebblev6 {
			checksumOffset := checkedPebbleDBChecksumOffset
			if f.format >= TableFormatPebblev7 {
				checksumOffset = pebbleDBv7FooterChecksumOffset
				// Write the attributes bitset.
				binary.LittleEndian.PutUint32(buf[pebbleDBV7FooterAttributesOffset:], uint32(f.attributes))
			}

			computedChecksum := crc.CRC(0).
				Update(buf[:checksumOffset]).
				Update(buf[checksumOffset+checksumLen:]).
				Value()
			binary.LittleEndian.PutUint32(buf[checksumOffset:], computedChecksum)
		}

	default:
		panic("sstable: unspecified table format version")
	}

	return buf
}

func supportsTwoLevelIndex(format TableFormat) bool {
	switch format {
	case TableFormatLevelDB:
		return false
	case TableFormatRocksDBv2, TableFormatPebblev1, TableFormatPebblev2, TableFormatPebblev3, TableFormatPebblev4,
		TableFormatPebblev5, TableFormatPebblev6, TableFormatPebblev7:
		return true
	default:
		panic("sstable: unspecified table format version")
	}
}
