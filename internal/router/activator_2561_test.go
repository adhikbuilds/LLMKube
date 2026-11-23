/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package router

import (
	"context"
	"errors"
	"testing"
	"time"
)

// #2561: spec.swapDelay holds a cross-model request before it may start a
// swap, so a client that gives up sooner (a health check or model probe with a
// short timeout) never unloads an idle resident member.

func delayPool(member string, delay time.Duration) *BackendPool {
	return &BackendPool{
		Name:       "heavy-slot",
		Namespace:  "lab",
		Member:     member,
		Members:    []string{"judge", "coder"},
		SwapBudget: 30 * time.Second,
		SwapDelay:  delay,
	}
}

func (a *Activator) waitingFor(member string) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	pr, ok := a.pools["lab/heavy-slot"]
	if !ok {
		return 0
	}
	return pr.waiting[member]
}

func (a *Activator) residentMember() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	pr, ok := a.pools["lab/heavy-slot"]
	if !ok {
		return ""
	}
	return pr.resident
}

func TestActivatorSwapDelay(t *testing.T) {
	tests := []struct {
		name string
		// resident makes judge the Ready resident before the request; busy
		// also keeps a judge request in flight while it runs.
		resident bool
		busy     bool
		member   string
		mode     string
		delay    time.Duration
		hold     time.Duration
		wantErr  error
		// wantActivate is how many times the requested member was activated,
		// read after settle has passed, so a swap started late still counts.
		wantActivate int
		settle       time.Duration
		minElapsed   time.Duration
		maxElapsed   time.Duration
		wantResident string
	}{
		{
			name:         "probe that gives up before the delay never swaps",
			resident:     true,
			member:       "coder",
			mode:         PoolActivationWait,
			delay:        300 * time.Millisecond,
			hold:         50 * time.Millisecond,
			wantErr:      ErrHoldBudgetExceeded,
			wantActivate: 0,
			settle:       500 * time.Millisecond,
			wantResident: "judge",
		},
		{
			name:         "request still waiting when the delay ends swaps",
			resident:     true,
			member:       "coder",
			mode:         PoolActivationWait,
			delay:        200 * time.Millisecond,
			hold:         5 * time.Second,
			wantActivate: 1,
			minElapsed:   200 * time.Millisecond,
			wantResident: "coder",
		},
		{
			// The hold is shorter than the delay: a delayed cold pool would
			// fail the request instead of loading the member.
			name:         "cold pool swaps without the delay",
			member:       "coder",
			mode:         PoolActivationWait,
			delay:        5 * time.Second,
			hold:         time.Second,
			wantActivate: 1,
			wantResident: "coder",
		},
		{
			name:         "zero delay swaps at once",
			resident:     true,
			member:       "coder",
			mode:         PoolActivationWait,
			hold:         time.Second,
			wantActivate: 1,
			wantResident: "coder",
		},
		{
			name:         "same-model request is served without the delay",
			resident:     true,
			member:       "judge",
			mode:         PoolActivationWait,
			delay:        5 * time.Second,
			hold:         time.Second,
			wantActivate: 0,
			wantResident: "judge",
		},
		{
			name:         "IfIdle still skips a busy resident at once",
			resident:     true,
			busy:         true,
			member:       "coder",
			mode:         PoolActivationIfIdle,
			delay:        2 * time.Second,
			hold:         5 * time.Second,
			wantErr:      ErrIncumbentBusy,
			wantActivate: 0,
			maxElapsed:   time.Second,
			wantResident: "judge",
		},
		{
			name:         "IfIdle waits out the delay against an idle resident",
			resident:     true,
			member:       "coder",
			mode:         PoolActivationIfIdle,
			delay:        200 * time.Millisecond,
			hold:         5 * time.Second,
			wantActivate: 1,
			minElapsed:   200 * time.Millisecond,
			wantResident: "coder",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := newFakeMemberController()
			a := NewActivator(context.Background(), fake, "r", nil)
			if tt.resident {
				fake.setPhase("judge", modelReadyPhase)
			}
			if tt.busy {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				rel, err := a.Acquire(ctx, delayPool("judge", tt.delay))
				cancel()
				if err != nil {
					t.Fatalf("Acquire judge: %v", err)
				}
				defer rel()
			}

			ctx, cancel := context.WithTimeout(context.Background(), tt.hold)
			defer cancel()
			start := time.Now()
			rel, err := a.AcquireWithMode(ctx, delayPool(tt.member, tt.delay), tt.mode)
			elapsed := time.Since(start)
			if rel != nil {
				rel()
			}
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("Acquire %s: err = %v, want %v", tt.member, err, tt.wantErr)
			}
			if tt.minElapsed > 0 && elapsed < tt.minElapsed {
				t.Errorf("Acquire returned after %s, want at least the %s delay", elapsed, tt.minElapsed)
			}
			if tt.maxElapsed > 0 && elapsed > tt.maxElapsed {
				t.Errorf("Acquire returned after %s, want within %s", elapsed, tt.maxElapsed)
			}
			time.Sleep(tt.settle)
			if got := fake.activateCount(tt.member); got != tt.wantActivate {
				t.Errorf("%s activate count = %d, want %d", tt.member, got, tt.wantActivate)
			}
			if got := a.residentMember(); got != tt.wantResident {
				t.Errorf("resident = %q, want %q", got, tt.wantResident)
			}
		})
	}
}

