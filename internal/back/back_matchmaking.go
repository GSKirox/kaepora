package back

import (
	"crypto/rand"
	"database/sql"
	"fmt"
	"kaepora/internal/generator"
	"kaepora/internal/util"
	"log"
	"math/big"
	"sort"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// doMatchMaking creates all Match and MatchEntry on Matches that reached the
// preparing state, and dispatches seeds to the players.
// This is done in a different transaction than makeMatchSessionsPreparing to
// ensure no one can join when we matchmake/generate the seeds.
func (b *Back) doMatchMaking(sessions []MatchSession) error {
	return b.transaction(func(tx *sqlx.Tx) error {
		for k := range sessions {
			if err := b.matchMakeSession(tx, sessions[k]); err != nil {
				return err
			}

			if err := b.generateAndSendSeeds(tx, sessions[k]); err != nil {
				return err
			}
		}

		return nil
	})
}

func (b *Back) generateAndSendSeeds(tx *sqlx.Tx, session MatchSession) error {
	matches, err := getMatchesBySessionID(tx, session.ID)
	if err != nil {
		return err
	}
	log.Printf("debug: got %d matches to generate seeds for", len(matches))

	var e []error

	for k := range matches {
		p1, err := getPlayerByID(tx, matches[k].Entries[0].PlayerID)
		if err != nil {
			e = append(e, err)
			continue
		}
		p2, err := getPlayerByID(tx, matches[k].Entries[1].PlayerID)
		if err != nil {
			e = append(e, err)
			continue
		}

		go func(match Match) {
			log.Printf("debug: generating seed %s for match %s", match.Seed, match.ID)
			if err := b.generateAndSendMatchSeed(match, session, p1, p2); err != nil {
				log.Printf("unable to generate and send seed: %s", err)
			}
		}(matches[k])
	}

	return util.ConcatErrors(e)
}

func (b *Back) generateAndSendMatchSeed(
	match Match,
	session MatchSession,
	p1, p2 Player,
) error {
	gen, err := generator.NewGenerator(match.Generator)
	if err != nil {
		return err
	}

	patch, err := gen.Generate(match.Settings, match.Seed)
	if err != nil {
		return err
	}

	b.sendMatchSeedNotification(match, session, patch, p1, p2)

	return nil
}

func (b *Back) SendDevSeed(
	discordID string,
	leagueShortCode string,
	seed string,
) error {
	return b.transaction(func(tx *sqlx.Tx) error {
		league, err := getLeagueByShortCode(tx, leagueShortCode)
		if err != nil {
			return fmt.Errorf("could not find League: %w", err)
		}

		game, err := getGameByID(tx, league.GameID)
		if err != nil {
			return fmt.Errorf("could not find Game: %w", err)
		}

		player, err := getPlayerByDiscordID(tx, discordID)
		if err != nil {
			return err
		}

		gen, err := generator.NewGenerator(game.Generator)
		if err != nil {
			return err
		}

		patch, err := gen.Generate(league.Settings, seed)
		if err != nil {
			return err
		}

		b.sendMatchSeedNotification(Match{}, MatchSession{}, patch, player, Player{})

		return nil
	})
}

type pair struct {
	p1, p2 Player
}

// I'm going to do things the sqlite way and JOIN nothing here, don't be afraid.
func (b *Back) matchMakeSession(tx *sqlx.Tx, session MatchSession) error {
	if session.Status != MatchSessionStatusPreparing {
		log.Printf("warning: attempted to matchmake session %s at status %d", session.ID, session.Status)
		return nil
	}

	session, ok, err := b.ensureSessionIsValidForMatchMaking(tx, session)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}

	players, err := getSessionPlayersSortedByRating(tx, session)
	if err != nil {
		return err
	}

	pairs := pairPlayers(players)
	log.Printf("debug: got %d players in the pool (%d pairs)", len(players), len(pairs))

	for k := range pairs {
		// google/uuid.v4 are generated using a CSPRNG
		match, err := NewMatch(tx, session, uuid.New().String())
		if err != nil {
			return err
		}
		if err := match.insert(tx); err != nil {
			return err
		}

		e1 := NewMatchEntry(match.ID, pairs[k].p1.ID)
		if err := e1.insert(tx); err != nil {
			return err
		}
		e2 := NewMatchEntry(match.ID, pairs[k].p2.ID)
		if err := e2.insert(tx); err != nil {
			return err
		}
	}

	return nil
}

