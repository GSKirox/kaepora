package back

import (
	"encoding/json"
	"kaepora/internal/util"
	"time"

	"github.com/Masterminds/squirrel"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type MatchSessionStatus int

const (
	MatchSessionStatusWaiting    MatchSessionStatus = 0 // waiting for StartDate - 30m
	MatchSessionStatusJoinable   MatchSessionStatus = 1 // waiting for runners to join
	MatchSessionStatusPreparing  MatchSessionStatus = 2 // runners setting up race (StartDate - 10m)
	MatchSessionStatusInProgress MatchSessionStatus = 3 // runners still racing
	MatchSessionStatusInClosed   MatchSessionStatus = 4 // everyone finished
)

type MatchSession struct {
	ID        util.UUIDAsBlob
	LeagueID  util.UUIDAsBlob
	CreatedAt util.TimeAsTimestamp
	StartDate util.TimeAsDateTimeTZ
	Status    MatchSessionStatus
	PlayerIDs []byte // JSON array of human-readable UUID strings
}

func (s *MatchSession) GetPlayerIDs() []uuid.UUID {
	if len(s.PlayerIDs) == 0 {
		return nil
	}

	var strs []string
	if err := json.Unmarshal(s.PlayerIDs, &strs); err != nil {
		panic(err)
	}

	var e []error
	uuids := make([]uuid.UUID, 0, len(strs))

	for _, v := range strs {
		uuid, err := uuid.Parse(v)
		if err != nil {
			e = append(e, err)
			continue
		}

		uuids = append(uuids, uuid)
	}

	if err := util.ConcatErrors(e); err != nil {
		panic(err)
	}

	return uuids
}

func NewMatchSession(leagueID util.UUIDAsBlob, startDate time.Time) MatchSession {
	return MatchSession{
		ID:        util.NewUUIDAsBlob(),
		LeagueID:  leagueID,
		CreatedAt: util.TimeAsTimestamp(time.Now()),
		StartDate: util.TimeAsDateTimeTZ(startDate),
		Status:    MatchSessionStatusWaiting,
		PlayerIDs: []byte("[]"),
	}
}

func (b *Back) GetMatchSessionByStartDate(leagueID util.UUIDAsBlob, startDate time.Time) (MatchSession, error) {
	var ret MatchSession
	query := `SELECT * FROM MatchSession WHERE MatchSession.LeagueID = ? && MatchSession.StartDate = ? LIMIT 1`
	if err := b.db.Get(&ret, query, leagueID, util.TimeAsDateTimeTZ(startDate)); err != nil {
		return MatchSession{}, err
	}

	return ret, nil
}

func (b *Back) GetNextJoinableMatchSessionForLeague(leagueID util.UUIDAsBlob) (MatchSession, error) {
	var ret MatchSession
	query := `
        SELECT * FROM MatchSession
        WHERE MatchSession.LeagueID = ? &&
              MatchSession.StartDate > ? &&
              MatchSession.Status = ?
        ORDER BY MatchSession.StartDate ASC
        LIMIT 1`

	if err := b.db.Get(
		&ret, query,
		leagueID,
		util.TimeAsDateTimeTZ(time.Now()),
		MatchSessionStatusJoinable,
	); err != nil {
		return MatchSession{}, err
	}

	return ret, nil
}

func (s *MatchSession) Insert(tx *sqlx.Tx) error {
	query, args, err := squirrel.Insert("MatchSession").SetMap(squirrel.Eq{
		"ID":        s.ID,
		"CreatedAt": s.CreatedAt,
		"LeagueID":  s.LeagueID,
		"StartDate": s.StartDate,
		"Status":    s.Status,
		"PlayerIDs": s.PlayerIDs,
	}).ToSql()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}

	return nil
}

func (s *MatchSession) AddPlayerID(collectionToAdd ...uuid.UUID) {
	uuids := s.GetPlayerIDs()

	for _, toAdd := range collectionToAdd {
		for k := range uuids {
			if uuids[k] == toAdd {
				return
			}
		}

		uuids = append(uuids, toAdd)
	}

	s.PlayerIDs = encodePlayerIDs(uuids)
}

func encodePlayerIDs(ids []uuid.UUID) []byte {
	ret, err := json.Marshal(ids)
	if err != nil {
		panic(err)
	}

	return ret
}

func (b *Back) UpdateMatchSession(s MatchSession) error {
	return b.transaction(s.Update)
}

func (s *MatchSession) Update(tx *sqlx.Tx) error {
	query, args, err := squirrel.Update("MatchSession").SetMap(squirrel.Eq{
		"StartDate": s.StartDate,
		"Status":    s.Status,
		"PlayerIDs": s.PlayerIDs,
	}).
		Where("MatchSession.ID = ?", s.ID).
		ToSql()
	if err != nil {
		return err
	}

	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}

	return nil
}
