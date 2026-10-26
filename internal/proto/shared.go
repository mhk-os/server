package proto

import (
	"fmt"
	"math"

	"github.com/mhk-os/server/internal/wire"
)

// Limits shared by lobby and race messages (docs/protocol.md, phase 3).
const (
	MaxItemOdds   = 64
	itemOddsMinSz = 1 + 4
)

// ItemOdds is one entry of custom item odds (RoomSettings.itemOdds, RaceSetup.itemOdds): a powerup and its share in
// percent. An empty list means the game's default odds.
type ItemOdds struct {
	PowerupID string
	Percent   float32
}

// EncodeItemOdds writes list<(powerupId: string ≤ 96, percent: f32)>.
func EncodeItemOdds(w *wire.Writer, odds []ItemOdds) {
	w.Count(len(odds))
	for _, o := range odds {
		w.String(o.PowerupID)
		w.F32(o.Percent)
	}
}

// DecodeItemOdds reads list<(powerupId, percent)>; percents must be finite and within 0–100.
func DecodeItemOdds(r *wire.Reader) []ItemOdds {
	n := r.Count(MaxItemOdds, itemOddsMinSz)
	if n == 0 {
		return nil
	}
	odds := make([]ItemOdds, n)
	for i := range odds {
		odds[i].PowerupID = r.String(MaxContentID)
		odds[i].Percent = r.FiniteF32()
		if r.Err() == nil && (odds[i].Percent < 0 || odds[i].Percent > 100 || math.IsNaN(float64(odds[i].Percent))) {
			r.Fail(fmt.Errorf("%w: item odds percent %v outside 0-100", wire.ErrInvalid, odds[i].Percent))
		}
	}
	return odds
}
