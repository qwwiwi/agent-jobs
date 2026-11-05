package jobs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// Prepare and deliver without starting a runner, proxy or socket.
func deliveredResumeSkills(t *testing.T, ignored bool) (*Manager, *store.Job, string) {
	t.Helper()
	m, _ := skillsManager(t)
	repo := newRepo(t)
	for _, name := range []string{"CLAUDE.md", "CLAUDE.local.md", ".claude/settings.json", ".claude/skills/local/SKILL.md"} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, name), []byte("original instructions\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, repo, "add", name)
	}
	gitT(t, repo, "commit", "-qm", "initial instructions")
	if ignored {
		if err := os.WriteFile(filepath.Join(repo, ".gitignore"), []byte(".claude/\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		gitT(t, repo, "add", ".gitignore")
		gitT(t, repo, "commit", "-qm", "ignore local instructions")
	}
	j := enqueue(t, m, "resume guard", func(r *StartRequest) { r.Kind, r.RepoPath = "fix", repo })
	if err := m.Store.Claim(j.ID, "skills-test", m.Now()); err != nil {
		t.Fatal(err)
	}
	j = get(t, m, j.ID)
	work := filepath.Join(m.JobDir(j.ID), "work")
	if err := os.MkdirAll(filepath.Dir(work), 0o700); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "worktree", "add", "--detach", work, j.BaseCommit)
	if err := m.deliverSkills(j, "skills-test", work); err != nil {
		t.Fatal(err)
	}
	return m, get(t, m, j.ID), work
}

func TestResumeDeliveredSkillsUnchanged(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		name := "runtime-exclude"
		if ignored {
			name = "ignored-parent"
		}
		t.Run(name, func(t *testing.T) {
			m, j, work := deliveredResumeSkills(t, ignored)
			t.Logf("instruction status: %q", gitT(t, work, append([]string{"status", "--porcelain", "--ignored", "--untracked-files=all", "--"}, projectClaudePathspecs...)...))
			if err := m.checkProjectClaude(j, work); err != nil {
				t.Fatalf("unchanged runtime skills refused: %v", err)
			}
			// The runtime exclude block may be gone while an earlier link remains.
			if err := updateExclude(j.RepoPath, j.ID, nil); err != nil {
				t.Fatal(err)
			}
			if err := m.checkProjectClaude(j, work); err != nil {
				t.Fatalf("unchanged links without runtime excludes refused: %v", err)
			}
			if err := m.removeSkills(j); err != nil {
				t.Fatal(err)
			}
			if err := m.checkProjectClaude(j, work); err != nil {
				t.Fatalf("unchanged instructions after cleanup refused: %v", err)
			}
		})
	}
}

