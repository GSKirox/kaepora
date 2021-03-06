package back

import (
	"fmt"
	"kaepora/internal/back/schedule"
	"kaepora/internal/generator/factory"
	"kaepora/internal/generator/oot"
	"kaepora/internal/util"
	"log"
	"time"

	"github.com/jmoiron/sqlx"
	"gopkg.in/guregu/null.v4"
)

// SendDevSeed generates a seed from a league settings and sends it over
// Discord. This was originally for dev purposes but is now accessible to
// everyone.
func (b *Back) SendDevSeed(discordID, leagueShortCode, seed, version string) error {
	return b.transaction(func(tx *sqlx.Tx) error {
		league, err := getLeagueByShortCode(tx, leagueShortCode)
		if err != nil {
			return fmt.Errorf("could not find League: %w", err)
		}

		generatorID := league.Generator
		if version != "" {
			generatorID = factory.OverrideVersion(generatorID, version)
		}

		gen, err := b.GetGenerator(generatorID)
		if err != nil {
			return err
		}

		out, err := gen.Generate(league.Settings, seed)
		if err != nil {
			return err
		}

		if err := gen.UnlockSpoilerLog(out.State); err != nil {
			return err
		}

		zlibLog, err := util.NewZLIBBlob(out.SpoilerLog)
		if err != nil {
			return err
		}

		player := Player{DiscordID: null.NewString(discordID, true)}
		b.sendMatchSeedNotification(
			MatchSession{},
			gen.GetDownloadURL(out.State), out,
			player, Player{},
		)
		b.sendRawSpoilerLogNotification(player, seed, zlibLog)

		return nil
	})
}

// LoadFixtures fills the DB with dev data.
func (b *Back) LoadFixtures() error {
	game := NewGame("The Legend of Zelda: Ocarina of Time")
	leagues := []League{
		NewLeague("Standard", "std", game.ID, oot.RandomizerAPIName+":5.2.0", "s3.json"),
		NewLeague("Debug", "debug", game.ID, oot.RandomizerName+":5.2.13", "s3.json"),
		NewLeague("Random", "random", game.ID, oot.SettingsRandomizerName+":5.2.13", "s3.json"),
	}

	leagues[0].Schedule = schedule.Config{
		Type: schedule.TypeDayOfWeek,
	}

	return b.transaction(func(tx *sqlx.Tx) error {
		if err := game.insert(tx); err != nil {
			return err
		}

		for _, v := range leagues {
			if err := v.insert(tx); err != nil {
				return err
			}
		}

		return nil
	})
}

// StartDevRace creates a in-progress race with random players plus the one with the given Discord ID.
func (b *Back) StartDevRace(shortcode, userDiscordID string) error {
	return b.transaction(func(tx *sqlx.Tx) error {
		league, err := getLeagueByShortCode(tx, shortcode)
		if err != nil {
			return err
		}

		session := NewMatchSession(league.ID, time.Now().Add(-45*time.Minute))
		session.Status = MatchSessionStatusPreparing
		var players []Player
		if err := tx.Select(&players, `SELECT * FROM Player WHERE DiscordID IS NULL ORDER BY RANDOM() LIMIT 11`); err != nil {
			return err
		}
		player, err := getPlayerByDiscordID(tx, userDiscordID)
		if err != nil {
			return err
		}
		players = append(players, player)

		for k := range players {
			log.Printf("debug: adding %s to race", players[k].Name)
			session.AddPlayerID(players[k].ID.UUID())
		}

		if err := session.insert(tx); err != nil {
			return err
		}

		if err := b.matchMakeSession(tx, session); err != nil {
			return err
		}

		if err := session.start(tx); err != nil {
			return err
		}
		if err := session.update(tx); err != nil {
			return err
		}

		return nil
	})
}
