package wire

import (
	"bytes"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	now := time.UnixMilli(1_761_000_000_123).UTC()
	w := NewWriter(0)
	w.U8(0xAB)
	w.U16(0xBEEF)
	w.U32(0xDEADBEEF)
	w.U64(math.MaxUint64 - 1)
	w.I32(-5)
	w.I64(math.MinInt64)
	w.F32(-1.5)
	w.Bool(true)
	w.Bool(false)
	w.VarUint(0)
	w.VarUint(127)
	w.VarUint(128)
	w.VarUint(MaxVarUint)
	w.String("Hühnchen")
	w.Raw([]byte{1, 2, 3})
	w.Count(2)
	w.Time(now)

	r := NewReader(w.Bytes())
	check := func(name string, got, want any) {
		t.Helper()
		if got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	check("u8", r.U8(), uint8(0xAB))
	check("u16", r.U16(), uint16(0xBEEF))
	check("u32", r.U32(), uint32(0xDEADBEEF))
	check("u64", r.U64(), uint64(math.MaxUint64-1))
	check("i32", r.I32(), int32(-5))
	check("i64", r.I64(), int64(math.MinInt64))
	check("f32", r.FiniteF32(), float32(-1.5))
	check("true", r.Bool(), true)
	check("false", r.Bool(), false)
	check("var0", r.VarUint(), uint32(0))
	check("var127", r.VarUint(), uint32(127))
	check("var128", r.VarUint(), uint32(128))
	check("varmax", r.VarUint(), uint32(MaxVarUint))
	check("string", r.String(64), "Hühnchen")
	if b := r.Raw(8); !bytes.Equal(b, []byte{1, 2, 3}) {
		t.Errorf("raw = %v", b)
	}
	check("count", r.Count(10, 1), 2)
	// Count only checks the items fit in what remains: the 8-byte time is there.
	check("time", r.Time(), now)
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	check("remaining", r.Remaining(), 0)
}

func TestLittleEndianLayout(t *testing.T) {
	w := NewWriter(0)
	w.U16(0x0102)
	w.U32(0x03040506)
	w.VarUint(300) // 0xAC 0x02
	w.String("ok")
	want := []byte{0x02, 0x01, 0x06, 0x05, 0x04, 0x03, 0xAC, 0x02, 0x02, 'o', 'k'}
	if !bytes.Equal(w.Bytes(), want) {
		t.Fatalf("got % x, want % x", w.Bytes(), want)
	}
}

func TestHostileInputs(t *testing.T) {
	cases := []struct {
		name string
		in   []byte
		read func(r *Reader)
		want error
	}{
		{"empty u32", nil, func(r *Reader) { r.U32() }, ErrTruncated},
		{"short u64", []byte{1, 2, 3}, func(r *Reader) { r.U64() }, ErrTruncated},
		{"bool 2", []byte{2}, func(r *Reader) { r.Bool() }, ErrInvalid},
		{"bool 255", []byte{255}, func(r *Reader) { r.Bool() }, ErrInvalid},
		{"varuint 6 bytes", []byte{0x80, 0x80, 0x80, 0x80, 0x80, 0x01}, func(r *Reader) { r.VarUint() }, ErrInvalid},
		{"varuint above int32", []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x0F}, func(r *Reader) { r.VarUint() }, ErrInvalid},
		{"varuint 2^31", []byte{0x80, 0x80, 0x80, 0x80, 0x08}, func(r *Reader) { r.VarUint() }, ErrInvalid},
		{"varuint truncated", []byte{0x80}, func(r *Reader) { r.VarUint() }, ErrTruncated},
		{"string over max", append([]byte{5}, "hello"...), func(r *Reader) { r.String(4) }, ErrTooLong},
		{"string beyond input", []byte{10, 'a'}, func(r *Reader) { r.String(64) }, ErrTruncated},
		{"huge string length", []byte{0xFF, 0xFF, 0xFF, 0xFF, 0x07}, func(r *Reader) { r.String(math.MaxInt32) }, ErrTruncated},
		{"bad utf8", []byte{2, 0xC3, 0x28}, func(r *Reader) { r.String(64) }, ErrInvalid},
		{"overlong utf8", []byte{2, 0xC0, 0xAF}, func(r *Reader) { r.String(64) }, ErrInvalid},
		{"surrogate utf8", []byte{3, 0xED, 0xA0, 0x80}, func(r *Reader) { r.String(64) }, ErrInvalid},
		{"bytes beyond input", []byte{3, 1}, func(r *Reader) { r.Raw(64) }, ErrTruncated},
		{"bytes over max", []byte{3, 1, 2, 3}, func(r *Reader) { r.Raw(2) }, ErrTooLong},
		{"count over max", []byte{11}, func(r *Reader) { r.Count(10, 1) }, ErrTooLong},
		{"count beyond input", []byte{3, 0, 0}, func(r *Reader) { r.Count(10, 1) }, ErrTruncated},
		{"count with item size", []byte{2, 0, 0, 0, 0}, func(r *Reader) { r.Count(10, 4) }, ErrTruncated},
		{"nan", []byte{0x00, 0x00, 0xC0, 0x7F}, func(r *Reader) { r.FiniteF32() }, ErrInvalid},
		{"+inf", []byte{0x00, 0x00, 0x80, 0x7F}, func(r *Reader) { r.FiniteF32() }, ErrInvalid},
		{"-inf", []byte{0x00, 0x00, 0x80, 0xFF}, func(r *Reader) { r.FiniteF32() }, ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(tc.in)
			tc.read(r)
			if !errors.Is(r.Err(), tc.want) {
				t.Fatalf("err = %v, want %v", r.Err(), tc.want)
			}
			// Errors are sticky and later reads return zero values.
			if v := r.U8(); v != 0 || !errors.Is(r.Err(), tc.want) {
				t.Fatalf("read after error = %d, err %v", v, r.Err())
			}
		})
	}
}

