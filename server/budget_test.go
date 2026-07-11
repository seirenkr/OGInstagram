package main

import (
	"testing"
	"time"
)

func TestHourlyBudgetCountsSuccessAndResets(t *testing.T) {
	p := &SessionPool{cfg: Config{GlobalHourlyLimit: 2}}

	if p.overBudget() {
		t.Fatal("fresh pool should not be over budget")
	}
	p.countRequest() // 1 success
	if p.overBudget() {
		t.Fatal("1/2 should not be over budget")
	}
	p.countRequest() // 2 success -> at cap
	if !p.overBudget() {
		t.Fatal("2/2 should be over budget")
	}
	if got := p.pick(nil); got != nil {
		t.Fatal("pick over budget should return nil")
	}

	// Rolling window: after an hour the budget resets.
	p.globalWindowStart = time.Now().Add(-time.Hour - time.Minute)
	if p.overBudget() {
		t.Fatal("budget should reset after the hour window")
	}
}

func TestHourlyBudgetUnlimitedWhenZero(t *testing.T) {
	p := &SessionPool{cfg: Config{GlobalHourlyLimit: 0}}
	for i := 0; i < 100; i++ {
		p.countRequest()
	}
	if p.overBudget() {
		t.Fatal("limit 0 means unlimited")
	}
}
