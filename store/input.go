package store

import (
	"errors"
	"fmt"
	"hash"
	"hash/crc32"
	"io"
	"os"
)

type DataInput struct {
	/*
		Reads and returns a single byte.
		Use panic/recover to catch IO error. Internal use only.
	*/
	ReadByte  func() byte
	ReadBytes func(buf []byte) (n int, err error)
}

type Error string

func (e Error) Error() string {
	return string(e)
}

func catchIOError(err *error) {
	if e := recover(); e != nil {
		if e2, ok := e.(Error); ok {
			*err = e2
		}
	}
}

func (in *DataInput) ReadInt() (n int, err error) {
	defer catchIOError(&err)
	ds := make([]byte, 4)
	for i, _ := range ds {
		ds[i] = in.ReadByte()
	}
	return (int(ds[0]&0xFF) << 24) | (int(ds[1]&0xFF) << 16) | (int(ds[2]&0xFF) << 8) | int(ds[3]&0xFF), nil
}

func (in *DataInput) ReadVInt() (n int, err error) {
	defer catchIOError(&err)
	b := in.ReadByte()
	if b >= 0 {
		return int(b), nil
	}
	n = int(b) & 0x7F

	b = in.ReadByte()
	n |= (int(b) & 0x7F) << 7
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int(b) & 0x7F) << 14
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int(b) & 0x7F) << 21
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	// Warning: the next ands use 0x0F / 0xF0 - beware copy/paste errors:
	n |= (int(b) & 0x0F) << 28
	if (b & 0xF0) == 0 {
		return n, nil
	}
	return 0, errors.New("Invalid vInt detected (too many bits)")
}

func (in *DataInput) ReadLong() (n int64, err error) {
	d1, err := in.ReadInt()
	if err != nil {
		return 0, err
	}
	d2, err := in.ReadInt()
	if err != nil {
		return 0, err
	}
	return (int64(d1) << 32) | (int64(d2) & 0xFFFFFFFF), nil
}

func (in *DataInput) ReadVLong() (n int64, err error) {
	defer catchIOError(&err)
	b := in.ReadByte()
	if b >= 0 {
		return int64(b), nil
	}

	n = int64(b) & 0x7F
	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 7
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 14
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 21
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 28
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 35
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 42
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 49
	if b >= 0 {
		return n, nil
	}

	b = in.ReadByte()
	n |= (int64(b) & 0x7F) << 56
	if b >= 0 {
		return n, nil
	}

	return 0, errors.New("Invalid vLong detected (negative values disallowed)")
}

func (in *DataInput) ReadString() (s string, err error) {
	length, err := in.ReadVInt()
	if err != nil {
		return "", err
	}
	bytes := make([]byte, length)
	in.ReadBytes(bytes)
	return string(bytes), nil
}

func (in *DataInput) ReadStringStringMap() (m map[string]string, err error) {
	count, err := in.ReadInt()
	if err != nil {
		return nil, err
	}
	m = make(map[string]string)
	for i := 0; i < count; i++ {
		key, err := in.ReadString()
		if err != nil {
			return nil, err
		}
		value, err := in.ReadString()
		if err != nil {
			return nil, err
		}
		m[key] = value
	}
	return m, nil
}

func (in *DataInput) ReadStringSet() (s map[string]bool, err error) {
	count, err := in.ReadInt()
	if err != nil {
		return nil, err
	}
	s = make(map[string]bool)
	for i := 0; i < count; i++ {
		key, err := in.ReadString()
		if err != nil {
			return nil, err
		}
		s[key] = true
	}
	return s, nil
}

type IndexInput struct {
	*DataInput
	desc        string
	close       func() error
	FilePointer func() int64
	Seek        func(pos int64)
	Length      func() int64
}

func newIndexInput(desc string) *IndexInput {
	if desc == "" {
		panic("resourceDescription must not be null")
	}
	super := &DataInput{}
	return &IndexInput{DataInput: super, desc: desc}
}

func (in *IndexInput) Close() error {
	return in.close()
}

type BufferedIndexInput struct {
	*IndexInput
	bufferSize     int
	buffer         []byte
	bufferStart    int64
	bufferLength   int
	bufferPosition int
	seekInternal   func(pos int64)
	readInternal   func(b []byte, offset, length int) error
}

func newBufferedIndexInput(desc string, context IOContext) *BufferedIndexInput {
	return newBufferedIndexInputBySize(desc, bufferSize(context))
}