func TestRawF32AllowsNaN(t *testing.T) {
	r := NewReader([]byte{0x00, 0x00, 0xC0, 0x7F})
	if v := r.F32(); !math.IsNaN(float64(v)) || r.Err() != nil {
		t.Fatalf("F32 = %v, err %v", v, r.Err())
	}
}

func TestRawCopies(t *testing.T) {
	in := []byte{2, 7, 8}
	b := NewReader(in).Raw(8)
	in[1] = 0
	if b[0] != 7 {
		t.Fatal("Raw aliases the input")
	}
}

func TestStringAtLimit(t *testing.T) {
	s := strings.Repeat("ä", 10) // 20 bytes
	w := NewWriter(0)
	w.String(s)
	r := NewReader(w.Bytes())
	if got := r.String(20); got != s || r.Err() != nil {
		t.Fatalf("got %q, err %v", got, r.Err())
	}
	r = NewReader(w.Bytes())
	r.String(19)
	if !errors.Is(r.Err(), ErrTooLong) {
		t.Fatalf("limit counts bytes, err = %v", r.Err())
	}
}

func TestFrame(t *testing.T) {
	frame := AppendFrame(nil, 1, 0x0203, 0x04050607, []byte{9, 9})
	want := []byte{9, 0, 0, 0, 1, 0x03, 0x02, 0x07, 0x06, 0x05, 0x04, 9, 9}
	if !bytes.Equal(frame, want) {
		t.Fatalf("frame % x, want % x", frame, want)
	}
	f, err := ReadFrame(bytes.NewReader(frame), MaxFramePreLogin)
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != 1 || f.Type != 0x0203 || f.ID != 0x04050607 || !bytes.Equal(f.Body, []byte{9, 9}) {
		t.Fatalf("decoded %+v", f)
	}
}

func TestFrameLimits(t *testing.T) {
	over := []byte{0x01, 0x00, 0x01, 0x00} // 65537 bytes announced, none sent
	if _, err := ReadFrame(bytes.NewReader(over), MaxFramePreLogin); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	huge := []byte{0xFF, 0xFF, 0xFF, 0xFF}
	if _, err := ReadFrame(bytes.NewReader(huge), MaxFrame); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("err = %v, want ErrFrameTooLarge", err)
	}
	short := []byte{3, 0, 0, 0, 0, 0, 0}
	if _, err := ReadFrame(bytes.NewReader(short), MaxFrame); !errors.Is(err, ErrFrameTooShort) {
		t.Fatalf("err = %v, want ErrFrameTooShort", err)
	}
	truncated := []byte{20, 0, 0, 0, 0, 1, 0}
	if _, err := ReadFrame(bytes.NewReader(truncated), MaxFrame); err == nil {
		t.Fatal("truncated frame accepted")
	}
	exact := AppendFrame(nil, 0, 1, 1, make([]byte, MaxFramePreLogin-HeaderSize))
	if _, err := ReadFrame(bytes.NewReader(exact), MaxFramePreLogin); err != nil {
		t.Fatalf("frame at the limit: %v", err)
	}
}

// FuzzReader feeds arbitrary bytes through every reader method; it must never panic. `go test` runs the seeds.
func FuzzReader(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Add([]byte{5, 'h', 'e', 'l', 'l', 'o', 1, 0x80, 0x01})
	f.Add([]byte{2, 0xC3, 0x28, 2, 9})
	f.Fuzz(func(t *testing.T, data []byte) {
		for op := range 12 {
			r := NewReader(data)
			for r.Err() == nil && r.Remaining() > 0 {
				switch op {
				case 0:
					r.U8()
				case 1:
					r.U16()
				case 2:
					r.U32()
				case 3:
					r.U64()
				case 4:
					r.FiniteF32()
				case 5:
					r.Bool()
				case 6:
					r.VarUint()
				case 7:
					r.String(DefaultMaxString)
				case 8:
					r.Raw(1 << 20)
				case 9:
					if n := r.Count(1000, 2); r.Err() == nil {
						_ = make([]uint16, n)
						for range n {
							r.U16()
						}
					}
				case 10:
					r.Time()
				case 11:
					r.String(math.MaxInt32)
				}
			}
		}
	})
}
