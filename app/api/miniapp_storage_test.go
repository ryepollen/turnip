package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDiskUsage(t *testing.T) {
	used, total := diskUsage(t.TempDir())
	assert.Positive(t, total, "a real volume reports a size")
	assert.Positive(t, used)
	assert.Less(t, used, total)

	u0, t0 := diskUsage("/definitely/not/a/path")
	assert.Zero(t, u0)
	assert.Zero(t, t0)
}
