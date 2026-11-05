package store

import (
	"path/filepath"
	"slices"
	"testing"
	"time"
)

func TestSourceFilesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jobs.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	origins := []string{"/reports/notes, final.md", "job:261105-000000-abcd"}
	j := &Job{ID: "review", Agent: "test", Title: "review", Kind: "review", Brief: "read", Status: StatusQueued,
		Model: "sonnet", CreatedAt: time.Now(), SourceFiles: origins}
	if err := st.Insert(j); err != nil {
		_ = st.Close()
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got, err := st.Get(j.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.SourceFiles, origins) || len(got.Sources) != 0 {
		t.Fatalf("file origins mixed with disabled clones: files=%v clones=%v", got.SourceFiles, got.Sources)
	}
}
