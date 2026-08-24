package main

import (
	"context"
	"testing"
	"time"

	"iris/wechat"
)

// TestPantheonClientListRuns 验证 PantheonClient.ListRuns 能正确调用 pantheon CLI
// 这个测试需要 pantheon daemon 运行，在 omarchy 上运行
func TestPantheonClientListRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	client := wechat.NewPantheonClient("pantheon", "")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	runs, err := client.ListRuns(ctx)
	if err != nil {
		t.Skipf("pantheon not available: %v", err)
	}
	if len(runs) == 0 {
		t.Log("no runs returned (expected if daemon is clean)")
	}
	t.Logf("got %d runs", len(runs))
}

// TestBeaconClientStatus 验证 BeaconClient.Status 能正确调用 beacon CLI
func TestBeaconClientStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	client := wechat.NewBeaconClient("")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	status, err := client.Status(ctx)
	if err != nil {
		t.Skipf("beacon not available: %v", err)
	}
	t.Logf("got %d panes", len(status.Panes))
}

// TestFormatRuns 验证 FormatRuns 输出格式
func TestFormatRuns(t *testing.T) {
	runs := []wechat.RunInfo{
		{RunID: "run_abc123", State: "completed", ResultState: "accepted", StartedAt: "2026-08-24T03:30:47Z"},
		{RunID: "run_def456", State: "running", ResultState: "", StartedAt: "2026-08-24T03:35:00Z"},
	}
	output := wechat.FormatRuns(runs, 5)
	if !contains(output, "run_abc") {
		t.Errorf("FormatRuns missing run ID: %s", output)
	}
	if !contains(output, "completed") {
		t.Errorf("FormatRuns missing state: %s", output)
	}
}

// TestFormatBeaconStatus 验证 FormatBeaconStatus 输出格式
func TestFormatBeaconStatus(t *testing.T) {
	status := &wechat.BeaconStatus{
		Panes: map[string]wechat.BeaconPane{
			"%5": {Session: "main", Window: "0", Status: "working", Summary: "test task", Cwd: "/tmp/test"},
		},
	}
	output := wechat.FormatBeaconStatus(status)
	if !contains(output, "%5") {
		t.Errorf("FormatBeaconStatus missing pane ID: %s", output)
	}
	if !contains(output, "▶") {
		t.Errorf("FormatBeaconStatus missing status icon for working: %s", output)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsStr(s, substr))
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
