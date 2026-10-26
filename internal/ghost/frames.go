package ghost

import "math"

// Quantisation scales of GhostBodyCodec.cs for the channels Frame keeps.
const (
	rotScale     = 1e6
	velScale     = 1000
	angleScale   = 1000
	commandScale = 10000
	inputScale   = 1000
)

// GhostFrameFlags bits (GhostFrame.cs).
const (
	FrameControlsEnabled = 1
	FrameRaceOver        = 2
	FrameLookBack        = 4
	FrameBurned          = 8
	FrameCut             = 16
)

// Frame is one tick of a ghost: the kart state the game recorded, minus the visual-only channels.
type Frame struct {
	Pos          [3]float32 // m
	Rot          [4]float32 // unit quaternion x, y, z, w
	Vel          [3]float32 // m/s
	ForwardSpeed float32    // m/s
	SteerAngle   float32
	SteerCommand float32
	InputSteer   float32 // −1..1
	Flags        uint32  // GhostFrameFlags
	Keys         uint32  // GhostInputKeys
}

// keepChannel adapts a frame setter to readChannel's check hook; nil when frames are not kept.
func keepChannel(frames []Frame, set func(f *Frame, q int64)) func(i int, q int64) error {
	if frames == nil || set == nil {
		return nil
	}
	return func(i int, q int64) error {
		set(&frames[i], q)
		return nil
	}
}

// firstOrderSetter maps first-order channel c (velocity x, y, z, then the scalars in GhostBodyCodec order) to the
// Frame field it fills; nil for visual channels.
func firstOrderSetter(c int) func(f *Frame, q int64) {
	switch c {
	case 0, 1, 2:
		return func(f *Frame, q int64) { f.Vel[c] = float32(q) / velScale }
	case 3:
		return func(f *Frame, q int64) { f.ForwardSpeed = float32(q) / velScale }
	case 4:
		return func(f *Frame, q int64) { f.SteerAngle = float32(q) / angleScale }
	case 5:
		return func(f *Frame, q int64) { f.SteerCommand = float32(q) / commandScale }
	case 6:
		return func(f *Frame, q int64) { f.InputSteer = float32(q) / inputScale }
	case 9:
		return func(f *Frame, q int64) { f.Flags = uint32(q) }
	case 10:
		return func(f *Frame, q int64) { f.Keys = uint32(q) }
	}
	return nil
}

// normaliseRotation mirrors GhostBodyCodec.ReadRotations: unit length, identity when degenerate.
func (f *Frame) normaliseRotation() {
	var m float64
	for _, c := range f.Rot {
		m += float64(c) * float64(c)
	}
	m = math.Sqrt(m)
	if m <= 1e-6 {
		f.Rot = [4]float32{0, 0, 0, 1}
		return
	}
	for i := range f.Rot {
		f.Rot[i] = float32(float64(f.Rot[i]) / m)
	}
}
