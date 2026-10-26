package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
	"unicode/utf8"
)

const (
	// MaxVarUint is the largest value a varuint may carry (lengths and counts fit a signed 32-bit int on every client).
	MaxVarUint = math.MaxInt32
	// DefaultMaxString is the per-field string limit when a message field does not specify one.
	DefaultMaxString = 256
	maxVarUintBytes  = 5
)

var (
	// ErrTruncated means the body ended before a field was complete.
	ErrTruncated = errors.New("wire: truncated")
	// ErrInvalid means a field held a value its type does not allow.
	ErrInvalid = errors.New("wire: invalid value")
	// ErrTooLong means a string, bytes or list exceeded its per-field maximum.
	ErrTooLong = errors.New("wire: over field limit")
)

// Writer appends primitives to a growing buffer. It cannot fail; limits are the caller's responsibility.
type Writer struct {
	buf []byte
}

// NewWriter returns a Writer with capacity hint n.
func NewWriter(n int) *Writer { return &Writer{buf: make([]byte, 0, n)} }

// Bytes returns the encoded bytes (aliases the internal buffer).
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of encoded bytes.
func (w *Writer) Len() int { return len(w.buf) }

func (w *Writer) U8(v uint8)   { w.buf = append(w.buf, v) }
func (w *Writer) U16(v uint16) { w.buf = binary.LittleEndian.AppendUint16(w.buf, v) }
func (w *Writer) U32(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }
func (w *Writer) U64(v uint64) { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }
func (w *Writer) I32(v int32)  { w.U32(uint32(v)) }
func (w *Writer) I64(v int64)  { w.U64(uint64(v)) }
func (w *Writer) F32(v float32) {
	w.U32(math.Float32bits(v))
}

func (w *Writer) Bool(v bool) {
	if v {
		w.U8(1)
	} else {
		w.U8(0)
	}
}

// VarUint writes v as unsigned LEB128. Values above MaxVarUint violate the protocol and panic: that is a server bug.
func (w *Writer) VarUint(v uint32) {
	if v > MaxVarUint {
		panic(fmt.Sprintf("wire: varuint %d exceeds 2^31-1", v))
	}
	for v >= 0x80 {
		w.buf = append(w.buf, byte(v)|0x80)
		v >>= 7
	}
	w.buf = append(w.buf, byte(v))
}

func (w *Writer) String(s string) {
	w.VarUint(uint32(len(s)))
	w.buf = append(w.buf, s...)
}

func (w *Writer) Raw(b []byte) {
	w.VarUint(uint32(len(b)))
	w.buf = append(w.buf, b...)
}

// Count writes a list count; the items follow through the other methods.
func (w *Writer) Count(n int) { w.VarUint(uint32(n)) }

// Time writes t as i64 Unix milliseconds.
func (w *Writer) Time(t time.Time) { w.I64(t.UnixMilli()) }

// Reader decodes primitives from a body. The first failure is sticky: later calls return zero values and Err
// reports it, so decoders read all fields and check once. No method panics or allocates more than the input holds.
type Reader struct {
	b   []byte
	off int
	err error
}

// NewReader returns a Reader over b.
func NewReader(b []byte) *Reader { return &Reader{b: b} }

// Err returns the first decoding error.
func (r *Reader) Err() error { return r.err }

// Remaining returns the number of unread bytes. Trailing bytes are legal (bodies are append-only).
func (r *Reader) Remaining() int { return len(r.b) - r.off }

// Fail records err (with field context) unless an earlier error is already recorded.
func (r *Reader) Fail(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *Reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > r.Remaining() {
		r.err = ErrTruncated
		return nil
	}
	s := r.b[r.off : r.off+n]
	r.off += n
	return s
}

func (r *Reader) U8() uint8 {
	if b := r.take(1); b != nil {
		return b[0]
	}
	return 0
}

