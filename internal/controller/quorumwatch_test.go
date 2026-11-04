package controller

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func TestAnAgentWithoutLeaderAlertsByItselfOnceItLasted(t *testing.T) {
	alerts, _, webhook, discord := newTestAlerts(t)
	known := true
	watch := NewQuorumWatch(alerts, "node2", func() bool { return known })
	ctx := context.Background()
	start := time.Now()
	key := "no-leader/node2"

	watch.round(ctx, start)
	known = false
	watch.round(ctx, start.Add(time.Minute))
	watch.round(ctx, start.Add(time.Minute+noLeaderFor-time.Second))
	if len(webhook.got()) != 0 {
		t.Fatalf("alerted before no leader lasted %v: %v", noLeaderFor, webhook.got())
	}
	// a channel that is down gets it at the next round
	discord.mu.Lock()
	discord.failing = true
	discord.mu.Unlock()
	watch.round(ctx, start.Add(time.Minute+noLeaderFor))
	discord.mu.Lock()
	discord.failing = false
	discord.mu.Unlock()
	watch.round(ctx, start.Add(time.Minute+noLeaderFor+alertsInterval))
	if got := fmt.Sprint(webhook.got()); got != "[raised "+key+"]" {
		t.Errorf("webhook %s", got)
	}
	if got := fmt.Sprint(discord.got()); got != "[raised "+key+"]" {
		t.Errorf("discord %s", got)
	}
	watch.round(ctx, start.Add(time.Minute+noLeaderFor+remindEvery))
	known = true
	watch.round(ctx, start.Add(time.Minute+noLeaderFor+remindEvery+time.Second))
	watch.round(ctx, start.Add(time.Minute+noLeaderFor+remindEvery+2*time.Second))
	if got := fmt.Sprint(webhook.got()); got != "[raised "+key+" reminder "+key+" resolved "+key+"]" {
		t.Errorf("webhook %s", got)
	}
}
