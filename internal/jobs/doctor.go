package jobs

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/qwwiwi/agent-jobs/internal/store"
)

// doctor procs (reference, «Как мы убираем»): «проверка призраков (doctor procs) ищет процессы,
// пережившие свою джобу или сессию: превью-серверы, браузеры Playwright, циклы ожидания, строки
// running с мёртвым pid. По умолчанию только сообщает, убивает по флагу.»
//
// What is found (Linux, /proc; only processes of this user):
//
//   - job ghosts: a process that carries the job marker (AGENT_JOBS_JOB=<id>) of a job in this agent's
//     store that is finished (or whose wrapper is dead), or a process in the session of such a job
//     (session id = the job's recorded pgid) whose session leader is gone. Either way the process
//     must have started while the job ran ([StartedAt-1s, FinishedAt+1s], the job started in the
//     current boot), and for a session the earliest-started live member of the session too: the
//     kernel keeps a session id allocated while any member lives, so a session whose first member
//     predates or postdates the job is a reused id, not the job's. These are the only ones --kill
//     ends.
//   - leaders: a marked process in that window that leads its own session (sid == pid: it
//     daemonized, like a tmux server whose environment, and so every later pane, carries the
//     marker). It may now host other people's work, so it is reported, never killed.
//   - suspects: a process whose session leader is gone and whose command line looks like a preview
//     server, a browser or a wait loop. It cannot be tied to a job (it may be the agent's own or
//     another agent's), so it is reported, never killed.
//   - dead rows: running rows whose wrapper pid is dead, or that never got a wrapper.

// Ghost kinds.
const (
	GhostJob     = "job"
	GhostLeader  = "leader"
	GhostSuspect = "suspect"
)

// attrSlack widens a job's run window: stored times have whole seconds, and so has btime.
const attrSlack = time.Second

// DoctorKillGrace is the wait between SIGTERM and SIGKILL of a ghost.
var DoctorKillGrace = 3 * time.Second

// Ghost is one process doctor procs reports.
type Ghost struct {
	PID       int    `json:"pid"`
	PPID      int    `json:"ppid"`
	SID       int    `json:"sid"`
	Cmdline   string `json:"cmdline"`
	StartedAt string `json:"started_at,omitempty"`
	Kind      string `json:"kind"` // job | leader | suspect
	JobID     string `json:"job_id,omitempty"`
	Reason    string `json:"reason"`
	Killed    bool   `json:"killed,omitempty"`
	KillError string `json:"kill_error,omitempty"`

	start uint64 // /proc starttime (clock ticks since boot): pins the process for the kill
}

// DeadRow is a running row whose wrapper is gone.
type DeadRow struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	PID    int    `json:"pid"`
	Reason string `json:"reason"`
}

// ProcsReport is the result of doctor procs.
type ProcsReport struct {
	Supported bool      `json:"supported"`
	Kill      bool      `json:"kill"`
	Ghosts    []Ghost   `json:"ghosts"`
	DeadRows  []DeadRow `json:"dead_rows"`
	Killed    int       `json:"killed"`
	Note      string    `json:"note,omitempty"`
}

// procInfo is what the scan reads about one process.
type procInfo struct {
	pid, ppid, sid int
	state          string
	start          uint64
	cmdline        []string
	marker         string
}

// procStatFull parses /proc/<pid>/stat: state, ppid, session and starttime.
func procStatFull(pid int) (procInfo, error) {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procInfo{}, err
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return procInfo{}, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	f := strings.Fields(s[i+1:]) // state ppid pgrp session tty tpgid flags … starttime (index 19)
	if len(f) < 20 {
		return procInfo{}, fmt.Errorf("parse /proc/%d/stat", pid)
	}
	p := procInfo{pid: pid, state: f[0]}
	if p.ppid, err = strconv.Atoi(f[1]); err != nil {
		return procInfo{}, err
	}
	if p.sid, err = strconv.Atoi(f[3]); err != nil {
		return procInfo{}, err
	}
	if p.start, err = strconv.ParseUint(f[19], 10, 64); err != nil {
		return procInfo{}, err
	}
	return p, nil
}

