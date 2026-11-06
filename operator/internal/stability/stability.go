// Package stability provides enterprise anti-flapping, hysteresis, signal smoothing,
// oscillation detection, adaptive backoff, and circuit-breaking for autonomous
// resource and replica decisions.
package stability

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// Direction represents the direction of a resource or replica adjustment.
type Direction string

const (
	DirectionHold   Direction = "hold"
	DirectionGrow   Direction = "grow"
	DirectionShrink Direction = "shrink"
)

// CircuitState represents the state of the stability circuit breaker.
type CircuitState string

const (
	CircuitClosed   CircuitState = "CLOSED"
	CircuitOpen     CircuitState = "OPEN"
	CircuitHalfOpen CircuitState = "HALF_OPEN"
)

// Config defines the configurable stability and anti-flapping parameters.
type Config struct {
	// UpThreshold is the utilization ratio above which expansion is considered (e.g., 0.85).
	UpThreshold float64
	// DownThreshold is the utilization ratio below which reduction is considered (e.g., 0.45).
	// Reductions are strictly more conservative than expansions.
	DownThreshold float64
	// HysteresisBand is the deadband around thresholds to prevent boundary jitter (e.g., 0.05).
	HysteresisBand float64
	// GrowCooldown is the minimum interval between consecutive growth actions.
	GrowCooldown time.Duration
	// ShrinkCooldown is the minimum interval between consecutive shrink actions (more conservative).
	ShrinkCooldown time.Duration
	// MinDwellTime is the minimum time a workload must remain in a configuration after any change.
	MinDwellTime time.Duration
	// SmoothingAlpha is the EMA smoothing factor in (0, 1] (lower = more smoothing).
	SmoothingAlpha float64
	// MinVariationPercent is the minimum relative change required to justify an actuation (e.g., 0.05 = 5%).
	MinVariationPercent float64
	// ObservationWindow is the time window over which changes and oscillations are tracked.
	ObservationWindow time.Duration
	// MaxChangesPerWindow caps the number of actuations allowed within ObservationWindow.
	MaxChangesPerWindow int
	// OscillationThreshold is the number of direction reversals within ObservationWindow that trips the circuit breaker.
	OscillationThreshold int
	// BaseBackoff is the initial adaptive backoff duration when oscillation or failure occurs.
	BaseBackoff time.Duration
	// MaxBackoff caps the adaptive backoff duration.
	MaxBackoff time.Duration
}

// DefaultConfig returns production-safe anti-flapping parameters.
func DefaultConfig() Config {
	return Config{
		UpThreshold:          0.85,
		DownThreshold:        0.45,
		HysteresisBand:       0.05,
		GrowCooldown:         30 * time.Second,
		ShrinkCooldown:       5 * time.Minute,
		MinDwellTime:         60 * time.Second,
		SmoothingAlpha:       0.35,
		MinVariationPercent:  0.08,
		ObservationWindow:    15 * time.Minute,
		MaxChangesPerWindow:  4,
		OscillationThreshold: 3,
		BaseBackoff:          1 * time.Minute,
		MaxBackoff:           15 * time.Minute,
	}
}

// ChangeEvent records an executed change for window rate-limiting and oscillation detection.
type ChangeEvent struct {
	At        time.Time
	Direction Direction
	FromValue float64
	ToValue   float64
}

// Verdict is the auditable result of a stability evaluation.
type Verdict struct {
	Allowed          bool
	Direction        Direction
	SmoothedSignal   float64
	RawSignal        float64
	CircuitState     CircuitState
	OscillationCount int
	ChangesInWindow  int
	BackoffRemaining time.Duration
	ReasonCode       string
	Explanation      string
}

type workloadState struct {
	initialized      bool
	emaSignal        float64
	lastAction       Direction
	lastChangeTime   time.Time
	history          []ChangeEvent
	circuit          CircuitState
	circuitOpenedAt  time.Time
	backoffLevel     int
	consecutiveFails int
}

// Controller enforces anti-flapping and stability across workloads.
type Controller struct {
	mu     sync.Mutex
	cfg    Config
	states map[string]*workloadState
}