// pairPlayers randomly pair close players together.
func pairPlayers(players []Player) []pair {
	if len(players) < 2 {
		return nil
	}

	// TODO: Heuristics, if both shared their last match: go one neighbor down/up
	pairs := make([]pair, 0, len(players)/2)
	for len(players) > 2 {
		i1 := randomIndex(len(players))
		p := pair{p1: players[i1]}
		players = players[:i1+copy(players[i1:], players[i1+1:])]

		minIndex := i1 - 5
		if minIndex < 0 {
			minIndex = 0
		}
		maxIndex := i1 + 5
		if maxIndex > len(players)-1 {
			maxIndex = len(players) - 1
		}
		if minIndex == maxIndex {
			panic("unreachable")
		}

		var i2 int
		for i2 == 0 {
			i2 = randomInt(minIndex, maxIndex)
		}
		p.p2 = players[i2]
		players = players[:i2+copy(players[i2:], players[i2+1:])]

		pairs = append(pairs, p)
	}
	pairs = append(pairs, pair{players[0], players[1]})

	return pairs
}

func getSessionPlayersSortedByRating(tx *sqlx.Tx, session MatchSession) ([]Player, error) {
	ids := session.GetPlayerIDs()
	players := make([]Player, 0, len(ids))

	for _, playerID := range ids {
		player, err := getPlayerByID(tx, util.UUIDAsBlob(playerID))
		if err != nil {
			return nil, err
		}
		player.Rating, err = getPlayerRating(tx, player.ID, session.LeagueID)
		if err != nil {
			return nil, err
		}

		players = append(players, player)
	}

	sort.Sort(byRating(players))
	return players, nil
}

func (b *Back) ensureSessionIsValidForMatchMaking(tx *sqlx.Tx, session MatchSession) (MatchSession, bool, error) {
	players := session.GetPlayerIDs()

	// Ditch the one player we can't match with anyone.
	if len(players)%2 == 1 {
		toRemove := players[randomIndex(len(players))]
		log.Printf("info: removed odd player %s from session %s", toRemove, session.ID.UUID())
		session.RemovePlayerID(toRemove)
		player, err := getPlayerByID(tx, util.UUIDAsBlob(toRemove))
		if err != nil {
			return MatchSession{}, false, fmt.Errorf("unable to fetch odd player: %w", err)
		}
		b.sendOddKickNotification(player)
	}

	// No one wants to play =(
	if len(players) < 2 {
		session.Status = MatchSessionStatusClosed
		log.Printf("info: no players for session %s", session.ID.UUID())
		if err := b.sendMatchSessionEmptyNotification(tx, session); err != nil {
			return MatchSession{}, false, err
		}
		return session, false, session.update(tx)
	}

	if err := session.update(tx); err != nil {
		return MatchSession{}, false, err
	}

	return session, true, nil
}

func randomIndex(length int) int {
	return randomInt(0, length-1)
}

func randomInt(iMin, iMax int) int {
	if iMin > iMax {
		panic("iMin > iMax")
	}

	max := big.NewInt(int64(iMax - iMin))
	offset, err := rand.Int(rand.Reader, max)
	if err != nil {
		panic(err)
	}

	return int(offset.Int64() - int64(iMin))
}

func getActiveMatchAndEntriesForPlayer(tx *sqlx.Tx, player Player) (
	match Match, self MatchEntry, opponent MatchEntry, _ error,
) {
	session, err := getPlayerActiveSession(tx, player.ID.UUID())
	if err != nil {
		if err == sql.ErrNoRows {
			return match, self, opponent, util.ErrPublic("you are not in any active race right now")
		}
		return match, self, opponent, fmt.Errorf("unable to get active session: %w", err)
	}

	if err := session.CanForfeit(); err != nil {
		return match, self, opponent, err
	}

	match, err = getMatchByPlayerAndSession(tx, player, session)
	if err != nil {
		return match, self, opponent, fmt.Errorf("cannot find Match: %w", err)
	}

	self, opponent, err = match.getPlayerAndOpponentEntries(player.ID)
	if err != nil {
		return match, self, opponent, err
	}

	return match, self, opponent, nil
}
