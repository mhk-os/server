package lobby

import (
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/mhk-os/server/internal/proto"
	"github.com/mhk-os/server/internal/race"
)

// TestRenameInRoomOverTCP: a member who changes their username shows the new name to the room, in the member list
// and in the last results.
func TestRenameInRoomOverTCP(t *testing.T) {
	s := startServer(t, false)
	ctx := itCtx(t)
	packs := s.packs("rename")
	owner, guest := s.connect(t, false), s.connect(t, false)
	owner.announce(t, s.contentVersion, packs)
	guest.announce(t, s.contentVersion, packs)
	st, err := owner.CreateRoom(ctx, &proto.RoomSettings{Name: "Umbenennen", Mode: proto.RoomRaceSet,
		Packs: []string{"mhk2", "mhk3"}, EngineClass: 1, Laps: 2, Items: true, MaxPlayers: 4,
		TrackPick: proto.PickOwner, RacesPerSet: 3})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := guest.JoinLobby(ctx, 0, st.Code); err != nil {
		t.Fatal(err)
	}
	if err := guest.SetLoadout(ctx, "game.huhn", "game.kart"); err != nil {
		t.Fatal(err)
	}
	if err := owner.StartRace(ctx, "mhk2.rome"); err != nil {
		t.Fatal(err)
	}
	call := s.waitStart(t, 1)
	call.cb.Finished(call.id, []race.Placement{{AccountID: guest.id, Username: guest.name, Position: 1, FinishMs: 5}})
	owner.waitState(t, func(x *proto.LobbyState) bool { return x.Summary.Phase == proto.PhaseResults })

	var nb [4]byte
	rand.Read(nb[:])
	renamed := "rn" + hex.EncodeToString(nb[:])
	if _, err := guest.ChangeUsername(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	owner.waitState(t, func(x *proto.LobbyState) bool {
		var member, row bool
		for _, m := range x.Members {
			member = member || m.AccountID == guest.id && m.Username == renamed
		}
		for _, r := range x.LastResults {
			row = row || r.AccountID == guest.id && r.Username == renamed
		}
		return member && row
	})
}
