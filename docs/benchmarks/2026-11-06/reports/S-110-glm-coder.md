[jobs:job-done] job 261106-131243-6798 BENCH S #110 glm succeeded · 4 turns · $0.13 · 24m · model openrouter:z-ai/glm-5.3 · effort high · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)

Посторонние процессы принадлежат другим заданиям (другим каталогам), а не моему. Всё готово — финальный отчёт:

## Not done / uncertain
- `scripts/check.sh` (весь репозиторий: gofmt + vet + `go test ./...`) не был запущен полностью; вместо этого были запущены `gofmt`, `go vet ./...` и весь пакет `internal/jobs` (проверка коммитов также пропущена, поскольку коммит на английском языке был создан после их запуска). Весь пакет `./...` занимает значительно больше 14 минут, из-за чего таймаут задания был слишком тесным для еще одного полного запуска; единственный измененный файл — это тестовый файл в `internal/jobs`, который был запущен полностью.
- UI-проверка не требуется (изменение затрагивает только Go-тест).
- Итерация воспроизведения с под-тестом (сначала была предпринята попытка) не удалась по артефактному принципу (намеренный сбой под-теста помечает родителя); он был заменен на представленный выше двухуровневый дочерний тест, который корректно воспроизводит проблему.

## Progress
- 13:15 UTC Прочитал testproc_test.go и помощники; подтвердил утечку; спланировал воспроизведение → исправление → тесты → коммит
- 13:16 UTC Добавил вспомогательный режим stall и первый черновик теста на утечку; запуск -> подтвердил утечку (pid 1918773), завершил утекший вспомогательный процесс
- 13:17 UTC Добавил startHelperOwned и подключил его к TestStartedDaemonNeverOutlivesItsTest; сфокусированные тесты пройдены, но тест на воспроизведение провалился из-за артефакта под-теста
- 13:18 UTC Переписал воспроизведение как TestPidWaitFailHelper + TestFailedPidWaitNeverLeaksHelper; сфокусированные тесты пройдены
- 13:19 UTC Проверил, что финальное воспроизведение дает сбой со старым паттерном (pid 1935701, завершил); восстановил исправление; gofmt/vet чисты; запустил весь пакет в фоне; создал ветку bench/110-glm
- 13:36 UTC Весь пакет ok (1020s, exit 0); закоммитил 9eae3b5; нет лишних процессов из этого задания; PROGRESS.md завершен

## Summary
Issue #110: `TestStartedDaemonNeverOutlivesItsTest` зарегистрировал очистку для своего вспомогательного процесса только после ожидания PID-файла, поэтому неудачное ожидание могло привести к утечке вспомогательного процесса (и его демона) до конца его двухминутного ожидания. Я переместил очистку прямо после `Start` (новый `startHelperOwned`, координированный с единственной горутиной `c.Wait`), добавил вспомогательный режим "stall" и два новых теста, подтверждающих исправление, и закоммитил в новую локальную ветку `bench/110-glm`.

## Findings / Changes
Всё в `internal/jobs/testproc_test.go`, коммит `9eae3b5` на ветке `bench/110-glm` (базовый `8f6e8f9`):
- `startHelperOwned` (`testproc_test.go:46`): вызывает `c.Start()`, порождает единственную горутину `c.Wait` (которая подает данные в буферизованный канал `done` и закрывает канал `exited`), и регистрирует `t.Cleanup` сразу — до любого ожидания — которое отправляет SIGKILL вспомогательному процессу, а затем присоединяется к горутине ожидания (`<-exited`, с запасным вариантом `t.Errorf` на 30 секунд). Исправляет проблему с утечкой.
- `TestStartedDaemonNeverOutlivesItsTest` (`testproc_test.go:114`): заменяет свой встроенный `c.Start`/wait-горутину на `startHelperOwned`, сохраняя существующее поведение (источник `done` для оператора select).
- `TestOwnedHelperDaemon` (`testproc_test.go:82`): новый режим `stall` — зависает на 2 минуты до записи PID-файла, чтобы ожидание родительского процесса не прошло успешно.
- `TestPidWaitFailHelper` (`testproc_test.go:192`): новый дочерний тест с контролем env, который запускает вспомогательный процесс stall, публикует его PID, затем завершается с ошибкой `waitFor` — точный путь утечки из issue.
- `TestFailedPidWaitNeverLeaksHelper` (`testproc_test.go:210`): запускает этот дочерний тест, утверждает, что он завершился с ошибкой ожидания (ненулевой exit), и что его вспомогательный PID больше не активен.

## Evidence
- Воспроизведение ДО исправления (старый паттерн Start-then-wait-then-cleanup): `go test ./internal/jobs -run 'TestFailedPidWaitNeverLeaksHelper' -count=1` → exit 1, `the failed pid wait leaked its helper (pid 1918773)`, а затем снова с pid 1935701 (утекшие вспомогательные процессы были завершены вручную; `pgrep` подтверждает, что мои задания не оставили никаких процессов).
- ПОСЛЕ исправления, сфокусировано: `go test ./internal/jobs -run 'TestFailedPidWaitNeverLeaksHelper|TestPidWaitFailHelper|TestStartedDaemonNeverOutlivesItsTest' -count=1 -v` → exit 0, `PASS TestFailedPidWaitNeverLeaksHelper (2.03s)`, `PASS TestStartedDaemonNeverOutlivesItsTest (1.26s)`.
- Весь затронутый пакет: `go test -count=1 -timeout 45m ./internal/jobs` → `ok github.com/qwwiwi/agent-jobs/internal/jobs 1020.402s`, exit 0.
- `go vet ./...` → exit 0; `gofmt -l internal/jobs/` → пусто; `git status` чистый после коммита `9eae3b5`.
