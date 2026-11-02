package main

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	sdk "github.com/apteva/app-sdk"
)

// EnvironmentClock owns logical time for one runtime. Operational deadlines
// deliberately continue to use the wall clock.
type EnvironmentClock struct {
	mu       sync.RWMutex
	mode     string
	initial  time.Time
	current  time.Time
	advances []sdk.RuntimeClockAdvance
}

const maxEnvironmentClockAdvances = 10000

func fixtureActive(from, until *time.Time, at time.Time) bool {
	return (from == nil || !at.Before(*from)) && (until == nil || at.Before(*until))
}

func validateFixtureWindow(from, until *time.Time) error {
	if from != nil && from.IsZero() || until != nil && until.IsZero() {
		return errors.New("fixture time must be RFC3339")
	}
	if from != nil && until != nil && !until.After(*from) {
		return errors.New("expires_at must be after available_at")
	}
	return nil
}

func validateTimedFixtures(httpMocks []HTTPMock, integrations []IntegrationFixture) error {
	for i, m := range httpMocks {
		if err := validateFixtureWindow(m.AvailableAt, m.ExpiresAt); err != nil {
			return fmt.Errorf("http mock %d: %w", i, err)
		}
		if m.AvailableAt != nil || m.ExpiresAt != nil {
			for j := 0; j < i; j++ {
				other := httpMocks[j]
				if (other.AvailableAt != nil || other.ExpiresAt != nil) && (m.Host == "" || other.Host == "" || strings.EqualFold(m.Host, other.Host)) && (strings.HasPrefix(m.Path, other.Path) || strings.HasPrefix(other.Path, m.Path)) && (m.Method == "" || other.Method == "" || strings.EqualFold(m.Method, other.Method)) && fixtureWindowsOverlap(m.AvailableAt, m.ExpiresAt, other.AvailableAt, other.ExpiresAt) {
					return fmt.Errorf("http mocks %d and %d have overlapping time windows", j, i)
				}
			}
		}
	}
	for i, f := range integrations {
		if err := validateFixtureWindow(f.AvailableAt, f.ExpiresAt); err != nil {
			return fmt.Errorf("integration fixture %d: %w", i, err)
		}
		if f.AvailableAt != nil || f.ExpiresAt != nil {
			for j := 0; j < i; j++ {
				other := integrations[j]
				if (other.AvailableAt != nil || other.ExpiresAt != nil) && f.App == other.App && f.Tool == other.Tool && fixtureWindowsOverlap(f.AvailableAt, f.ExpiresAt, other.AvailableAt, other.ExpiresAt) {
					return fmt.Errorf("integration fixtures %d and %d have overlapping time windows", j, i)
				}
			}
		}
	}
	return nil
}

func fixtureWindowsOverlap(aFrom, aUntil, bFrom, bUntil *time.Time) bool {
	return (aUntil == nil || bFrom == nil || aUntil.After(*bFrom)) && (bUntil == nil || aFrom == nil || bUntil.After(*aFrom))
}

func newEnvironmentClock(spec *sdk.RuntimeClockSpec) (*EnvironmentClock, error) {
	now := time.Now().UTC()
	c := &EnvironmentClock{mode: "real", initial: now, current: now}
	if spec == nil || spec.Mode == "" || spec.Mode == "real" {
		if spec != nil && spec.InitialTime != nil {
			return nil, errors.New("initial_time requires manual clock mode")
		}
		return c, nil
	}
	if spec.Mode != "manual" {
		return nil, errors.New("clock mode must be real or manual")
	}
	c.mode = "manual"
	if spec.InitialTime != nil {
		if spec.InitialTime.IsZero() {
			return nil, errors.New("initial_time must be a nonzero RFC3339 timestamp")
		}
		c.initial = spec.InitialTime.UTC()
	} else {
		c.initial = now
	}
	c.current = c.initial
	return c, nil
}

func (c *EnvironmentClock) Now() time.Time {
	if c == nil {
		return time.Now().UTC()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.mode == "real" {
		return time.Now().UTC()
	}
	return c.current
}

func (c *EnvironmentClock) State() sdk.RuntimeClockState {
	if c == nil {
		return sdk.RuntimeClockState{Mode: "real", CurrentTime: time.Now().UTC()}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	current := c.current
	if c.mode == "real" {
		current = time.Now().UTC()
	}
	return sdk.RuntimeClockState{Mode: c.mode, InitialTime: c.initial, CurrentTime: current, Advancements: append([]sdk.RuntimeClockAdvance(nil), c.advances...)}
}

func (c *EnvironmentClock) Advance(to time.Time) (sdk.RuntimeClockState, error) {
	if c == nil {
		return sdk.RuntimeClockState{}, errors.New("runtime clock unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.mode != "manual" {
		return sdk.RuntimeClockState{}, errors.New("clock is not in manual mode")
	}
	if to.IsZero() || !to.After(c.current) {
		return sdk.RuntimeClockState{}, errors.New("clock advancement must be after current time")
	}
	if len(c.advances) >= maxEnvironmentClockAdvances {
		return sdk.RuntimeClockState{}, errors.New("clock advancement limit reached")
	}
	to = to.UTC()
	c.advances = append(c.advances, sdk.RuntimeClockAdvance{From: c.current, To: to, WallAt: time.Now().UTC()})
	c.current = to
	return sdk.RuntimeClockState{Mode: c.mode, InitialTime: c.initial, CurrentTime: c.current, Advancements: append([]sdk.RuntimeClockAdvance(nil), c.advances...)}, nil
}

func clockFromState(state sdk.RuntimeClockState) (*EnvironmentClock, error) {
	if state.Mode == "" || state.Mode == "real" {
		return newEnvironmentClock(nil)
	}
	if state.Mode != "manual" || state.InitialTime.IsZero() || state.CurrentTime.Before(state.InitialTime) {
		return nil, errors.New("invalid snapshot clock")
	}
	return &EnvironmentClock{mode: "manual", initial: state.InitialTime, current: state.CurrentTime, advances: append([]sdk.RuntimeClockAdvance(nil), state.Advancements...)}, nil
}