// NewController creates a new stability controller with validated defaults.
func NewController(cfg Config) *Controller {
	def := DefaultConfig()
	if cfg.UpThreshold <= 0 {
		cfg.UpThreshold = def.UpThreshold
	}
	if cfg.DownThreshold <= 0 || cfg.DownThreshold >= cfg.UpThreshold {
		cfg.DownThreshold = def.DownThreshold
	}
	if cfg.HysteresisBand < 0 {
		cfg.HysteresisBand = def.HysteresisBand
	}
	if cfg.GrowCooldown <= 0 {
		cfg.GrowCooldown = def.GrowCooldown
	}
	if cfg.ShrinkCooldown <= 0 {
		cfg.ShrinkCooldown = def.ShrinkCooldown
	}
	if cfg.MinDwellTime <= 0 {
		cfg.MinDwellTime = def.MinDwellTime
	}
	if cfg.SmoothingAlpha <= 0 || cfg.SmoothingAlpha > 1 {
		cfg.SmoothingAlpha = def.SmoothingAlpha
	}
	if cfg.MinVariationPercent <= 0 {
		cfg.MinVariationPercent = def.MinVariationPercent
	}
	if cfg.ObservationWindow <= 0 {
		cfg.ObservationWindow = def.ObservationWindow
	}
	if cfg.MaxChangesPerWindow <= 0 {
		cfg.MaxChangesPerWindow = def.MaxChangesPerWindow
	}
	if cfg.OscillationThreshold <= 0 {
		cfg.OscillationThreshold = def.OscillationThreshold
	}
	if cfg.BaseBackoff <= 0 {
		cfg.BaseBackoff = def.BaseBackoff
	}
	if cfg.MaxBackoff <= 0 {
		cfg.MaxBackoff = def.MaxBackoff
	}
	return &Controller{
		cfg:    cfg,
		states: make(map[string]*workloadState),
	}
}

