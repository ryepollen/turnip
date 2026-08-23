package publisher

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeStub lays out zero-byte placeholder parts under a work — the manifest
// skeleton variant A mirrors to the VM in place of the real archive. Catalog
// must treat these exactly like real audio: it reads names + work.yaml only,
// never bytes.
func writeStub(t *testing.T, svc *Service, category, work string, parts ...string) {
	t.Helper()
	for _, rel := range parts {
		p := filepath.Join(svc.categoryDir(category), work, filepath.FromSlash(rel))
		require.NoError(t, os.MkdirAll(filepath.Dir(p), 0o750))
		require.NoError(t, os.WriteFile(p, nil, 0o600)) // 0 bytes
	}
}

// TestCatalogStubSkeleton locks the load-bearing assumption of variant A: a tree
// of empty stub files (the kilobyte "manifest" the Mac agent mirrors to the VM
// instead of the gigabyte archive) yields the same Catalog() as real audio would.
// If anyone ever adds a size or content check to gatherParts, this fails loudly.
func TestCatalogStubSkeleton(t *testing.T) {
	svc := &Service{AudioDir: t.TempDir()}

	// flat work, zero-byte parts
	writeStub(t, svc, "books", "Гэри Стивенсон — Бешеные деньги",
		"01 - Часть 1.mp3", "02 - Часть 2.mp3")
	// every supported extension, recognized from the name alone
	writeStub(t, svc, "courses", "Курс",
		"01 - Урок.m4a", "02 - Урок.m4b", "03 - Урок.aac", "04 - Урок.ogg")
	// seasoned stub work + a real (tiny) work.yaml carried alongside the stubs
	writeStub(t, svc, "podcasts", "Шоу",
		"Сезон 01/01 - E1.mp3", "Сезон 01/02 - E2.mp3", "Сезон 02/01 - E1.mp3")
	require.NoError(t, os.WriteFile(
		filepath.Join(svc.categoryDir("podcasts"), "Шоу", "work.yaml"),
		[]byte("title: Настоящее шоу\ntype: streaming"), 0o600))

	cat, err := svc.Catalog()
	require.NoError(t, err)

	byslug := map[string]CatWork{}
	for _, w := range cat {
		byslug[w.Category+"/"+w.Slug] = w
	}

	// zero-byte parts counted exactly like real audio
	require.Contains(t, byslug, "books/Гэри Стивенсон — Бешеные деньги")
	assert.Equal(t, 2, byslug["books/Гэри Стивенсон — Бешеные деньги"].Parts)
	assert.Equal(t, 0, byslug["books/Гэри Стивенсон — Бешеные деньги"].Seasons)
	assert.True(t, byslug["books/Гэри Стивенсон — Бешеные деньги"].Finite)

	require.Contains(t, byslug, "courses/Курс")
	assert.Equal(t, 4, byslug["courses/Курс"].Parts, "mp3/m4a/m4b/aac/ogg all count from the name")

	// seasons parsed from stub subfolders; work.yaml (a real small file) still read
	require.Contains(t, byslug, "podcasts/Шоу")
	assert.Equal(t, 3, byslug["podcasts/Шоу"].Parts)
	assert.Equal(t, 2, byslug["podcasts/Шоу"].Seasons)
	assert.Equal(t, "Настоящее шоу", byslug["podcasts/Шоу"].Title, "work.yaml title honored")
	assert.False(t, byslug["podcasts/Шоу"].Finite, "streaming from work.yaml")
}