func TestResumeDeliveredSkillEditRefused(t *testing.T) {
	for _, cleanup := range []bool{false, true} {
		name := "link-present"
		if cleanup {
			name = "link-removed-by-cleanup"
		}
		t.Run(name, func(t *testing.T) {
			m, j, work := deliveredResumeSkills(t, false)
			if err := os.WriteFile(filepath.Join(work, ".claude", "skills", "golang", "SKILL.md"), []byte("changed instructions\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if cleanup {
				if err := m.removeSkills(j); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.checkProjectClaude(j, work); err == nil {
				t.Fatal("edited delivered skill accepted")
			}
		})
	}
}

func TestResumeDeliveredSkillsRecordedPaths(t *testing.T) {
	m, j, work := deliveredResumeSkills(t, false)
	// Delivery provenance must not depend on the current catalog configuration.
	m.Cfg.SkillRoots = nil
	if err := m.checkProjectClaude(j, work); err != nil {
		t.Fatalf("unchanged recorded delivery refused after catalog reconfiguration: %v", err)
	}
}

func TestResumeDeliveredSkillsRejectInstructionChanges(t *testing.T) {
	for _, name := range []string{
		".claude/settings.local.json", ".claude/settings.json",
		".claude/skills/custom/SKILL.md", ".claude/skills/local/SKILL.md",
		"CLAUDE.md", "CLAUDE.local.md", "nested/CLAUDE.md",
	} {
		t.Run(name, func(t *testing.T) {
			m, j, work := deliveredResumeSkills(t, true)
			path := filepath.Join(work, name)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("job instructions\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := m.checkProjectClaude(j, work); err == nil {
				t.Fatal("job instruction change accepted alongside delivered skills")
			}
		})
	}
}

func TestResumeDeliveredSkillsRejectReplacements(t *testing.T) {
	for _, name := range []string{"retargeted", "directory", "staged", "legacy", "unrecorded"} {
		t.Run(name, func(t *testing.T) {
			m, j, work := deliveredResumeSkills(t, false)
			link := filepath.Join(work, ".claude", "skills", "golang")
			switch name {
			case "retargeted", "directory":
				if err := os.Remove(link); err != nil {
					t.Fatal(err)
				}
				if name == "retargeted" {
					target, err := os.Readlink(filepath.Join(work, ".claude", "skills", "git-workflows"))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(target, link); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.Mkdir(link, 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(filepath.Join(link, "SKILL.md"), []byte("job skill\n"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			case "staged":
				gitT(t, work, "add", "-f", ".claude/skills/golang")
			case "legacy":
				j.SkillsManifest = ""
			case "unrecorded":
				target, err := os.Readlink(link)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(target, filepath.Join(filepath.Dir(link), "custom")); err != nil {
					t.Fatal(err)
				}
			}
			if err := m.checkProjectClaude(j, work); err == nil {
				t.Fatal("changed or unrecorded delivery accepted")
			}
		})
	}
}

func TestResumeLegacySkillsAfterCleanupRefused(t *testing.T) {
	for _, missingWork := range []bool{false, true} {
		for _, mode := range []string{"resume", "auto-continue"} {
			name := mode + "/work-present"
			if missingWork {
				name = mode + "/work-missing"
			}
			t.Run(name, func(t *testing.T) {
				m, j, work := deliveredResumeSkills(t, false)
				// Model a pre-manifest delivery, edited through its link before normal cleanup.
				if err := m.Store.SetSkillsDelivered(j.ID, "skills-test", j.SkillsDelivered, j.SkillsNote); err != nil {
					t.Fatal(err)
				}
				j = get(t, m, j.ID)
				if err := os.WriteFile(filepath.Join(work, ".claude", "skills", "golang", "SKILL.md"), []byte("changed instructions\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := m.removeSkills(j); err != nil {
					t.Fatal(err)
				}
				if _, err := os.Lstat(filepath.Join(work, ".claude", "skills", "golang")); !os.IsNotExist(err) {
					t.Fatalf("cleanup left the delivered link: %v", err)
				}
				if missingWork {
					if err := os.Rename(work, work+"-previous"); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "prompt.md"), []byte("original task\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := m.Store.Finish(j.ID, "skills-test", store.StatusFailed, 7, 0.4, 1, "turn limit", m.Now()); err != nil {
					t.Fatal(err)
				}
				const reason = "no verifiable baseline"
				if mode == "resume" {
					if _, err := m.Resume(j.ID, "continue", Limits{}); err == nil || !strings.Contains(err.Error(), reason) {
						t.Fatalf("legacy delivery resume must refuse with %q: %v", reason, err)
					}
				} else {
					unlock, err := m.lock()
					if err != nil {
						t.Fatal(err)
					}
					m.autoContinue(j.ID, stopTurnLimit)
					unlock()
					if got := get(t, m, j.ID); !strings.Contains(got.Error, "not auto-continued: ") || !strings.Contains(got.Error, reason) {
						t.Fatalf("legacy delivery auto-continuation must refuse with %q: status=%s error=%q", reason, got.Status, got.Error)
					}
				}
				if got := get(t, m, j.ID); got.Status != store.StatusFailed || got.Attempt != 1 || got.SkillsManifest != "" {
					t.Fatalf("refused legacy delivery changed: status=%s attempt=%d manifest=%q", got.Status, got.Attempt, got.SkillsManifest)
				}
			})
		}
	}
}

func TestResumeDeliveredSkillsSymlinkedWorkRefused(t *testing.T) {
	for _, mode := range []string{"admission", "launch"} {
		for _, name := range []string{"CLAUDE.md", ".claude/settings.local.json"} {
			t.Run(mode+"/"+name, func(t *testing.T) {
				m, _ := skillsManager(t)
				j := enqueue(t, m, "resume guard", func(r *StartRequest) { r.Kind = "fix" })
				if err := m.Store.Claim(j.ID, "skills-test", m.Now()); err != nil {
					t.Fatal(err)
				}
				j = get(t, m, j.ID)
				work := filepath.Join(m.JobDir(j.ID), "work")
				if err := os.MkdirAll(work, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := m.deliverSkills(j, "skills-test", work); err != nil {
					t.Fatal(err)
				}
				if len(j.SkillsDelivered) == 0 || j.SkillsManifest == "" || j.RepoPath != "" {
					t.Fatal("fixture must have recorded deliveries and no repository")
				}
				if err := os.WriteFile(filepath.Join(m.JobDir(j.ID), "prompt.md"), []byte("original task\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := m.Store.Finish(j.ID, "skills-test", store.StatusFailed, 7, 0.4, 1, "turn limit", m.Now()); err != nil {
					t.Fatal(err)
				}
				if mode == "launch" {
					if _, err := m.Resume(j.ID, "continue", Limits{}); err != nil {
						t.Fatalf("unchanged work refused at admission: %v", err)
					}
					if err := m.Store.Claim(j.ID, "resume-test", m.Now()); err != nil {
						t.Fatal(err)
					}
				}
				// Replace the root after admission too, to exercise launch-time revalidation.
				moved := work + "-previous"
				if err := os.Rename(work, moved); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(moved, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte("planted instructions\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, work); err != nil {
					t.Fatal(err)
				}
				var err error
				if mode == "admission" {
					_, err = m.Resume(j.ID, "continue", Limits{})
				} else {
					_, err = m.prepareResume(get(t, m, j.ID), "resume-test")
				}
				if err == nil || !strings.Contains(err.Error(), "work is not a real directory") {
					t.Fatalf("symlinked work must be refused at %s: %v", mode, err)
				}
			})
		}
	}
}