// procMarker returns the job marker in /proc/<pid>/environ ("" when absent or unreadable).
func procMarker(pid int) string {
	b, err := os.ReadFile(filepath.Join(procRoot, strconv.Itoa(pid), "environ"))
	if err != nil {
		return ""
	}
	for _, kv := range strings.Split(string(b), "\x00") {
		if v, ok := strings.CutPrefix(kv, JobEnvVar+"="); ok {
			return v
		}
	}
	return ""
}

// procOwnedByMe reports whether /proc/<pid> belongs to this user.
func procOwnedByMe(pid int) bool {
	fi, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid)))
	if err != nil {
		return false
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid()
}

// userHZ is the kernel's USER_HZ (clock ticks per second of /proc times); 100 on every Linux ABI.
const userHZ = 100

// bootTime reads btime from /proc/stat.
func bootTime() (time.Time, error) {
	f, err := os.Open(filepath.Join(procRoot, "stat"))
	if err != nil {
		return time.Time{}, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "btime "); ok {
			n, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			if err != nil {
				return time.Time{}, err
			}
			return time.Unix(n, 0), nil
		}
	}
	return time.Time{}, errors.New("no btime in /proc/stat")
}

// selfAndAncestors are never reported or signalled.
func selfAndAncestors() map[int]bool {
	out := map[int]bool{os.Getpid(): true}
	p := os.Getppid()
	for n := 0; n < maxAncestry && p > 1; n++ {
		out[p] = true
		pp, err := procParent(p)
		if err != nil {
			break
		}
		p = pp
	}
	return out
}

func procExists(pid int) bool {
	_, err := os.Stat(filepath.Join(procRoot, strconv.Itoa(pid)))
	return err == nil
}

// SuspectKind classifies a command line as a preview server, browser or wait loop ("" = none).
func SuspectKind(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	line := " " + strings.ToLower(strings.Join(argv, " ")) + " "
	for _, w := range []string{"chrome", "chromium", "headless_shell", "playwright", "firefox", "msedge", "webkit"} {
		if strings.Contains(line, w) {
			return "browser"
		}
	}
	for _, w := range []string{" vite", "next dev", "next start", "webpack serve", "webpack-dev-server", "http.server",
		"http-server", "live-server", "storybook", " serve ", "npm run dev", "npm run preview", "npm run start",
		"pnpm dev", "pnpm preview", "pnpm run dev", "yarn dev", "yarn preview", "astro dev", "nuxt dev", "remix dev"} {
		if strings.Contains(line, w) {
			return "preview server"
		}
	}
	for _, a := range argv[:min(len(argv), 3)] {
		switch filepath.Base(a) {
		case "vite", "webpack-dev-server", "http-server", "live-server", "storybook", "serve":
			return "preview server"
		}
	}
	switch filepath.Base(argv[0]) {
	case "sh", "bash", "zsh", "dash", "ash", "ksh":
		if (strings.Contains(line, "while ") || strings.Contains(line, "until ")) &&
			(strings.Contains(line, "sleep") || strings.Contains(line, "pgrep") || strings.Contains(line, "kill -0")) {
			return "wait loop"
		}
	}
	return ""
}

// liveJob reports whether a job's own processes are legitimately alive: it is running and its
// wrapper still lives.
func liveJob(j *store.Job) bool {
	return j.Status == store.StatusRunning && (j.PID == 0 || alive(j.PID))
}

