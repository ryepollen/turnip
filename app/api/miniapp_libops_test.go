package api

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/feed-master/app/publisher"
)

func TestLibOpRegistry(t *testing.T) {
	s := &Server{}
	cw := publisher.CatWork{Category: "books", Slug: "W", Title: "Книга"}

	require.True(t, s.libOpStart(publisher.OpActivate, cw))
	assert.False(t, s.libOpStart(publisher.OpDeactivate, cw), "second op in the category must be refused while running")

	ops := s.libOpsSnapshot()
	require.Contains(t, ops, "books")
	assert.True(t, ops["books"].Running)
	assert.Equal(t, "activate", ops["books"].Op)
	assert.Equal(t, "W", ops["books"].Work)

	s.libOpFinish("books", errors.New("part not yet in R2"))
	ops = s.libOpsSnapshot()
	assert.False(t, ops["books"].Running)
	assert.Contains(t, ops["books"].Error, "not yet in R2", "the failure reason survives for the next catalog poll")
	assert.False(t, ops["books"].Finished.IsZero())

	require.True(t, s.libOpStart(publisher.OpActivate, cw), "a finished op frees the category")
	ops = s.libOpsSnapshot()
	assert.Empty(t, ops["books"].Error, "a fresh start clears the stale failure")
}

func TestLibraryActionBusyConflict(t *testing.T) {
	// a real catalog on disk (no R2 needed: validation only scans names)
	svc := &publisher.Service{AudioDir: t.TempDir(), Secret: "s", BaseURL: "http://x"}
	dir := filepath.Join(svc.AudioDir, "originals", "books", "Занятая")
	require.NoError(t, os.MkdirAll(dir, 0o750))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "01 - One.mp3"), []byte("a"), 0o600))

	s := &Server{Pub: svc}
	require.True(t, s.libOpStart(publisher.OpActivate, publisher.CatWork{Category: "books", Slug: "Занятая"}))

	body := bytes.NewReader([]byte(`{"category":"books","work":"Занятая"}`))
	req := httptest.NewRequest(http.MethodPost, "/wegweiser/api/library/activate", body)
	rec := httptest.NewRecorder()
	s.appLibraryActivateCtrl(rec, req)

	assert.Equal(t, http.StatusConflict, rec.Code, "a running op must reject the double-tap, not spawn a second goroutine")
	assert.Contains(t, rec.Body.String(), "operation in progress")
}
