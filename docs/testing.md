# 테스트 실행

이 문서는 저장소의 Go 테스트와 Playwright E2E 테스트를 로컬에서 실행하는 방법을 설명합니다. E2E는 실제 Dropbox 계정을 쓰지 않고 저장소에 포함된 fake Dropbox 서버와 임시 데이터 디렉터리로 실행합니다.

## 준비

- Go 1.27.3 이상. `backend/go.mod`의 `go 1.27.3`을 따릅니다.
- Node.js 20.19.x, 22.13 이상, 또는 24 이상. 설치된 `vite@8.3.5`는 `^20.19.0 || >=22.12.0`을, `eslint@10.14.0`은 `^20.19.0 || ^22.13.0 || >=24`를 요구합니다.
- Microsoft Edge. 세 Playwright 설정 모두 `channel: "msedge"`를 사용합니다.
- libx264를 포함한 FFmpeg. streaming 설정이 `ffmpeg`로 H.264 영상과 HLS 세그먼트를 만듭니다. `ffmpeg`가 PATH에 없으면 `FFMPEG_PATH`를 지정합니다. `scripts/run-local.ps1`은 `tools\ffmpeg\bin\ffmpeg.exe`에 내려받습니다.

`frontend` 디렉터리에서 로컬 FFmpeg를 지정합니다.

```powershell
$env:FFMPEG_PATH = (Resolve-Path ..\tools\ffmpeg\bin\ffmpeg.exe).Path
```

## Go 테스트

```powershell
cd C:\@AI\dropbox-vfs-manager\backend
go build ./...
go test ./... -count=1
```

서버 통합 테스트는 실제 `httptest` 서버, 세션과 CSRF 검사, SQLite, 메모리 fake Dropbox를 사용합니다. 테스트마다 `t.TempDir()` 기반 데이터 디렉터리를 만들고 정리합니다.

## 프론트엔드 빌드와 Go 임베드

Go 서버는 `backend/internal/web/dist`를 `//go:embed all:dist`로 포함합니다. E2E smoke 설정은 이 디렉터리를 서빙하므로 먼저 프론트엔드를 빌드해 복사합니다.

```powershell
cd C:\@AI\dropbox-vfs-manager\frontend
npm ci
npm run build

$embed = (Resolve-Path ..\backend\internal\web\dist).Path
Get-ChildItem $embed -Force | Where-Object { $_.Name -ne '.keep' } | Remove-Item -Recurse -Force
Copy-Item -Recurse -Force .\dist\* $embed
```

`npm ci`는 처음 한 번만 실행합니다. `package-lock.json`이 바뀌면 다시 실행합니다. 복사 명령은 `.keep`을 남겨 임베드 디렉터리가 비지 않게 합니다.

## E2E 전체 실행

```powershell
cd C:\@AI\dropbox-vfs-manager\frontend
$env:PLAYWRIGHT_HTML_OPEN = "never"
npm run e2e
```

`npm run e2e`는 `frontend/scripts/run-e2e.mjs`를 실행합니다. 세 설정을 smoke, streaming, browse 순서로 실행하고 첫 실패에서 멈춥니다. 종료 코드는 0이면 모두 통과, 아니면 첫 실패한 Playwright 실행의 종료 코드입니다. 실패 뒤의 설정은 실행하지 않습니다.

현재 스위트는 62개 테스트입니다.

| 설정 | 파일 | 테스트 | 서버 |
| --- | --- | --- | --- |
| smoke | `playwright.config.ts` | `smoke.spec.ts` 4개 | 실제 Go 서버(`:8099`)와 fake Dropbox(`127.0.0.1:9911`) |
| streaming | `playwright.streaming.config.ts` | `streaming.spec.ts` 20개 | `e2e/fixtures/streaming-server.cjs`(`127.0.0.1:18764`) |
| browse | `playwright.browse.config.ts` | `browse.spec.ts` 14개, `search.spec.ts` 24개 | `e2e/fixtures`의 Vite fixture 서버 |

browse 설정은 Go 서버와 fake Dropbox를 띄우지 않습니다. 두 fixture 서버가 API 응답을 대신합니다.

## 산출물과 종료 코드

`npm run e2e`는 시작할 때 두 임시 경로를 출력합니다.

- `$env:TEMP\dvfs-e2e-run-XXXX`: `smoke.json`, `streaming.json`, `browse.json`과 `html-smoke`, `html-streaming`, `html-browse` 디렉터리.
- `$env:TEMP\dvfs-e2e-data-XXXX`: smoke 실행이 쓰는 새 서버 데이터 디렉터리.

runner는 이 경로를 지우지 않습니다. 실패를 조사한 뒤 직접 정리합니다. 성공 실행도 HTML과 JSON을 남깁니다.

실패한 테스트의 trace는 설정에 따라 다음에 남습니다.

- smoke와 browse: `frontend/test-results`.
- streaming: `$env:TEMP\dvfs-streaming-XXXX\results`. 이 디렉터리는 fixture의 영상과 HLS 파일도 담습니다.

`PLAYWRIGHT_HTML_OPEN=never`는 HTML 리포트가 자동으로 열리지 않게 합니다. runner는 이 값을 직접 설정하므로 `npm run e2e`에는 따로 지정하지 않아도 됩니다.

## 설정을 직접 실행할 때

기본 설정만 직접 실행하면 smoke 테스트만 수집합니다. 이 명령은 `DVFS_E2E_DATA`를 지정하지 않는 한 기존 `data-playwright` 디렉터리를 사용합니다.

```powershell
cd C:\@AI\dropbox-vfs-manager\frontend
$env:PLAYWRIGHT_HTML_OPEN = "never"
npx playwright test
```

기존 `data-playwright`를 건드리지 않으려면 새 데이터 디렉터리를 지정합니다.

```powershell
$env:DVFS_E2E_DATA = Join-Path $env:TEMP ("dvfs-smoke-" + [guid]::NewGuid().ToString("N"))
npx playwright test
```

streaming과 browse 설정은 각각 따로 실행할 수 있습니다.

```powershell
npx playwright test --config=playwright.streaming.config.ts
npx playwright test --config=playwright.browse.config.ts
```

`npm run e2e`와 직접 실행의 차이는 데이터와 리포트입니다. runner는 항상 새 임시 데이터 디렉터리를 쓰고, 세 설정을 순서대로 실행하며, HTML과 JSON 리포트를 임시 디렉터리에 모읍니다. 직접 실행은 해당 설정 하나만 실행하고, `data-playwright`를 쓰거나 HTML 리포트를 열 수 있습니다.
