package service

import (
	"sort"

	"tunecord/internal/mpris"
)

// Candidate is a player considered for display.
type Candidate struct {
	ID       mpris.PlayerID
	Identity string
	Status   mpris.Status
}

// AllowlistIndex returns the priority index of the first allowlist entry
// matching the player, or -1 if the player is not allowlisted.
func AllowlistIndex(id mpris.PlayerID, identity string, allowlist []string) int {
	for i, e := range allowlist {
		if mpris.MatchesEntry(id, identity, e) {
			return i
		}
	}
	return -1
}

// Select picks the player that drives the presence. The allowlist is an
// ordered priority list: the highest-listed Playing player wins; if none is
// playing, the highest-listed Paused player is used when showWhenPaused is
// set. Players not on the allowlist are ignored. Ties (several instances of
// the same player) are broken by bus name for determinism. Returns -1 if no
// player should be shown.
func Select(cands []Candidate, allowlist []string, showWhenPaused bool) int {
	type ranked struct{ idx, prio int }
	var playing, paused []ranked
	for i, c := range cands {
		p := AllowlistIndex(c.ID, c.Identity, allowlist)
		if p < 0 {
			continue
		}
		switch c.Status {
		case mpris.Playing:
			playing = append(playing, ranked{i, p})
		case mpris.Paused:
			paused = append(paused, ranked{i, p})
		}
	}
	best := func(rs []ranked) int {
		sort.SliceStable(rs, func(a, b int) bool {
			if rs[a].prio != rs[b].prio {
				return rs[a].prio < rs[b].prio
			}
			return cands[rs[a].idx].ID.BusName < cands[rs[b].idx].ID.BusName
		})
		return rs[0].idx
	}
	if len(playing) > 0 {
		return best(playing)
	}
	if showWhenPaused && len(paused) > 0 {
		return best(paused)
	}
	return -1
}
