package back

import (
	"kaepora/internal/util"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/jmoiron/sqlx"
)

// Game is the root of all leagues and randomizers, it is mostly unused but
// paves the way for multi-game support.
type Game struct {
	ID        util.UUIDAsBlob
	CreatedAt util.TimeAsTimestamp
	Name      string
}

func NewGame(name string) Game {
	return Game{
		ID:        util.NewUUIDAsBlob(),
		CreatedAt: util.TimeAsTimestamp(time.Now()),
		Name:      name,
	}
}

func (g *Game) insert(tx *sqlx.Tx) error {
	query, args, err := squirrel.Insert("Game").SetMap(squirrel.Eq{
		"ID":        g.ID,
		"CreatedAt": g.CreatedAt,
		"Name":      g.Name,
	}).ToSql()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}

	return nil
}

func getGames(tx *sqlx.Tx) ([]Game, error) {
	var ret []Game
	if err := tx.Select(&ret, "SELECT * FROM Game ORDER BY Name ASC"); err != nil {
		return nil, err
	}

	return ret, nil
}