// A request for the same member that arrives while an earlier one is held
// keeps that earlier request's clock: when the delay ends, the swap starts for
// it even though the request that started the clock has given up.
func TestActivatorSwapDelayLaterRequestKeepsClock(t *testing.T) {
	const delay = 2 * time.Second
	fake := newFakeMemberController()
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, "r", nil)

	probeDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
		defer cancel()
		_, err := a.Acquire(ctx, delayPool("coder", delay))
		probeDone <- err
	}()
	waitFor(t, "the probe to be held", func() bool { return a.waitingFor("coder") == 1 })
	time.Sleep(time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	rel, err := a.Acquire(ctx, delayPool("coder", delay))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Acquire coder: %v", err)
	}
	rel()
	if err := <-probeDone; !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Errorf("probe err = %v, want ErrHoldBudgetExceeded", err)
	}
	// Started about 1s after the probe, so the swap is due about 1s in. A
	// clock of its own would hold it the full 2s.
	if elapsed > 1600*time.Millisecond {
		t.Errorf("Acquire returned after %s; the swap should start when the probe's delay ends, about 1s in", elapsed)
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("coder activate count = %d, want 1", got)
	}
}

// Once every request for a member has left, the next one starts a fresh clock:
// a probe that gave up earlier does not shorten a later request's delay.
func TestActivatorSwapDelayRestartsAfterEveryRequestLeft(t *testing.T) {
	const delay = 400 * time.Millisecond
	fake := newFakeMemberController()
	fake.setPhase("judge", modelReadyPhase)
	a := NewActivator(context.Background(), fake, "r", nil)

	probeCtx, probeCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	_, err := a.Acquire(probeCtx, delayPool("coder", delay))
	probeCancel()
	if !errors.Is(err, ErrHoldBudgetExceeded) {
		t.Fatalf("probe err = %v, want ErrHoldBudgetExceeded", err)
	}
	waitFor(t, "the probe to leave", func() bool { return a.waitingFor("coder") == 0 })
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	rel, err := a.Acquire(ctx, delayPool("coder", delay))
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Acquire coder: %v", err)
	}
	rel()
	if elapsed < delay {
		t.Errorf("Acquire returned after %s, want at least the full %s delay", elapsed, delay)
	}
	if got := fake.activateCount("coder"); got != 1 {
		t.Errorf("coder activate count = %d, want 1", got)
	}
}
