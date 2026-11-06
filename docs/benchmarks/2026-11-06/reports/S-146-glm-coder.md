[jobs:job-done] job 261106-131245-42fd BENCH S #146 glm failed · 4 turns · $0.53 · 56m · model openrouter:z-ai/glm-5.3 · effort high · report has no Summary section: likely stopped mid-task · merge onto a branch not checked: the job's base is not a branch (a tag, a commit or a detached HEAD)
error: report has no Summary section: likely stopped mid-task; not auto-continued: job 261106-131245-42fd runs on an openrouter model (billing api): it is not resumed or continued; start a new job with what it left (its report, its commits)

## Сводка
Issue #146: `providers.openrouter.deny_repos` проверял только репозиторий самой задачи, поэтому источник файла (`job_start sources`) из отклоненного репозитория — или `job:<id>` отчет о нем — мог быть прикреплен к задаче OpenRouter и отправлен стороннему провайдеру. Я реализовал главное предложение из issue: источники файлов, находящиеся внутри отклоненного репозитория, и источники `job:<id>`, чья задача выполнялась в нем, отклоняются как при `job_start`, так и при каждом запуске. Закоммичено как `9db5f5c` в ветке `bench/146-glm` (локально, не отправлено на удаленный сервер).

## Результаты / Изменения
- `internal/jobs/openrouter.go:439` — новый `checkOpenRouterSources(origins, repoOf)`: источник файла отклоняется, если любой родительский каталог совпадает с записью из `deny_repos` (`deniedContainingRepo` проверяет каждый родительский каталог с той же семантикой, что и `deniedRepo`: абсолютная запись/glob против пути, имя против имени папки); источник `job:<id>` отклоняется, когда задача, на которую он ссылается, выполнялась в отклоненном репозитории, или (fail-closed) когда ее строку невозможно прочитать.
- `internal/jobs/manager.go:508` — `newJob` вызывает проверку для `openrouter:` задач сразу после `checkOpenRouterJob`.
- `internal/jobs/orproxy.go:946` — `setupOpenRouter` переработан для приема строки задачи (`*store.Job`) и `repoOf`; он заново проверяет `j.SourceFiles` при каждом запуске, как и сам `deny_repos`, чтобы репозиторий, отклоненный после постановки задачи в очередь, также отклонял и ее источники. `internal/jobs/wrapper.go:872` передает `wrapper` `store` как `repoOf`.
- `internal/jobs/source_files.go:98` — вспомогательный метод менеджера `sourceJobRepo(id)`.
- `docs/GUIDE.md:160,167` — оба абзаца `deny_repos` теперь описывают привязку источников.
- Тесты (`internal/jobs/openrouter_sources_test.go`): `TestOpenRouterSourcesDeniedRepo` (запись в очередь: файл внутри отклоненного репозитория по имени/абсолютному пути/glob отклонен, файл вне его принят, источник `job:` из отклоненного отклонен, из разрешенного принят) и `TestOpenRouterSourcesDeniedAtStart` (повторная проверка при запуске).

## Доказательства
- Воспроизведение перед исправлением: `go test ./internal/jobs/ -run TestOpenRouterSourcesDeniedRepo` завершился ошибкой `file inside the denied repo: <nil>` (задача успешно добавлена в очередь), и `TestOpenRouterSourcesDeniedAtStart` не смог скомпилироваться (проверки не существовало).
- После исправления: `go test ./internal/jobs/ -run 'TestOpenRouterSources' -count=1` → `ok ... 2.602s`.
- `go vet ./...` → exit 0; `gofmt -l internal/ cmd/` → пусто (без вывода).
- `scripts/check.sh` (репозиторий): gofmt/vet и каждый пакет прошли успешно (`internal/brain`, `guard`, `httpapi`, `mcp`, `store` и т.д.), кроме одного прогона `internal/jobs`, который был завершен сигналом SIGTERM на 913 с без имени какого-либо теста — четыре других задачи на этом хосте запускали свои собственные прогоны `internal/jobs` параллельно на момент выполнения (проверено с помощью `pgrep`). Чистый повторный запуск с собственным 45-минутным таймаутом скрипта: `ok github.com/qwwiwi/agent-jobs/internal/jobs 1018.157s`, `exit=0` (`jobs-retest.log` в директории задачи).
- Целевые регрессии вокруг этой области: `-run 'OpenRouter|Source|JobID|Wrapper'` → `ok ... 155.182s`.
- Коммит: `9db5f5c` «Refuse OpenRouter sources from denied repositories (#146)» на ветке `bench/146-glm`; `git status` чистый. Не выполнен push, PR не создан, никому не отправлялись сообщения. Никаких новых зависимостей.

## Не выполнено / неясно
- Вариант «полностью отклонить источники в задачах OpenRouter» из issue не реализован — вместо него реализовано главное предложение по гранулированному правилу; отмечаю это как выбор исполнителя для проверки владельцем (текущий пилотный режим владельца запускается без источников в любом случае, так что на него это никак не влияет).
- Повторная проверка при запуске не проверяет **содержимое** отчета об источнике `job:<id>` — только репозиторий исходной задачи; отчет о задаче без репозитория, которая работала с файлами из отклоненного репозитория, все равно может быть передан. Файлы брифа/промпта (основной вектор) покрыты; остаточный риск содержимого задокументирован здесь, а не в коде.
- Единственный прогон `check.sh` `internal/jobs` завершился сбоем из-за внешнего SIGTERM (одновременные наборы тестов других заданий на хосте); чистый повторный запуск оказался успешным, но полная интеграция `scripts/check.sh` в один запуск не была зафиксирована как успешная с первого раза.

## Прогресс
- 16:09 UTC Прочитал issue, изучил openrouter.go, source_files.go, manager.go, wrapper.go, orproxy.go; записал план в PROGRESS.md.
- 16:10 UTC Написал провальные тесты (TestOpenRouterSourcesDeniedRepo, TestOpenRouterSourcesDeniedAtStart); сборка завершилась ошибкой — проверка не существовала.
- 16:14 UTC Реализовал checkOpenRouterSources + deniedContainingRepo, sourceJobRepo, newJob и настройку setupOpenRouter при запуске, вызов wrapper.
- 16:17 UTC Убедился, что тест очереди задач воспроизводит утечку (err=nil), затем завершил wiring; оба новых теста пройдены.
- 16:22 UTC Проверил gofmt/vet; запустил набор тестов; обновил docs/GUIDE.md; проверил тесты документации и схему MCP.
- 16:27 UTC Закоммитил 9db5f5c в bench/146-glm; запустил scripts/check.sh в фоновом режиме.
- 16:31 UTC check.sh: все пакеты пройдены, кроме internal/jobs (внешний SIGTERM на 913 с; четыре одновременных набора тестов других задач на хосте).
- 16:43 UTC Повторный запуск internal/jobs с таймаутом 45 м (первая попытка завершилась с кодом 144 из-за того, что pkill совпал с собственной командной строкой; перезапущена).
- 17:00 UTC Чистый повторный запуск пройден: ok internal/jobs 1018 с, exit=0; обновил PROGRESS.md, подготовил отчет.
