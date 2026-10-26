package ghost_test

import (
	"math"
	"testing"

	"github.com/mhk-os/server/internal/ghost"
	"github.com/mhk-os/server/internal/ghost/ghosttest"
)

func TestParseFrames(t *testing.T) {
	g := ghosttest.New()
	n := int(g.Header.FrameCount)
	for i := range n {
		g.Channels[7][i] = 12345         // velocity x: 12.345 m/s
		g.Channels[13][i] = -500         // inputSteer −0.5
		g.Channels[16][i] = int64(i % 3) // flags
		g.Channels[17][i] = 4 | 1        // keys: throttle, steer left
	}
	s, frames, err := ghost.ParseFrames(g.Encode())
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) != n || s.FinishMs != 90500 {
		t.Fatalf("%d frames, finish %d", len(frames), s.FinishMs)
	}
	f := frames[450] // a quarter lap: angle π/2
	if math.Abs(float64(f.Pos[0])-1000) > 1e-3 || f.Pos[1] != 5 || math.Abs(float64(f.Pos[2])+2800) > 1e-3 {
		t.Fatalf("position %v", f.Pos)
	}
	if want := float32(math.Sin(math.Pi / 4)); math.Abs(float64(f.Rot[1]-want)) > 1e-5 || math.Abs(float64(f.Rot[3]-want)) > 1e-5 {
		t.Fatalf("rotation %v", f.Rot)
	}
	if f.Vel[0] != 12.345 || f.InputSteer != -0.5 || f.Flags != 0 || frames[451].Flags != 1 || f.Keys != 5 {
		t.Fatalf("frame %+v", f)
	}
	if _, err := ghost.Parse(g.Encode()); err != nil {
		t.Fatal(err)
	}
}
