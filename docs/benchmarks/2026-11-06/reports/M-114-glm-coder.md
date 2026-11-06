[jobs:job-done] job 261106-131252-ede6 BENCH M #114 glm failed · 4 turns · $0.32 · 40m · model openrouter:z-ai/glm-5.3 · effort high · report has no Summary section: likely stopped mid-task · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)
error: report has no Summary section: likely stopped mid-task; not auto-continued: job 261106-131252-ede6 runs on an openrouter model (billing api): it is not resumed or continued; start a new job with what it left (its report, its commits)

## Сводка
Проблема №114: унаследованный дочерний процесс `cat-file --batch` в `materializeTree` мог пережить таймаут, поскольку stdout передавался как `*os.File`, который вызывающая сторона считывает сама, из-за чего рипер преждевременно завершал git-процесс, и групповое уничтожение (group kill) превращалось в no-op. Я добавил отслеживаемые вручную потоки в протокол завершения перед рипингом в git runner, добавил регрессионный тест и закоммитил изменения в `bench/114-glm` как `d3e27ed`.

## Результаты / Изменения
- `internal/jobs/gitrun.go`:
  - `gitProc.manual []chan struct{}` (gitrun.go:55) + `(*gitProc).trackOutput()` (gitrun.go:158): вызывающая сторона, которая сама считывает stdout/stderr, помечает поток из `startGit`'s setup; рипер сохраняет зарезервированный PID git'а (ID группы) до тех пор, пока поток не завершится, группа не будет уничтожена или не истечет `gitWaitDelay` после завершения git'а.
  - `reap()` (gitrun.go:191-200): ждет каждый отслеживаемый поток (`ended` / `killed` / `gitWaitDelay`→`signal()`) перед установкой `reaped` — зеркально отражает пайпы, копируемые рипером, поэтому уничтожение по дедлайну все еще достигает потомка, удерживающего поток.
  - `startGit`'s setup сигнатура теперь `func(*exec.Cmd, *gitProc)` (gitrun.go:81); места вызова `runGit` (gitrun.go:279) и `sources.go:471` обновлены механически (`_ *gitProc`, поведение не изменилось).
- `internal/jobs/gate.go` (`materializeTree`):
  - Регистрирует вручную считываемый stdout: `readDone = gp.trackOutput()` в setup (gate.go:519-522).
  - Отложенная очистка теперь закрывает `outPipe` и `readDone` перед `p.wait(ctx)` (gate.go:550-555), высвобождая резервирование на каждом пути (все блобы прочитаны, ошибка чтения, пайп закрыт по дедлайну). Раньше `outPipe` закрывался только после ожидания.
- `internal/jobs/git_timeout_test.go:290-336`: новый `TestMaterializeTreeTimeoutKillsCatFileChild` — фейковый `cat-file` запускает `sleep 60`, наследуя stdout, записывает свой PID (через `FAKE_GIT_STATE`) и сразу завершается; утверждает, что `materializeTree` возвращает `errGitTimeout` и дочерний процесс завершился. Использует `gitLongTimeout=1s` с `gitWaitDelay=30s` (как и существующий `TestGitTimeoutKillsChildAfterGitExit`), чтобы уничтожение явно происходило по дедлайну, а не по встроенной задержке рипера.
- Коммит: `d3e27ed` на `bench/114-glm` (4 файла, +99/−14), не отправлен (pushed). Трейлер: `Requirements: none (issue #114, no numbered brief requirements)` — следуя соглашению репозитория, подтвержденному существующими коммитами.

## Доказательства
- Сначала воспроизведение, без фикса: `go test ./internal/jobs/ -run TestMaterializeTreeTimeoutKillsCatFileChild -count=1` → `--- FAIL: ... cat-file's child 3507123 still runs after the deadline: git's group was not killed`, exit=1.
- После фикса, тот же тест: `--- PASS (1.02s)`, exit=0.
- Полный пакет (единственный измененный): `go test ./internal/jobs/ -count=1 -timeout 30m` → `ok github.com/qwwiwi/agent-jobs/internal/jobs 758.025s`, exit=0, 0 `--- FAIL`. (Два предыдущих запуска превысили 9-минутный лимит, который я установил; `scripts/check.sh` документирует, что internal/jobs занимает ~14 минут на загруженном хосте, №97, канонический лимит 45м — и параллельно выполнялся набор тестов другого задания. Дамп goroutine в этих запусках показал отсутствие зависших тестов.)
- Целевые подмножества: git-runner (`TestGit|TestMaterialize|TestDaemonGit|TestGroupKill|TestProcessGone|TestSource|TestRaw`) `ok 24.912s`, exit=0; gate (`TestGate|TestJudge|TestDrop|TestSafeTree`) `ok 20.876s`, exit=0.
- `go vet ./...` exit=0; `gofmt -l internal/jobs/` чисто; `scripts/check-commit-messages.sh HEAD~1..HEAD` exit=0; `go build ./...` exit=0.
- Проанализированные файлы: internal/jobs/gitrun.go, gate.go, sources.go, finish.go (`gitRaw`), manager.go (`daemonGitEnv`), git_timeout_test.go, gitrun_test.go, gitrun_wait_linux.go, scripts/check.sh.

## Не сделано / неопределенно
- `-race` не запущено: cgo отключен, а на хосте отсутствует gcc (`CGO_ENABLED=1 go test -race` → "C compiler gcc not found"). Новая логика, изменяющая concurrency в `reap`/`trackOutput`, не проверена на детекторе гонок данных (data race); логика доступа к `p.manual` аналогична существующей, только-запись в setup перед запуском goroutine рипера.
- Путь ожидания darwin (`gitrun_wait_darwin.go`, kqueue/sysctl): перекрестная сборка и проверка ветом (vetted) согласно задаче, но не запущена на macOS (linux-хост) — то же предостережение, что и в задаче.
- `go test ./...` по всем репозиториям не запускался, только измененный пакет `internal/jobs` (что требует задание) плюс `go build`/`go vet ./...`; `scripts/check.sh` целиком не запускался, так как он запускает `go test ./...` с таймаутом 45м.
- Подагенты не использовались (изменение локализовано; `internal/jobs` — единственный потребитель `startGit`'s setup).

## Прогресс
- 17:02 UTC — прочитал gitrun.go, gate.go materializeTree, git_timeout_test.go, sources.go, manager.go; создал ветку bench/114-glm, написал план PROGRESS.md
- 17:07 UTC — добавил TestMaterializeTreeTimeoutKillsCatFileChild; подтвердил FAIL на неисправленном коде (дочерний элемент остался жив, exit=1)
- 17:12 UTC — исправил gitrun.go (trackOutput + manual-stream ожидание в reap, сигнатура setup), gate.go (track + close before wait), sources.go/runGit места вызова
- 17:15 UTC — go build ./... и go vet ./internal/jobs exit=0; регрессионный тест PASS
- 17:19 UTC — полный запуск пакета превысил лимит 9м (хост загружен параллельным набором тестов задания); перезапущен с большим таймаутом
- 17:26 UTC — целевые подмножества git-runner/gate ok exit=0; go vet ./... exit=0; gofmt чисто
- 17:31 UTC — закоммитил d3e27ed на bench/114-glm (проверка сообщений коммитов exit=0); перезапустил полный набор с таймаутом 30м
- 17:42 UTC — полный internal/jobs suite ok 758.025s exit=0; завершил PROGRESS.md
