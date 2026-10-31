package watch

import (
	"context"
	"net/http"
	"sync"
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
			card.Familiarity = s.familiarity.get(s.ctx, now)
			if card.Familiarity != nil && card.Familiarity.UnreadablePractice && card.Problem == "" {
				card.Problem = "practice records unreadable; familiarity may be incomplete"
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

type familiarityCache struct {
	mu      sync.Mutex
	value   *game.Familiarity
	checked time.Time
	loading bool
	loader  func(context.Context, time.Time) (game.Familiarity, error)
}

func (c *familiarityCache) get(ctx context.Context, now time.Time) *game.Familiarity {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ctx != nil && ctx.Err() == nil && !c.loading && (c.checked.IsZero() || now.Sub(c.checked) >= time.Minute) {
		c.loading = true
		go func() {
			loader := c.loader
			if loader == nil {
				loader = game.ReadFamiliarity
			}
			value, err := loader(ctx, time.Now().UTC())
			c.mu.Lock()
			defer c.mu.Unlock()
			if err == nil && ctx.Err() == nil {
				c.value = &value
			}
			c.checked = time.Now().UTC()
			c.loading = false
		}()
	}
	return c.value
}
