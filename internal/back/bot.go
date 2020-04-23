package back

import (
	"database/sql"
	"errors"
	"fmt"
	"kaepora/internal/util"
	"time"

	"github.com/jmoiron/sqlx"
)

// Put bot-specific oddities here

func (b *Back) GetGamesLeaguesAndTheirNextSessionStartDate() (
	[]Game,
	[]League,
	map[util.UUIDAsBlob]time.Time,
	error,
) {
	var (
		games   []Game
		leagues []League
		times   map[util.UUIDAsBlob]time.Time
	)

	if err := b.transaction(func(tx *sqlx.Tx) (err error) {
		games, err = getGames(tx)
		if err != nil {
			return err
		}

		leagues, err = getLeagues(tx)
		if err != nil {
			return err
		}
		times = make(map[util.UUIDAsBlob]time.Time, len(leagues))

		for _, league := range leagues {
			session, err := getNextMatchSessionForLeague(tx, league.ID)
			if err != nil {
				return err
			}
			times[league.ID] = session.StartDate.Time()
		}

		return nil
	}); err != nil {
		return nil, nil, nil, err
	}

	return games, leagues, times, nil
}

func getPlayerByDiscordID(tx *sqlx.Tx, discordID string) (Player, error) {
	var ret Player
	query := `SELECT * FROM Player WHERE Player.DiscordID = ? LIMIT 1`
	if err := tx.Get(&ret, query, discordID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Player{}, util.ErrPublic(
				"there is no player associated with this discord account, did you forget to `!register`?",
			)
		}
		return Player{}, err
	}

	return ret, nil
}

func (b *Back) SetLeagueAnnounceChannel(shortcode, channelID string) error {
	return b.transaction(func(tx *sqlx.Tx) error {
		league, err := getLeagueByShortCode(tx, shortcode)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return util.ErrPublic(fmt.Sprintf("invalid shortcode '%s'", shortcode))
			}

			return err
		}

		league.AnnounceDiscordChannelID = channelID
		return league.update(tx)
	})
}

// TODO remove the need for this
func (b *Back) GetPlayerByDiscordID(discordID string) (player Player, _ error) {
	if err := b.transaction(func(tx *sqlx.Tx) (err error) {
		player, err = getPlayerByDiscordID(tx, discordID)
		return err
	}); err != nil {
		return Player{}, err
	}

	return player, nil
}

type LeaderboardEntry struct {
	PlayerName string
	Rating     float64
}

func (b *Back) GetLeaderboardsForDiscordUser(discordID, shortcode string) (
	[]LeaderboardEntry, // top20
	[]LeaderboardEntry, // top around player, might be nil
	error,
) {
	var (
		top    []LeaderboardEntry
		around []LeaderboardEntry
	)

	if err := b.transaction(func(tx *sqlx.Tx) error {
		player, err := getPlayerByDiscordID(tx, discordID)
		if err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			player.ID = util.UUIDAsBlob{} // zero value as canary
		}

		league, err := getLeagueByShortCode(tx, shortcode)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return util.ErrPublic(fmt.Sprintf("no league found with shortcode '%s'", shortcode))
			}
			return err
		}

		top, err = getTop20(tx, league.ID)
		if err != nil {
			return err
		}

		if !player.ID.IsZero() {
			around, err = getTopAroundPlayer(tx, player, league.ID)
			if err != nil {
				return err
			}
		}

		return nil
	}); err != nil {
		return nil, nil, err
	}

	return top, around, nil
}

func getTop20(tx *sqlx.Tx, leagueID util.UUIDAsBlob) ([]LeaderboardEntry, error) {
	query := `
    SELECT Player.Name AS PlayerName, PlayerRating.Rating AS Rating
    FROM PlayerRating
    INNER JOIN Player ON(PlayerRating.PlayerID = Player.ID)
    WHERE PlayerRating.LeagueID = ?
    ORDER BY PlayerRating.Rating DESC
    LIMIT 20`

	var ret []LeaderboardEntry
	if err := tx.Select(&ret, query, leagueID); err != nil {
		return nil, err
	}

	return ret, nil
}

func getTopAroundPlayer(tx *sqlx.Tx, player Player, leagueID util.UUIDAsBlob) ([]LeaderboardEntry, error) {
	rating, err := getPlayerRating(tx, player.ID, leagueID)
	if err != nil {
		return nil, err
	}

	topAround := func(above bool, rating float64) ([]LeaderboardEntry, error) {
		op := ">"
		dir := "ASC"
		if !above {
			op = "<="
			dir = "DESC"
		}

		// nolint:gosec
		query := fmt.Sprintf(`
            SELECT Player.Name AS PlayerName, PlayerRating.Rating AS Rating
            FROM PlayerRating
            INNER JOIN Player ON (PlayerRating.PlayerID = Player.ID)
            WHERE
                PlayerRating.LeagueID = ?
                AND PlayerRating.Rating %[1]s ?  AND Player.ID != ?
            ORDER BY PlayerRating.Rating %[2]s
            LIMIT 5`,
			op, dir,
		)

		var ret []LeaderboardEntry
		if err := tx.Select(&ret, query, leagueID, rating, player.ID); err != nil {
			return nil, err
		}

		if !above { // reverse order to get ASC back
			for left, right := 0, len(ret)-1; left < right; left, right = left+1, right-1 {
				ret[left], ret[right] = ret[right], ret[left]
			}
		}

		return ret, nil
	}

	above, err := topAround(true, rating.Rating)
	if err != nil {
		return nil, err
	}

	below, err := topAround(false, rating.Rating)
	if err != nil {
		return nil, err
	}

	if len(above) == 0 && len(below) == 0 {
		return nil, nil
	}

	ret := make([]LeaderboardEntry, 0, len(above)+1+len(below))
	ret = append(ret, above...)
	ret = append(ret, LeaderboardEntry{PlayerName: player.Name, Rating: rating.Rating})
	ret = append(ret, below...)

	return ret, nil
}

func (b *Back) SendSeedSpoilerLog(player Player, seed string) error {
	var match Match
	if err := b.transaction(func(tx *sqlx.Tx) (err error) {
		match, err = getMatchBySeed(tx, seed)
		if err != nil {
			return err
		}

		if !match.hasEnded() {
			return util.ErrPublic(fmt.Sprintf("The race for seed %s is still in progress.", seed))
		}

		return nil
	}); err != nil {
		return err
	}

	b.sendSpoilerLogNotification(player, seed, match.SpoilerLog)

	return nil
}