// Evaluate checks whether a proposed change from currentVal to proposedVal at time now
// is permitted by all stability rules, hysteresis, dwell time, rate limits, and circuit breakers.
func (c *Controller) Evaluate(key string, now time.Time, rawSignal, currentVal, proposedVal float64, emergency bool) Verdict {
	c.mu.Lock()
	defer c.mu.Unlock()

	st := c.getOrInit(key, rawSignal)
	// Update Exponential Moving Average (EMA) signal smoothing
	if !st.initialized {
		st.emaSignal = rawSignal
		st.initialized = true
	} else {
		st.emaSignal = c.cfg.SmoothingAlpha*rawSignal + (1.0-c.cfg.SmoothingAlpha)*st.emaSignal
	}
	c.pruneWindow(st, now)

	dir := DirectionHold
	if proposedVal > currentVal {
		dir = DirectionGrow
	} else if proposedVal < currentVal {
		dir = DirectionShrink
	}

	reversals := countReversals(st.history, dir)

	if dir == DirectionHold {
		return Verdict{
			Allowed:          false,
			Direction:        DirectionHold,
			SmoothedSignal:   st.emaSignal,
			RawSignal:        rawSignal,
			CircuitState:     st.circuit,
			OscillationCount: reversals,
			ChangesInWindow:  len(st.history),
			ReasonCode:       "NoChangeProposed",
			Explanation:      "proposed resource configuration matches current value; holding steady",
		}
	}

	// 1. Check Circuit Breaker & Adaptive Backoff
	if st.circuit == CircuitOpen {
		backoffDur := c.adaptiveBackoff(st.backoffLevel)
		elapsed := now.Sub(st.circuitOpenedAt)
		if elapsed < backoffDur && !emergency {
			rem := backoffDur - elapsed
			return Verdict{
				Allowed:          false,
				Direction:        dir,
				SmoothedSignal:   st.emaSignal,
				RawSignal:        rawSignal,
				CircuitState:     CircuitOpen,
				OscillationCount: reversals,
				ChangesInWindow:  len(st.history),
				BackoffRemaining: rem,
				ReasonCode:       "CircuitBreakerOpen",
				Explanation:      fmt.Sprintf("stability circuit breaker is OPEN due to oscillation/failures; %s backoff remaining", rem.Round(time.Second)),
			}
		}
		// Transition to HALF_OPEN once backoff expires (or emergency overrides)
		st.circuit = CircuitHalfOpen
	}

	// 2. Oscillation Detection (trip circuit breaker if direction alternates repeatedly)
	if reversals >= c.cfg.OscillationThreshold && !emergency {
		st.circuit = CircuitOpen
		st.circuitOpenedAt = now
		st.backoffLevel++
		backoffDur := c.adaptiveBackoff(st.backoffLevel)
		return Verdict{
			Allowed:          false,
			Direction:        dir,
			SmoothedSignal:   st.emaSignal,
			RawSignal:        rawSignal,
			CircuitState:     CircuitOpen,
			OscillationCount: reversals,
			ChangesInWindow:  len(st.history),
			BackoffRemaining: backoffDur,
			ReasonCode:       "OscillationDetected",
			Explanation:      fmt.Sprintf("blocked %s: detected %d direction reversals in %s window; tripping circuit breaker for %s", dir, reversals, c.cfg.ObservationWindow, backoffDur),
		}
	}

	// 3. Minimum Variation Threshold
	if currentVal > 0 && !emergency {
		relDelta := math.Abs(proposedVal-currentVal) / currentVal
		if relDelta < c.cfg.MinVariationPercent {
			return Verdict{
				Allowed:          false,
				Direction:        dir,
				SmoothedSignal:   st.emaSignal,
				RawSignal:        rawSignal,
				CircuitState:     st.circuit,
				OscillationCount: reversals,
				ChangesInWindow:  len(st.history),
				ReasonCode:       "BelowMinimumVariation",
				Explanation:      fmt.Sprintf("blocked %s: relative change %.1f%% is below minimum variation threshold %.1f%%", dir, relDelta*100, c.cfg.MinVariationPercent*100),
			}
		}
	}

	// 4. Asymmetric Hysteresis Band Validation on Smoothed Signal
	if !emergency {
		if dir == DirectionGrow {
			effectiveUp := c.cfg.UpThreshold + c.cfg.HysteresisBand
			if st.lastAction == DirectionShrink {
				effectiveUp += c.cfg.HysteresisBand
			}
			if st.emaSignal < effectiveUp {
				return Verdict{
					Allowed:          false,
					Direction:        dir,
					SmoothedSignal:   st.emaSignal,
					RawSignal:        rawSignal,
					CircuitState:     st.circuit,
					OscillationCount: reversals,
					ChangesInWindow:  len(st.history),
					ReasonCode:       "HysteresisSuppressedGrow",
					Explanation:      fmt.Sprintf("postponed grow: EMA smoothed signal %.3f is below hysteresis threshold %.3f (raw=%.3f)", st.emaSignal, effectiveUp, rawSignal),
				}
			}
		} else if dir == DirectionShrink {
			effectiveDown := c.cfg.DownThreshold - c.cfg.HysteresisBand
			if st.lastAction == DirectionGrow {
				effectiveDown -= c.cfg.HysteresisBand
			}
			if st.emaSignal > effectiveDown {
				return Verdict{
					Allowed:          false,
					Direction:        dir,
					SmoothedSignal:   st.emaSignal,
					RawSignal:        rawSignal,
					CircuitState:     st.circuit,
					OscillationCount: reversals,
					ChangesInWindow:  len(st.history),
					ReasonCode:       "HysteresisSuppressedShrink",
					Explanation:      fmt.Sprintf("postponed shrink: EMA smoothed signal %.3f is above conservative down-threshold %.3f (raw=%.3f)", st.emaSignal, effectiveDown, rawSignal),
				}
			}
		}
	}

	// 5. Minimum Dwell Time & Asymmetric Cooldown
	if !st.lastChangeTime.IsZero() && !emergency {
		elapsed := now.Sub(st.lastChangeTime)
		if elapsed < c.cfg.MinDwellTime {
			rem := c.cfg.MinDwellTime - elapsed
			return Verdict{
				Allowed:          false,
				Direction:        dir,
				SmoothedSignal:   st.emaSignal,
				RawSignal:        rawSignal,
				CircuitState:     st.circuit,
				OscillationCount: reversals,
				ChangesInWindow:  len(st.history),
				BackoffRemaining: rem,
				ReasonCode:       "MinimumDwellTimeActive",
				Explanation:      fmt.Sprintf("postponed %s: minimum dwell time (%s) has %s remaining", dir, c.cfg.MinDwellTime, rem.Round(time.Second)),
			}
		}
		requiredCooldown := c.cfg.ShrinkCooldown
		if dir == DirectionGrow {
			requiredCooldown = c.cfg.GrowCooldown
		}
		if elapsed < requiredCooldown {
			rem := requiredCooldown - elapsed
			return Verdict{
				Allowed:          false,
				Direction:        dir,
				SmoothedSignal:   st.emaSignal,
				RawSignal:        rawSignal,
				CircuitState:     st.circuit,
				OscillationCount: reversals,
				ChangesInWindow:  len(st.history),
				BackoffRemaining: rem,
				ReasonCode:       "AsymmetricCooldownActive",
				Explanation:      fmt.Sprintf("postponed %s: %s cooldown (%s) has %s remaining", dir, dir, requiredCooldown, rem.Round(time.Second)),
			}
		}
	}

	// 6. Rate Limit: Maximum Changes per Observation Window
	if len(st.history) >= c.cfg.MaxChangesPerWindow && !emergency {
		return Verdict{
			Allowed:          false,
			Direction:        dir,
			SmoothedSignal:   st.emaSignal,
			RawSignal:        rawSignal,
			CircuitState:     st.circuit,
			OscillationCount: reversals,
			ChangesInWindow:  len(st.history),
			ReasonCode:       "MaxChangesPerWindowExceeded",
			Explanation:      fmt.Sprintf("blocked %s: already executed %d changes within %s window (max=%d)", dir, len(st.history), c.cfg.ObservationWindow, c.cfg.MaxChangesPerWindow),
		}
	}

	// Allowed! Record the transition.
	st.lastAction = dir
	st.lastChangeTime = now
	st.history = append(st.history, ChangeEvent{
		At:        now,
		Direction: dir,
		FromValue: currentVal,
		ToValue:   proposedVal,
	})
	if st.circuit == CircuitHalfOpen {
		st.circuit = CircuitClosed
		st.backoffLevel = max(0, st.backoffLevel-1)
	}

	return Verdict{
		Allowed:          true,
		Direction:        dir,
		SmoothedSignal:   st.emaSignal,
		RawSignal:        rawSignal,
		CircuitState:     st.circuit,
		OscillationCount: reversals,
		ChangesInWindow:  len(st.history),
		ReasonCode:       "Approved",
		Explanation:      fmt.Sprintf("approved %s (%.1f -> %.1f): smoothed signal %.3f passed hysteresis, dwell, and stability gates", dir, currentVal, proposedVal, st.emaSignal),
	}
}

