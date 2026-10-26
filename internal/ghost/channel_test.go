package ghost

import (
	"math"
	"testing"
)

// writeChannel mirrors GhostChannelCodec.WriteChannel (ghosttest has the full encoder; it imports this package).
func writeChannel(q []int64, order int) []byte {
	var out []byte
	for i := range q {
		d := q[i]
		if i >= 1 {
			d -= q[i-1]
		}
		if order == 2 && i >= 2 {
			d -= q[i-1] - q[i-2]
		}
		z := uint64((d << 1) ^ (d >> 63))
		for z >= 0x80 {
			out = append(out, byte(z)|0x80)
			z >>= 7
		}
		out = append(out, byte(z))
	}
	return out
}

// TestChannelRoundTrip checks the decoder reproduces every value, including differences that wrap int64 the way
// unchecked C# arithmetic does.
func TestChannelRoundTrip(t *testing.T) {
	values := []int64{0, 1, -1, 1000, math.MaxInt64, math.MinInt64, 5, math.MaxInt64, -math.MaxInt64, 42, 42, 43}
	for _, order := range []int{1, 2} {
		r := &reader{b: writeChannel(values, order)}
		var got []int64
		if err := readChannel(r, len(values), order, func(_ int, q int64) error {
			got = append(got, q)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if r.remaining() != 0 {
			t.Fatalf("order %d: %d bytes left", order, r.remaining())
		}
		for i := range values {
			if got[i] != values[i] {
				t.Fatalf("order %d value %d: got %d want %d", order, i, got[i], values[i])
			}
		}
	}
}

func TestVarintAndStrings(t *testing.T) {
	// A 10-byte varint is legal (bits past 64 drop), an 11th continuation byte is not.
	ten := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	if r := (&reader{b: ten}); r.varint() != math.MaxUint64 || r.err != nil {
		t.Fatalf("10-byte varint: %v", r.err)
	}
	eleven := []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0x01}
	if r := (&reader{b: eleven}); r.varint() != 0 || r.err == nil {
		t.Fatal("11-byte varint accepted")
	}
	strErr := func(b []byte) error {
		r := &reader{b: b}
		r.str()
		return r.err
	}
	// BinaryReader string lengths: 5 bytes at most, the 5th carrying 4 bits; negative lengths are invalid.
	if strErr([]byte{0x80, 0x80, 0x80, 0x80, 0x10}) == nil {
		t.Fatal("overlong 7-bit length accepted")
	}
	if strErr([]byte{0xff, 0xff, 0xff, 0xff, 0x0f}) == nil {
		t.Fatal("negative string length accepted")
	}
	if r := (&reader{b: []byte{3, 'a', 'b', 'c'}}); r.str() != "abc" || r.err != nil {
		t.Fatal("plain string")
	}
	if strErr([]byte{4, 'a'}) == nil {
		t.Fatal("truncated string accepted")
	}
}
