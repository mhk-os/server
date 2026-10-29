package race

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"time"

	"github.com/mhk-os/server/internal/proto"
)

// projectile tracks the reports about one item use (docs/protocol.md, "Projectiles").
type projectile struct {
	pending     map[uint8]pendingReport // latest report per reporter of the current window
	windowStart time.Time               // arrival of the window's first report
	lastAt      time.Time               // the use, or the latest report
	reporters   map[uint8]bool          // everyone who ever reported it
	gone        map[uint8]bool          // ProjectileGone senders
	struck      map[uint8]bool          // reporters already struck as outliers for this projectile
}

func newProjectile(now time.Time) *projectile {
	return &projectile{pending: map[uint8]pendingReport{}, lastAt: now, reporters: map[uint8]bool{},
		gone: map[uint8]bool{}, struck: map[uint8]bool{}}
}

// addReport files a report into the current window; a window lasts one report period (0.5 s) so it holds one
// report of every live reporter whatever their phase.
func (p *projectile) addReport(slot uint8, r pendingReport, now time.Time) {
	if len(p.pending) == 0 {
		p.windowStart = now
	}
	p.pending[slot] = r
	p.reporters[slot] = true
	p.lastAt = now
}

type pendingReport struct {
	proto.ProjectileReport
	eligible bool // reporter eligible when it reported
}

// onProjectileGone ends tracking when the owner (the AI host for an AI kart's use) or a majority of reporters
// say the projectile is gone.
func (rs *raceState) onProjectileGone(s *seat, m *proto.ProjectileGone, now time.Time) {
	if !rs.arbitrated() {
		return
	}
	u := rs.uses[m.UseID]
	if u == nil || u.proj == nil {
		return
	}
	p := u.proj
	p.gone[s.slot] = true
	owner := s.slot == u.owner || (rs.seats[u.owner].ai && s == rs.aiHost)
	ended := owner || 2*len(p.gone) > len(p.reporters)
	rs.debug("projectile gone", s, "use", m.UseID, "gone", len(p.gone), "reporters", len(p.reporters), "owner", owner,
		"ended", ended)
	if ended {
		rs.untrack(m.UseID, u)
	}
}

func (rs *raceState) untrack(useID uint32, u *use) {
	u.proj = nil
	delete(rs.live, useID)
}

// tickProjectiles judges each live projectile's report window once it is 0.5 s old. Projectiles nobody reported
// for projectileSilence are gone without anyone saying so (their reporters left, or missed the end).
func (rs *raceState) tickProjectiles(now time.Time) {
	for useID, u := range rs.live {
		p := u.proj
		if len(p.pending) == 0 {
			if now.Sub(p.lastAt) >= projectileSilence {
				rs.debug("projectile silent, tracking ended", nil, "use", useID)
				rs.untrack(useID, u)
			}
			continue
		}
		if now.Sub(p.windowStart) < projectileEvery {
			continue
		}
		reports := make([]pendingReport, 0, len(p.pending))
		slots := make([]uint8, 0, len(p.pending))
		for slot, r := range p.pending {
			reports = append(reports, r)
			slots = append(slots, slot)
		}
		clear(p.pending)
		res := judgeProjectile(reports)
		if rs.debugging() {
			rs.debug("projectile judged", nil, "use", useID, "reports", len(reports), "slots", fmt.Sprint(slots),
				"tick", res.tick,
				"median", res.pos, "vel", res.vel, "corrected", res.correct, "outliers", len(res.outliers))
		}
		if res.correct {
			rs.pushAll(proto.PushProjectileCorrect, &proto.ProjectileCorrect{RaceID: rs.id, UseID: useID,
				Tick: res.tick, Pos: res.pos, Vel: res.vel}, nil)
		}
		// An AI kart's projectile flies in every client's own copy of the race (AI karts diverge by design): its
		// reports are corrected towards the median but a far one is never a strike.
		if rs.seats[u.owner].ai {
			continue
		}
		for _, i := range res.outliers {
			if s := rs.seats[slots[i]]; !p.struck[s.slot] {
				p.struck[s.slot] = true
				rs.debug("projectile outlier", s, "use", useID, "pos", reports[i].Pos, "tick", reports[i].Tick)
				rs.strike(s, "projectile report far from the others", now)
			}
		}
	}
}

type projectileJudgement struct {
	tick     uint32
	pos, vel proto.Vec3
	correct  bool  // some report is more than 2 m from the median
	outliers []int // indexes of eligible outlier reports
}

// judgeProjectile extrapolates every report to the newest tick and takes the component-wise median. When at least
// 3 eligible reporters agree, the median uses only them (lagging reporters cannot drag it) and eligible reports
// farther than max(4 m, 3 × MAD) are outliers.
func judgeProjectile(reports []pendingReport) projectileJudgement {
	var j projectileJudgement
	if len(reports) == 0 {
		return j
	}
	for _, r := range reports {
		j.tick = max(j.tick, r.Tick)
	}
	pos := make([]proto.Vec3, len(reports))
	for i, r := range reports {
		dt := float32(int64(j.tick)-int64(r.Tick)) / ticksPerSecond
		for c := range 3 {
			pos[i][c] = r.Pos[c] + r.Vel[c]*dt
		}
	}
	var basis []int
	for i, r := range reports {
		if r.eligible {
			basis = append(basis, i)
		}
	}
	judgeOutliers := len(basis) >= 3
	if len(basis) < 2 {
		basis = basis[:0]
		for i := range reports {
			basis = append(basis, i)
		}
	}
	vals := make([]float64, len(basis))
	for c := range 3 {
		for k, i := range basis {
			vals[k] = float64(pos[i][c])
		}
		j.pos[c] = float32(median(vals))
		for k, i := range basis {
			vals[k] = float64(reports[i].Vel[c])
		}
		j.vel[c] = float32(median(vals))
	}
	if len(reports) < 2 {
		return j
	}
	dists := make([]float64, len(reports))
	for i := range reports {
		dists[i] = dist(pos[i], j.pos)
		if dists[i] > projectileSpread {
			j.correct = true
		}
	}
	if !judgeOutliers {
		return j
	}
	bd := make([]float64, len(basis))
	for k, i := range basis {
		bd[k] = dists[i]
	}
	limit := max(outlierMin, 3*median(bd))
	for _, i := range basis {
		if dists[i] > limit {
			j.outliers = append(j.outliers, i)
		}
	}
	return j
}

func median(xs []float64) float64 {
	s := slices.Clone(xs)
	slices.Sort(s)
	n := len(s)
	if n == 0 {
		return 0
	}
	if n%2 == 1 {
		return s[n/2]
	}
	return (s[n/2-1] + s[n/2]) / 2
}

func dist(a, b proto.Vec3) float64 {
	var s float64
	for c := range 3 {
		d := float64(a[c]) - float64(b[c])
		s += d * d
	}
	return math.Sqrt(s)
}

func formatID(id uint64) string { return strconv.FormatUint(id, 10) }

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && s[n]&0xC0 == 0x80 {
		n--
	}
	return s[:n]
}