// RecordFailure notifies the stability controller that an actuation failed, incrementing adaptive backoff.
func (c *Controller) RecordFailure(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.getOrInit(key, 0)
	st.consecutiveFails++
	if st.consecutiveFails >= 3 {
		st.circuit = CircuitOpen
		st.circuitOpenedAt = now
		st.backoffLevel++
	}
}

func (c *Controller) getOrInit(key string, initialSignal float64) *workloadState {
	st, ok := c.states[key]
	if !ok {
		st = &workloadState{
			emaSignal:  initialSignal,
			lastAction: DirectionHold,
			circuit:    CircuitClosed,
		}
		c.states[key] = st
	}
	return st
}

func (c *Controller) pruneWindow(st *workloadState, now time.Time) {
	cutoff := now.Add(-c.cfg.ObservationWindow)
	kept := st.history[:0]
	for _, ev := range st.history {
		if !ev.At.Before(cutoff) {
			kept = append(kept, ev)
		}
	}
	st.history = kept
}

func (c *Controller) adaptiveBackoff(level int) time.Duration {
	if level <= 1 {
		return c.cfg.BaseBackoff
	}
	mult := 1 << min(level-1, 6)
	d := time.Duration(mult) * c.cfg.BaseBackoff
	if d > c.cfg.MaxBackoff {
		return c.cfg.MaxBackoff
	}
	return d
}

func countReversals(history []ChangeEvent, nextDir Direction) int {
	if len(history) == 0 {
		return 0
	}
	reversals := 0
	prev := history[0].Direction
	for i := 1; i < len(history); i++ {
		cur := history[i].Direction
		if cur != DirectionHold && prev != DirectionHold && cur != prev {
			reversals++
		}
		if cur != DirectionHold {
			prev = cur
		}
	}
	if nextDir != DirectionHold && prev != DirectionHold && nextDir != prev {
		reversals++
	}
	return reversals
}
