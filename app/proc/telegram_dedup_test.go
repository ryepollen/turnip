package proc

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestPluralDays(t *testing.T) {
	tests := []struct {
		n    int
		want string
	}{
		{1, "1 день"},
		{2, "2 дня"},
		{3, "3 дня"},
		{4, "4 дня"},
		{5, "5 дней"},
		{11, "11 дней"},
		{12, "12 дней"},
		{14, "14 дней"},
		{21, "21 день"},
		{22, "22 дня"},
		{25, "25 дней"},
		{101, "101 день"},
		{111, "111 дней"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, pluralDays(tt.n), "pluralDays(%d)", tt.n)
	}
}

func TestHumanAgo(t *testing.T) {
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		when time.Time
		want string
	}{
		{"just now", now.Add(-30 * time.Second), "только что"},
		{"minutes", now.Add(-5 * time.Minute), "5 мин назад"},
		{"hours", now.Add(-3 * time.Hour), "3 ч назад"},
		{"one day", now.Add(-25 * time.Hour), "1 день назад"},
		{"three days", now.Add(-3 * 24 * time.Hour), "3 дня назад"},
		{"five days", now.Add(-5 * 24 * time.Hour), "5 дней назад"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, humanAgo(tt.when, now))
		})
	}
}

func TestDupMarkerAndNote(t *testing.T) {
	now := time.Now()

	assert.Equal(t, "", dupMarker(dupNone, time.Time{}))
	assert.Equal(t, "⏳ в очереди", dupMarker(dupInQueue, time.Time{}))
	assert.Equal(t, "✅ в ленте", dupMarker(dupInFeed, time.Time{}))
	assert.Contains(t, dupMarker(dupInFeed, now.Add(-3*24*time.Hour)), "✅ в ленте (3 дня назад)")

	assert.Equal(t, "", dupNote(dupNone, time.Time{}))
	assert.Contains(t, dupNote(dupInQueue, time.Time{}), "в очереди")
	assert.Contains(t, dupNote(dupInFeed, time.Time{}), "Уже в ленте")
	assert.Contains(t, dupNote(dupInFeed, now.Add(-2*24*time.Hour)), "2 дня назад")
}
