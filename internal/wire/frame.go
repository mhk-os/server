// Package wire implements the framing and primitive encoding of the MHK:OS protocol (docs/protocol.md).
// It knows nothing about message types; internal/proto builds the messages on top of it.
package wire

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const (
	// HeaderSize is kind (u8) + type (u16) + id (u32), the part of every frame after the length prefix.
	HeaderSize = 7
	// MaxFramePreLogin bounds frames before login so unauthenticated peers cannot make the server buffer much.
	MaxFramePreLogin = 64 << 10
	// MaxFrame bounds frames after login (ghost uploads in phase 2 are the largest legitimate payloads).
	MaxFrame = 16 << 20
)

var (
	// ErrFrameTooLarge means the length prefix exceeded the allowed maximum; the connection must be closed.
	ErrFrameTooLarge = errors.New("wire: frame too large")
	// ErrFrameTooShort means the length prefix cannot even hold the frame header.
	ErrFrameTooShort = errors.New("wire: frame shorter than header")
)

// Frame is one decoded frame. Body aliases the buffer it was read into.
type Frame struct {
	Kind uint8
	Type uint16
	ID   uint32
	Body []byte
}

// ReadFrame reads one frame whose length (the bytes after the u32 prefix) must not exceed max.
// The body buffer is allocated only after the length passed the limit check.
func ReadFrame(r io.Reader, max int) (Frame, error) {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return Frame{}, err
	}
	n := binary.LittleEndian.Uint32(prefix[:])
	if uint64(n) > uint64(max) {
		return Frame{}, fmt.Errorf("%w: %d > %d bytes", ErrFrameTooLarge, n, max)
	}
	if n < HeaderSize {
		return Frame{}, ErrFrameTooShort
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return Frame{}, err
	}
	return Frame{
		Kind: buf[0],
		Type: binary.LittleEndian.Uint16(buf[1:3]),
		ID:   binary.LittleEndian.Uint32(buf[3:7]),
		Body: buf[HeaderSize:],
	}, nil
}

// AppendFrame appends a complete frame (length prefix, header, body) to dst.
func AppendFrame(dst []byte, kind uint8, typ uint16, id uint32, body []byte) []byte {
	dst = binary.LittleEndian.AppendUint32(dst, uint32(HeaderSize+len(body)))
	dst = append(dst, kind)
	dst = binary.LittleEndian.AppendUint16(dst, typ)
	dst = binary.LittleEndian.AppendUint32(dst, id)
	return append(dst, body...)
}