// DoctorProcs scans for processes that outlived their job or session and for running rows whose
// wrapper is dead. It only reports, unless kill is set: then every job ghost (never a leader or a
// suspect) gets SIGTERM, DoctorKillGrace, SIGKILL – through a pidfd opened first and only after its
// start time and session were re-read and found unchanged, so a reused pid is never signalled.
func (m *Manager) DoctorProcs(kill bool) (ProcsReport, error) {
	rep := ProcsReport{Kill: kill, Ghosts: []Ghost{}, DeadRows: []DeadRow{}}
	running, err := m.Store.ListByStatus(store.StatusRunning)
	if err != nil {
		return rep, err
	}
	now := m.Now()
	for _, j := range running {
		switch {
		case j.PID > 0 && !alive(j.PID):
			rep.DeadRows = append(rep.DeadRows, DeadRow{ID: j.ID, Title: j.Title, PID: j.PID,
				Reason: "running row, wrapper pid is dead (the next tick fails it)"})
		case j.PID == 0 && j.StartedAt != nil && now.Sub(*j.StartedAt) > LaunchGrace:
			rep.DeadRows = append(rep.DeadRows, DeadRow{ID: j.ID, Title: j.Title,
				Reason: "claimed, the wrapper never registered (the next tick fails it)"})
		}
	}
	if runtime.GOOS != "linux" || !procExists(os.Getpid()) {
		rep.Note = "process scan needs Linux /proc; only the job rows were checked"
		return rep, nil
	}
	rep.Supported = true
	boot, err := bootTime()
	if err != nil {
		return rep, fmt.Errorf("boot time: %w", err)
	}
	// Sessions of finished jobs: recorded pgid (= the wrapper's session id) -> job. Not running rows
	// whose wrapper died: their run window has no end, so a later session reusing the id would be
	// attributed; such a row goes by the marker only.
	sessions := map[int][]*store.Job{}
	for _, status := range []string{store.StatusSucceeded, store.StatusFailed, store.StatusTimeout, store.StatusCancelled,
		store.StatusStopped} {
		list, err := m.Store.ListByStatus(status)
		if err != nil {
			return rep, err
		}
		for _, j := range list {
			if j.PGID > 1 && j.FinishedAt != nil && !liveJob(j) {
				sessions[j.PGID] = append(sessions[j.PGID], j)
			}
		}
	}
	skip := selfAndAncestors()
	ents, err := os.ReadDir(procRoot)
	if err != nil {
		return rep, err
	}
	// The earliest start of every live session member (all users: a session id stays allocated while
	// any member lives, whoever owns it).
	type scanned struct {
		pid int
		p   procInfo
	}
	var mine []scanned
	earliest := map[int]uint64{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 {
			continue
		}
		p, err := procStatFull(pid)
		if err != nil || p.state == "Z" {
			continue
		}
		if s, ok := earliest[p.sid]; !ok || p.start < s {
			earliest[p.sid] = p.start
		}
		if !skip[pid] && procOwnedByMe(pid) {
			mine = append(mine, scanned{pid, p})
		}
	}
	at := func(start uint64) time.Time { return boot.Add(time.Duration(start) * time.Second / userHZ) }
	jobCache := map[string]*store.Job{}
	lookup := func(id string) *store.Job {
		if j, ok := jobCache[id]; ok {
			return j
		}
		j, err := m.Store.Get(id)
		if err != nil {
			j = nil // another agent's job, or an unknown id: not ours to judge
		}
		jobCache[id] = j
		return j
	}
	for _, sc := range mine {
		pid, p := sc.pid, sc.p
		p.cmdline, _ = procCmdline(pid)
		p.marker = procMarker(pid)
		started := at(p.start)
		g := Ghost{PID: pid, PPID: p.ppid, SID: p.sid, Cmdline: Printable(strings.Join(p.cmdline, " "), 300),
			StartedAt: started.UTC().Format(time.RFC3339), start: p.start}
		leaderGone := p.sid > 1 && p.sid != pid && !procExists(p.sid)
		var sessJob *store.Job // the finished job whose run window holds this process and its session
		if leaderGone {
			for _, j := range sessions[p.sid] {
				if ranDuring(j, started, boot) && ranDuring(j, at(earliest[p.sid]), boot) {
					sessJob = j
					break
				}
			}
		}
		switch {
		case p.marker != "":
			j := lookup(p.marker)
			if j == nil || liveJob(j) || !ranDuring(j, started, boot) {
				continue
			}
			g.Kind, g.JobID = GhostJob, j.ID
			if j.Status == store.StatusRunning {
				g.Reason = fmt.Sprintf("carries the marker of job %s, whose wrapper is dead", j.ID)
			} else {
				g.Reason = fmt.Sprintf("carries the marker of job %s, which is %s", j.ID, j.Status)
			}
			switch {
			case p.sid == pid:
				g.Kind = GhostLeader
				g.Reason += "; it leads its own session (daemonized) and may host other work: reported, never killed"
			case p.sid > 1 && p.sid != j.PGID && procExists(p.sid):
				g.Kind = GhostLeader
				g.Reason += "; its session leader is alive and is not the job's wrapper (it may run under a daemon): reported, never killed"
			case underMarkedLeader(p.ppid, j.PGID):
				g.Kind = GhostLeader
				g.Reason += "; a marked session leader (daemonized) is among its ancestors: reported, never killed"
			}
		case sessJob != nil:
			j := sessJob
			g.Kind, g.JobID = GhostJob, j.ID
			g.Reason = fmt.Sprintf("in the session of job %s (%s), its wrapper is gone", j.ID, j.Status)
		case leaderGone:
			kind := SuspectKind(p.cmdline)
			if kind == "" {
				continue
			}
			g.Kind = GhostSuspect
			g.Reason = kind + " whose session leader is gone (not tied to a job: reported, never killed)"
		default:
			continue
		}
		rep.Ghosts = append(rep.Ghosts, g)
	}
	slices.SortFunc(rep.Ghosts, func(a, b Ghost) int { return a.PID - b.PID })
	if !kill {
		return rep, nil
	}
	for i := range rep.Ghosts {
		g := &rep.Ghosts[i]
		if g.Kind != GhostJob {
			continue
		}
		if err := killGhost(g.PID, g.SID, g.start); err != nil {
			g.KillError = err.Error()
			continue
		}
		g.Killed = true
		rep.Killed++
		m.Log.Printf("doctor procs: killed pid %d (%s)", g.PID, g.Reason)
	}
	return rep, nil
}

