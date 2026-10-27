package proto

import (
	"time"

	"github.com/mhk-os/server/internal/wire"
)

// Limits of the achievement messages (docs/protocol.md, "Achievements").
const (
	MaxStatID          = 64
	MaxAchievementID   = 96
	MaxSetItem         = 96
	MaxSyncStats       = 256  // counters and maxima of one SyncAchievements
	MaxSyncSets        = 64   // sets of one SyncAchievements
	MaxSyncSetItems    = 2000 // items of one set in a SyncAchievements
	MaxSyncUnlocked    = 2000 // unlocked of one SyncAchievements
	MaxAccountStats    = 500  // counters, maxima and set stats an account keeps together
	MaxAccountSetItems = 5000 // set items an account keeps over all its sets
	MaxAccountUnlocked = 2000 // achievements an account keeps unlocked
	MaxShareIDs        = 2000 // ids of an AchievementShares request and entries of its response
	statMinSize        = 1 + 4
	totalMinSize       = 1 + 8
	setMinSize         = 1 + 1
	unlockMinSize      = 1 + 8
	shareMinSize       = 1 + 4
)

// StatValue is a counter delta (request) or a maximum (request and response) of one stat.
type StatValue struct {
	Stat  string
	Value uint32
}

// StatTotal is a counter's account total.
type StatTotal struct {
	Stat  string
	Value uint64
}

// StatSet is the items of a set stat: the ones to add (request) or the whole set (response).
type StatSet struct {
	Stat  string
	Items []string
}

// Unlock is an unlocked achievement and when it was unlocked.
type Unlock struct {
	ID string
	At time.Time
}

func encodeStatValues(w *wire.Writer, list []StatValue) {
	w.Count(len(list))
	for _, s := range list {
		w.String(s.Stat)
		w.U32(s.Value)
	}
}

func decodeStatValues(r *wire.Reader, max int) []StatValue {
	list := make([]StatValue, r.Count(max, statMinSize))
	for i := range list {
		list[i].Stat = r.String(MaxStatID)
		list[i].Value = r.U32()
	}
	return list
}

func encodeStatSets(w *wire.Writer, list []StatSet) {
	w.Count(len(list))
	for _, s := range list {
		w.String(s.Stat)
		w.Count(len(s.Items))
		for _, item := range s.Items {
			w.String(item)
		}
	}
}

func decodeStatSets(r *wire.Reader, maxSets, maxItems int) []StatSet {
	list := make([]StatSet, r.Count(maxSets, setMinSize))
	for i := range list {
		list[i].Stat = r.String(MaxStatID)
		list[i].Items = make([]string, r.Count(maxItems, 1))
		for j := range list[i].Items {
			list[i].Items[j] = r.String(MaxSetItem)
		}
	}
	return list
}

func encodeUnlocks(w *wire.Writer, list []Unlock) {
	w.Count(len(list))
	for _, u := range list {
		w.String(u.ID)
		w.Time(u.At)
	}
}

func decodeUnlocks(r *wire.Reader, max int) []Unlock {
	list := make([]Unlock, r.Count(max, unlockMinSize))
	for i := range list {
		list[i].ID = r.String(MaxAchievementID)
		list[i].At = r.Time()
	}
	return list
}

// SyncAchievements is request 27: progress to merge into the account. Counters carry deltas.
type SyncAchievements struct {
	Counters []StatValue
	Maxima   []StatValue
	Sets     []StatSet
	Unlocked []Unlock
}

func (m *SyncAchievements) Encode(w *wire.Writer) {
	encodeStatValues(w, m.Counters)
	encodeStatValues(w, m.Maxima)
	encodeStatSets(w, m.Sets)
	encodeUnlocks(w, m.Unlocked)
}

func (m *SyncAchievements) Decode(r *wire.Reader) error {
	m.Counters = decodeStatValues(r, MaxSyncStats)
	m.Maxima = decodeStatValues(r, MaxSyncStats)
	m.Sets = decodeStatSets(r, MaxSyncSets, MaxSyncSetItems)
	m.Unlocked = decodeUnlocks(r, MaxSyncUnlocked)
	return r.Err()
}

// AchievementState is the response to SyncAchievements: the account's merged progress.
type AchievementState struct {
	Counters []StatTotal
	Maxima   []StatValue
	Sets     []StatSet
	Unlocked []Unlock
}

func (m *AchievementState) Encode(w *wire.Writer) {
	w.Count(len(m.Counters))
	for _, c := range m.Counters {
		w.String(c.Stat)
		w.U64(c.Value)
	}
	encodeStatValues(w, m.Maxima)
	encodeStatSets(w, m.Sets)
	encodeUnlocks(w, m.Unlocked)
}

func (m *AchievementState) Decode(r *wire.Reader) error {
	m.Counters = make([]StatTotal, r.Count(MaxAccountStats, totalMinSize))
	for i := range m.Counters {
		m.Counters[i].Stat = r.String(MaxStatID)
		m.Counters[i].Value = r.U64()
	}
	m.Maxima = decodeStatValues(r, MaxAccountStats)
	m.Sets = decodeStatSets(r, MaxAccountStats, MaxAccountSetItems)
	m.Unlocked = decodeUnlocks(r, MaxAccountUnlocked)
	return r.Err()
}

// AchievementSharesRequest is request 28; no ids means every achievement anybody unlocked.
type AchievementSharesRequest struct {
	IDs []string
}

func (m *AchievementSharesRequest) Encode(w *wire.Writer) {
	w.Count(len(m.IDs))
	for _, id := range m.IDs {
		w.String(id)
	}
}

func (m *AchievementSharesRequest) Decode(r *wire.Reader) error {
	m.IDs = make([]string, r.Count(MaxShareIDs, 1))
	for i := range m.IDs {
		m.IDs[i] = r.String(MaxAchievementID)
	}
	return r.Err()
}

// AchievementShare is how many players unlocked one achievement.
type AchievementShare struct {
	ID       string
	Unlocked uint32
}

// AchievementShares is the response to AchievementShares.
type AchievementShares struct {
	Players uint32
	Shares  []AchievementShare
}

func (m *AchievementShares) Encode(w *wire.Writer) {
	w.U32(m.Players)
	w.Count(len(m.Shares))
	for _, s := range m.Shares {
		w.String(s.ID)
		w.U32(s.Unlocked)
	}
}

func (m *AchievementShares) Decode(r *wire.Reader) error {
	m.Players = r.U32()
	m.Shares = make([]AchievementShare, r.Count(MaxShareIDs, shareMinSize))
	for i := range m.Shares {
		m.Shares[i].ID = r.String(MaxAchievementID)
		m.Shares[i].Unlocked = r.U32()
	}
	return r.Err()
}
