[jobs:job-done] job 261106-131244-54a0 BENCH L #134 glm succeeded · 8 turns · $0.76 · 48m · model openrouter:z-ai/glm-5.3 · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

## Not done / uncertain

- «Failed before» нельзя показать прогоном новых тестов на базе: `gaterunner.go` новый, идентификаторы не существуют в `8f6e8f9` — тесты не компилируются там. Поведение до фикса зафиксировано в комментариях тестов (конкурентные гейты, отсутствие exit-строки, TELEGRAM_ в env prepare); ручная демонстрация на базе не проводилась.
- `TELEGRAM_*` дроп покрыт тестом только на prepare; acceptance/manifest env — allow-list и так не содержали этих переменных (код, не новый тест).
- Лок `home/gate.lock` и фолбэк `state/gate.lock` покрыты тестами через собственный state-хоум теста; сценарий «неоткрываемый lock-файл → гейт без лока» в тестах не разыгран (ветка с `continue`/финальным return nil).
- Символьный шаг: бриф не просил `check_mark` — не оставлял.

## Progress

- 14:52 UTC — read the issue and the gate call sites (acceptance, manifest, prepare), rejected Wave F gate jobs as the target
- 14:52 UTC — implemented gaterunner.go (lock, head assert, marker, env drop) and wired it into acceptance.go, manifest.go, repoprepare.go; extracted verifyWorktreeAt in finish.go
- 14:52 UTC — added FAKE_DETACH handler and gaterunner_test.go (8 tests); fixed compile issues (jobDir param, acceptanceKillGrace const→var)
- 14:52 UTC — new tests pass (after fixing the two self-inflicted test bugs); affected suites pass; go vet clean
- 14:52 UTC — full go test ./... timed out at the harness default (package takes ~15m); reran via canonical scripts/check.sh — all green, "check.sh: OK"
- 14:52 UTC — committed bca5338 on bench/134-glm (local, not pushed, no PR per the brief); PROGRESS.md updated; final report written

## Summary

Реализовал issue #134 (serialized gate runner) в agent-jobs. Все тяжёлые локальные гейты враппера — acceptance command (check.sh, pg-сьюты), manifest-check шаг судьи и prepare шаг (pnpm install, vite build) — идут через общий `runShellGroup`, и теперь для каждого из них держатся четыре правила: один гейт на бокс за раз, assert HEAD, всегда печатается `exit=`, чистое окружение. Локальный коммит `bca5338` на ветке `bench/134-glm` (база `8f6e8f9`), не пушится, PR не открывается — по брифу.

## Findings / Changes

- **internal/jobs/gaterunner.go (новый, 176 строк):**
  - `acquireGateLock` — flock в agent-jobs home (родитель state-дира, общий для всех агентов на боксе), опрос `LOCK_EX|LOCK_NB` каждые 50 мс (паттерн tmux.lock); ожидание делит таймаут самого гейта; нет открываемого lock-файла → гейт идёт без лока (с записью в лог), а не падает.
  - `assertGateHead` — ворктри проверяется `verifyWorktreeAt` (это работа бОльшего кода, см. ниже), затем `merge-base --is-ancestor base HEAD`; отказ → гейт не запускается.
  - `gateExitMarker` — единая строка `run-wrapper: <step> exit=<n>[ head=<sha>][ (note)] after <dur>` на каждом пути завершения; head передаёт вызывающий (acceptance — asserted head, manifest — `g.SHA`).
  - `dropGateEnv` — выкидывает `TELEGRAM_*` из окружения prepare.
- **acceptance.go:** `runShellGroup` получил `jobDir`, берёт бокс-лок на всё время прогона; ожидание лока делит таймаут (очередник, не дождавшийся, = `AcceptanceTimedOut`, зависших ждунов нет); SIGTERM по таймауту идёт всей группе `-pid`, ESRCH терпится (иначе Wait сообщал бы ошибку килла вместо таймаута); acceptance-шаг сначала ассертит HEAD — при отказе ничего не записывает, печатает `exit=125` с причиной, джоб фейлится по существующему пути «acceptance did not run»; `runAcceptance` больше не пишет свою exit-строку; `acceptanceKillGrace` стал `var` (тесты укорачивают).
- **manifest.go / repoprepare.go:** `gateExitMarker` на каждом пути (включая signalled); env prepare = `GitEnv(jobDir, dropGateEnv(os.Environ()))`.
- **finish.go:** `verifyWorktree` → тонкая обёртка над новым `verifyWorktreeAt(jobDir, ...)` — враппер может верифицировать ворктри без Manager.
- **jobs_test.go:** хэндлер фейкового claude `FAKE_DETACH <sha>` (checkout --detach).
- **gaterunner_test.go (новый, 8 тестов):** сериализация (порядок по наносекундам), ограниченное ожидание лока, таблица точных строк маркера, head-ассерт, гейт на своём HEAD (`exit=0 head=` в stderr.log), отказ на чужом HEAD (джоб failed, ничего не записано, `exit=125`), дроп TELEGRAM_ при сохранении остальных переменных, килл группы по таймауту (ребёнок с `trap "" TERM` умирает).

Про «гейт на чужом HEAD»: тест создаёт несвязанный root-коммит через `commit-tree` — `merge-base --is-ancestor` даёт не-нуль, гейт отказывается до записи чего-либо.

## Evidence

- `go vet ./...` → exit **0**.
- Новые тесты: `go test ./internal/jobs/ -run 'TestGate|TestAssertGateHead|TestAcceptanceGateRunsAtTheJobsHead|TestAcceptanceWrongHeadRefused|TestPrepareEnvDropsTelegram' -count=1` → **ok** (18.4s), exit **0**. Первые прогоны падали (exit 1): `TestGateLockOneAtATime` — waitFor внутри `run()` до старта держателя; `TestGateExitMarkerAlwaysPrinted` — ожидал `15m`, а Go печатает `15m0s`; оба исправлены.
- Затронутые сьюты: `-run 'TestAcceptance|TestPrepare|TestManifest|TestWrapper|TestSweep' -count=1` → **ok** (169s), exit **0**.
- Канонический полный чек: `bash scripts/check.sh` → все фазы (commit messages, gofmt, go vet, go test ./...) зелёные, `check.sh: OK`, exit **0**; `internal/jobs` 899s. Отдельный первый `go test ./...` без чековского таймаута упал по 10-минутному дефолту пакета (603s) — это ограничение запуска, не тестов; check.sh задаёт `GO_TEST_TIMEOUT` под это (комментарий в scripts/check.sh:10).
- Коммит: `bca533870d4c5e96bb19c39a6422d9183a03d4d1`, 7 файлов, +539/−15, ветка `bench/134-glm`, локально.