// underMarkedLeader reports whether a process whose parent is ppid descends from a marked process
// that leads its own session (a daemon the job started), other than the job's wrapper (pgid).
func underMarkedLeader(ppid, pgid int) bool {
	for n := 0; n < maxAncestry && ppid > 1; n++ {
		a, err := procStatFull(ppid)
		if err != nil {
			return false
		}
		if a.sid == ppid && ppid != pgid && procMarker(ppid) != "" {
			return true
		}
		ppid = a.ppid
	}
	return false
}

// ranDuring reports whether a process that started at t can belong to job j: the job started in
// the current boot (pids and session ids of an earlier boot mean nothing now) and t lies within
// [StartedAt-attrSlack, FinishedAt+attrSlack] (no upper bound for a running row whose wrapper died:
// such a row is matched by the marker only).
func ranDuring(j *store.Job, t, boot time.Time) bool {
	if j.StartedAt == nil || j.StartedAt.Before(boot) || t.Before(j.StartedAt.Add(-attrSlack)) {
		return false
	}
	return j.FinishedAt == nil || !t.After(j.FinishedAt.Add(attrSlack))
}

// samePinned reports whether pid is still the process that was scanned (start time and session).
func samePinned(pid, sid int, start uint64) bool {
	p, err := procStatFull(pid)
	return err == nil && p.state != "Z" && p.start == start && p.sid == sid
}

// killGhost ends one ghost: pidfd first, identity re-checked after it is open, SIGTERM, a grace
// period, SIGKILL. Without pidfd support nothing is signalled.
func killGhost(pid, sid int, start uint64) error {
	fd, err := pidfdOpen(pid)
	if errors.Is(err, unix.ESRCH) {
		return nil // gone already
	}
	if err != nil {
		return fmt.Errorf("pidfd_open %d: %w (not signalled)", pid, err)
	}
	defer unix.Close(fd)
	if !samePinned(pid, sid, start) {
		return fmt.Errorf("pid %d is no longer the scanned process (not signalled)", pid)
	}
	send := func(sig syscall.Signal) error {
		if err := pidfdSend(fd, sig); err != nil && !errors.Is(err, unix.ESRCH) {
			return fmt.Errorf("pidfd_send_signal %d: %w", pid, err)
		}
		return nil
	}
	if err := send(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(DoctorKillGrace)
	for time.Now().Before(deadline) {
		if !samePinned(pid, sid, start) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err := send(syscall.SIGKILL); err != nil {
		return err
	}
	for i := 0; i < 40 && samePinned(pid, sid, start); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if samePinned(pid, sid, start) {
		return fmt.Errorf("pid %d survived SIGKILL", pid)
	}
	return nil
}