func newBufferedIndexInputBySize(desc string, bufferSize int) *BufferedIndexInput {
	super := newIndexInput(desc)
	checkBufferSize(bufferSize)
	in := &BufferedIndexInput{IndexInput: super, bufferSize: bufferSize}
	super.ReadByte = func() byte {
		if in.bufferPosition >= in.bufferLength {
			in.refill()
		}
		b := in.buffer[in.bufferPosition]
		in.bufferPosition++
		return b
	}
	super.FilePointer = func() int64 {
		return in.bufferStart + int64(in.bufferPosition)
	}
	super.Seek = func(pos int64) {
		if pos >= in.bufferStart && pos < (in.bufferStart+int64(in.bufferLength)) {
			in.bufferPosition = int(pos - in.bufferStart)
		} else {
			in.bufferStart = pos
			in.bufferPosition = 0
			in.bufferLength = 0 // trigger refill() on read()
			in.seekInternal(pos)
		}
	}
	return in
}

func (in *BufferedIndexInput) newBuffer(newBuffer []byte) {
	// Subclasses can do something here
	in.buffer = newBuffer
}

func checkBufferSize(bufferSize int) {
	if bufferSize <= 0 {
		panic(fmt.Sprintf("bufferSize must be greater than 0 (got %v)", bufferSize))
	}
}

const (
	BUFFER_SIZE       = 1024
	MERGE_BUFFER_SIZE = 4096
)

func bufferSize(context IOContext) int {
	switch context.context {
	case IO_CONTEXT_TYPE_MERGE:
		// The normal read buffer size defaults to 1024, but
		// increasing this during merging seems to yield
		// performance gains.  However we don't want to increase
		// it too much because there are quite a few
		// BufferedIndexInputs created during merging.  See
		// LUCENE-888 for details.
		return MERGE_BUFFER_SIZE
	default:
		return BUFFER_SIZE
	}
}

// use panic/recover to handle error
func (in *BufferedIndexInput) refill() error {
	start := in.bufferStart + int64(in.bufferPosition)
	end := start + int64(in.bufferSize)
	if end > in.Length() { // don't read past EOF
		end = in.Length()
	}
	newLength := int(end - start)
	if newLength <= 0 {
		return errors.New(fmt.Sprintf("read past EOF: %v", in))
	}

	if in.buffer == nil {
		in.newBuffer(make([]byte, in.bufferSize)) // allocate buffer lazily
		in.seekInternal(int64(in.bufferStart))
	}
	in.readInternal(in.buffer, 0, newLength)
	in.bufferLength = newLength
	in.bufferStart = start
	in.bufferPosition = 0
	return nil
}

type FSIndexInput struct {
	*BufferedIndexInput
	file      *os.File
	isClone   bool
	chunkSize int
	off       int64
	end       int64
}

func newFSIndexInput(desc, path string, context IOContext, chunkSize int) (in *FSIndexInput, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	super := newBufferedIndexInput(desc, context)
	in = &FSIndexInput{super, f, false, chunkSize, 0, fi.Size()}
	super.Length = func() int64 {
		return in.end - in.off
	}
	super.close = func() error {
		// only close the file if this is not a clone
		if !in.isClone {
			in.file.Close()
		}
		return nil
	}
	return in, nil
}

type ChecksumIndexInput struct {
	*IndexInput
	main   *IndexInput
	digest hash.Hash32
}

func NewChecksumIndexInput(main *IndexInput) *ChecksumIndexInput {
	super := newIndexInput(fmt.Sprintf("ChecksumIndexInput(%v)", main))
	digest := crc32.NewIEEE()
	super.ReadByte = func() byte {
		b := main.ReadByte()
		digest.Write([]byte{b})
		return b
	}
	super.ReadBytes = func(buf []byte) (n int, err error) {
		n, err = main.ReadBytes(buf)
		if err == nil {
			digest.Write(buf)
		}
		return n, err
	}
	super.FilePointer = main.FilePointer
	super.Length = main.Length
	return &ChecksumIndexInput{super, main, digest}
}

func (in *ChecksumIndexInput) Checksum() int64 {
	return int64(in.digest.Sum32())
}

type IndexInputSlicer interface {
	io.Closer
	openSlice(desc string, offset, length int64) IndexInput
	openFullSlice() IndexInput
}

type SlicedIndexInput struct {
	*BufferedIndexInput
	base       IndexInput
	fileOffset int64
	length     int64
}

func newSlicedIndexInput(desc string, base *IndexInput, fileOffset, length int64) SlicedIndexInput {
	return newSlicedIndexInputBySize(desc, base, fileOffset, length, BUFFER_SIZE)
}

func newSlicedIndexInputBySize(desc string, base *IndexInput, fileOffset, length int64, bufferSize int) SlicedIndexInput {
	return SlicedIndexInput{
		BufferedIndexInput: newBufferedIndexInputBySize(fmt.Sprintf(
			"SlicedIndexInput(%v in %v slice=%v:%v)", desc, base, fileOffset, fileOffset+length), bufferSize),
		base: *base, fileOffset: fileOffset, length: length}
}
