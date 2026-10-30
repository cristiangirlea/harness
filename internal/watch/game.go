package watch

import (
	"net/http"
	"time"

	"github.com/cristiangirlea/harness/internal/game"
)

type gameCard struct {
	Level, Streak      int
	Days               []bool
	Badges             []string
	Local              bool
	Delivery, Practice int
	Exercise           *game.Exercise
	Problem            string
	Familiarity        *game.Familiarity
}

func (s *server) gameCard(w http.ResponseWriter, r *http.Request) *gameCard {
	if s.opt.Public != nil {
		return nil
	}
	settings, err := game.LoadSettings()
	if err != nil || !settings.On() {
		return nil
	}
	events, _, err := game.Load()
	if err != nil {
		return nil
	}
	now := time.Now().UTC()
	practice := game.NewPractice(nil, now)
	streak, days, err := practice.Streak()
	totals := game.Sum(events)
	card := &gameCard{Level: game.Level(totals.Person), Streak: streak, Days: days, Local: s.mayChangeBackends(w, r)}
	// One unreadable practice record should not hide the level and badges.
	if err != nil {
		card.Problem = "practice records unreadable; the streak is not shown"
	}
	for _, badge := range totals.Badges {
		card.Badges = append(card.Badges, game.BadgeName(badge.Key))
	}
	if card.Local {
		if len(s.opt.LAN) == 0 {
			if familiarity, err := game.ReadFamiliarity(now); err == nil {
				card.Familiarity = &familiarity
			}
		}
		weekly := totals.Since(game.WeekStart(now))
		card.Delivery, card.Practice = weekly[game.TrackDelivery], weekly[game.TrackPractice]
		exercise, err := practice.ExistingToday()
		if err == nil {
			card.Exercise = exercise
		}
	}
	return card
}
