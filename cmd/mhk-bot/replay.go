package main

import (
	"math"
	"time"

	"github.com/mhk-os/server/internal/ghost"
	"github.com/mhk-os/server/internal/proto"
)

// replay turns a ghost into the kart states a racing client would send. Ghosts carry no track data, so
// raceDistance is the distance driven along the recorded path and lap counts follow the recorded lap times.
type replay struct {
	g       *ghost.Summary
	frames  []ghost.Frame
	dist    []float32 // metres driven up to each frame
	lapEnds []time.Duration
}

func newReplay(g *ghost.Summary, frames []ghost.Frame) *replay {
	r := &replay{g: g, frames: frames, dist: make([]float32, len(frames))}
	for i := 1; i < len(frames); i++ {
		d := 0.0
		if frames[i].Flags&ghost.FrameCut == 0 { // a respawn teleport is not driven distance
			for c := range 3 {
				x := float64(frames[i].Pos[c] - frames[i-1].Pos[c])
				d += x * x
			}
		}
		r.dist[i] = r.dist[i-1] + float32(math.Sqrt(d))
	}
	sum := float32(0)
	for _, l := range g.LapTimes {
		sum += l
	}
	end := g.FinishTime - sum // run-up from GO to the first line crossing
	for _, l := range g.LapTimes {
		end += l
		r.lapEnds = append(r.lapEnds, seconds(end))
	}
	return r
}

func seconds(s float32) time.Duration { return time.Duration(float64(s) * float64(time.Second)) }

// finish is the ghost's finish time after GO.
func (r *replay) finish() time.Duration { return time.Duration(r.g.FinishMs) * time.Millisecond }

// state is the kart state at race time t (negative: before GO, parked on the first frame).
func (r *replay) state(t time.Duration) proto.KartState {
	var st proto.KartState
	i := 0
	if t > 0 {
		st.Tick = uint32(t * 60 / time.Second)
		// The epsilon keeps float32 tick lengths (1/60 is not exact) from landing one frame early.
		i = min(int(t.Seconds()/float64(r.g.TickSeconds)+1e-3), len(r.frames)-1)
	}
	if len(r.frames) == 0 {
		return st
	}
	f := &r.frames[i]
	st.Pos = proto.Vec3(f.Pos)
	for c := range 4 {
		st.Rot[c] = int16(math.Round(float64(f.Rot[c]) * 32767))
	}
	for c := range 3 {
		st.Vel[c] = int16(max(min(math.Round(float64(f.Vel[c])*100), 32767), -32767))
	}
	st.Keys = uint8(f.Keys)
	if f.Flags&ghost.FrameCut != 0 {
		st.Flags |= proto.KartCut
	}
	if f.Flags&ghost.FrameControlsEnabled != 0 && t > 0 {
		st.Flags |= proto.KartControls
	}
	if f.Flags&ghost.FrameRaceOver != 0 || t >= r.finish() {
		st.Flags |= proto.KartFinished
	}
	if f.Flags&ghost.FrameLookBack != 0 {
		st.Flags |= proto.KartLookBack
	}
	st.Steer = int8(max(min(math.Round(float64(f.InputSteer)*127), 127), -127))
	st.Lap = 1
	for _, end := range r.lapEnds {
		if t >= end && int(st.Lap) < len(r.lapEnds) {
			st.Lap++
		}
	}
	st.RaceDistance = r.dist[i]
	st.Health = 100
	return st
}
