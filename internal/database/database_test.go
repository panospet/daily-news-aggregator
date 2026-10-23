package database

import (
	"testing"
	"time"
)

func TestTimeAgo_JustNow(t *testing.T) {
	result := TimeAgo(time.Now())
	if result != "just now" {
		t.Errorf("expected 'just now', got %q", result)
	}
}

func TestTimeAgo_Minutes(t *testing.T) {
	result := TimeAgo(time.Now().Add(-5 * time.Minute))
	if result != "5 mins ago" {
		t.Errorf("expected '5 mins ago', got %q", result)
	}
}

func TestTimeAgo_OneMinute(t *testing.T) {
	result := TimeAgo(time.Now().Add(-1 * time.Minute))
	if result != "1 min ago" {
		t.Errorf("expected '1 min ago', got %q", result)
	}
}

func TestTimeAgo_Hours(t *testing.T) {
	result := TimeAgo(time.Now().Add(-3 * time.Hour))
	if result != "3 hours ago" {
		t.Errorf("expected '3 hours ago', got %q", result)
	}
}

func TestTimeAgo_OneHour(t *testing.T) {
	result := TimeAgo(time.Now().Add(-1 * time.Hour))
	if result != "1 hour ago" {
		t.Errorf("expected '1 hour ago', got %q", result)
	}
}

func TestTimeAgo_Days(t *testing.T) {
	past := time.Now().Add(-48 * time.Hour)
	result := TimeAgo(past)
	loc, _ := time.LoadLocation("Europe/Athens")
	expected := past.In(loc).Format("Jan 2 15:04")
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}