func (r *Reader) U16() uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func (r *Reader) U32() uint32 {
	if b := r.take(4); b != nil {
		return binary.LittleEndian.Uint32(b)
	}
	return 0
}

func (r *Reader) U64() uint64 {
	if b := r.take(8); b != nil {
		return binary.LittleEndian.Uint64(b)
	}
	return 0
}

func (r *Reader) I32() int32 { return int32(r.U32()) }
func (r *Reader) I64() int64 { return int64(r.U64()) }

// F32 returns the raw float, NaN and infinities included; use FiniteF32 for fields that forbid them (the default).
func (r *Reader) F32() float32 { return math.Float32frombits(r.U32()) }

// FiniteF32 reads an f32 and rejects NaN and ±Inf.
func (r *Reader) FiniteF32() float32 {
	v := r.F32()
	if r.err == nil && (math.IsNaN(float64(v)) || math.IsInf(float64(v), 0)) {
		r.err = fmt.Errorf("%w: non-finite f32", ErrInvalid)
		return 0
	}
	return v
}

// Bool accepts only 0 and 1.
func (r *Reader) Bool() bool {
	b := r.take(1)
	if b == nil {
		return false
	}
	switch b[0] {
	case 0:
		return false
	case 1:
		return true
	}
	r.err = fmt.Errorf("%w: bool %d", ErrInvalid, b[0])
	return false
}

// VarUint reads unsigned LEB128 of at most 5 bytes with a value of at most 2^31-1.
func (r *Reader) VarUint() uint32 {
	var v uint64
	for i := range maxVarUintBytes {
		b := r.take(1)
		if b == nil {
			return 0
		}
		v |= uint64(b[0]&0x7f) << (7 * i)
		if b[0]&0x80 == 0 {
			if v > MaxVarUint {
				r.err = fmt.Errorf("%w: varuint above 2^31-1", ErrInvalid)
				return 0
			}
			return uint32(v)
		}
	}
	r.err = fmt.Errorf("%w: varuint longer than %d bytes", ErrInvalid, maxVarUintBytes)
	return 0
}

// length reads a varuint length, checks it against max and against the remaining input.
func (r *Reader) length(max int) int {
	n := int(r.VarUint())
	if r.err != nil {
		return 0
	}
	if n > max {
		r.err = fmt.Errorf("%w: %d > %d", ErrTooLong, n, max)
		return 0
	}
	if n > r.Remaining() {
		r.err = ErrTruncated
		return 0
	}
	return n
}

// String reads a UTF-8 string of at most max bytes.
func (r *Reader) String(max int) string {
	n := r.length(max)
	b := r.take(n)
	if r.err != nil {
		return ""
	}
	if !utf8.Valid(b) {
		r.err = fmt.Errorf("%w: invalid UTF-8", ErrInvalid)
		return ""
	}
	return string(b)
}

// Raw reads a bytes field of at most max bytes. The result is a copy, safe to keep after the frame buffer is reused.
func (r *Reader) Raw(max int) []byte {
	n := r.length(max)
	b := r.take(n)
	if r.err != nil {
		return nil
	}
	return append([]byte(nil), b...)
}

// Count reads a list count of at most max items. minItemSize (>= 1 for every real item type) lets the reader reject
// counts the remaining input cannot possibly hold, so callers can preallocate len == count safely.
func (r *Reader) Count(max, minItemSize int) int {
	n := int(r.VarUint())
	if r.err != nil {
		return 0
	}
	if n > max {
		r.err = fmt.Errorf("%w: count %d > %d", ErrTooLong, n, max)
		return 0
	}
	if minItemSize < 1 {
		minItemSize = 1
	}
	if n > r.Remaining()/minItemSize {
		r.err = ErrTruncated
		return 0
	}
	return n
}

// Time reads i64 Unix milliseconds as a UTC time.
func (r *Reader) Time() time.Time {
	ms := r.I64()
	if r.err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}
