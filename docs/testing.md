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

`DVFS_E2E_ARTIFACTS`를 지정하면 runner는 그 디렉터리에 리포트를 쓰고, 실패한 테스트의 trace는 `results-smoke`, `results-streaming`, `results-browse`에 남깁니다.

`DVFS_E2E_ARTIFACTS`를 지정하지 않으면 실패한 테스트의 trace는 설정에 따라 다음에 남습니다.

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

## CI

`.github/workflows/ci.yml`은 Go 빌드와 전체 테스트, 프론트 타입 검사와 빌드, Edge E2E 전체를 GitHub-hosted Windows runner에서 실행합니다. E2E 로그와 리포트는 성공과 실패 모두 아티팩트로 남깁니다.

### 트리거와 권한

- `pull_request`: PR이 열리거나, 갱신되거나, 다시 열릴 때 실행합니다.
- `push`: `main`에서만 실행합니다. PR 브랜치는 `pull_request`가 검사하므로 push로 다시 돌리지 않습니다. PR 없는 브랜치는 `workflow_dispatch`로 수동 실행합니다.
- `workflow_dispatch`: 수동으로 실행합니다.
- 권한은 `contents: read`만 씁니다. secret, 배포, 쓰기 권한은 쓰지 않습니다.

### 실행 환경

| 항목 | 값 |
| --- | --- |
| runner | `windows-2025` |
| Go | `backend/go.mod`의 `go 1.27.3` |
| Node.js | `24` |
| 브라우저 | 이미지에 포함된 Microsoft Edge |
| FFmpeg | `scripts/ci/prepare-ffmpeg.ps1`이 libx264를 포함한 빌드를 준비 |

`windows-2025` 이미지에는 Edge와 Node.js 24가 있고 FFmpeg는 없습니다. Go 1.27.3과 FFmpeg는 CI가 직접 준비합니다. Windows runner 라벨은 [GitHub Docs의 runner 표](https://docs.github.com/en/actions/reference/runners/github-hosted-runners)와 [Windows2025 이미지 목록](https://github.com/actions/runner-images/blob/main/images/windows/Windows2025-Readme.md)을 따릅니다.

workflow는 `actions/checkout@v7`, `actions/setup-go@v7`, `actions/setup-node@v7`, `actions/upload-artifact@v7`를 씁니다.

`prepare-ffmpeg.ps1`은 [BtbN FFmpeg-Builds](https://github.com/BtbN/FFmpeg-Builds)의 `win64-gpl` 빌드를 내려받습니다. `lgpl` 변형에는 libx264가 없고 `gpl` 변형에는 있습니다. 스크립트는 같은 릴리스의 `checksums.sha256`으로 SHA-256을 확인하고, 설치한 바이너리가 `libx264` 인코더를 보고하는지 검사합니다. 내려받는 태그는 `latest`라서 FFmpeg 버전은 실행마다 달라질 수 있습니다. 체크섬은 손상된 다운로드만 걸러 냅니다.

### 실행 단계

1. `scripts/ci/prepare-ffmpeg.ps1`
2. `backend`에서 `go build ./...`, `go test ./... -count=1`
3. `frontend`에서 `npm ci`, `npm run typecheck`, `npm run build`
4. `scripts/ci/embed-frontend.ps1`
5. `scripts/ci/run-e2e.ps1`이 `npm run e2e`를 실행

E2E는 62개 테스트를 세 설정에서 `msedge`로 실행합니다. fake Dropbox와 실행마다 새로 만드는 임시 데이터 디렉터리를 쓰고, `data-playwright`와 실제 Dropbox 계정은 쓰지 않습니다. workflow는 `continue-on-error`와 재시도를 쓰지 않습니다. Go 미디어 테스트는 FFmpeg가 없으면 스스로 skip하므로 CI는 FFmpeg 준비를 먼저 검사합니다.

helper는 호출자 경로 인자를 받지 않습니다. `embed-frontend.ps1`은 스크립트가 속한 저장소의 `frontend/dist`와 `backend/internal/web/dist`만 씁니다. `prepare-ffmpeg.ps1`은 OS 임시 디렉터리의 `dvfs-ffmpeg`만 씁니다. 두 스크립트는 reparse point를 따라가지 않습니다. `prepare-ffmpeg.ps1`은 자기가 만든 표시 파일(`.dvfs-ci-ffmpeg`)이 없는 디렉터리를 수정하지 않고 실패합니다.

### 아티팩트

E2E 단계는 workflow 식 `${{ runner.temp }}\dvfs-e2e-artifacts`를 `DVFS_E2E_ARTIFACTS`로 지정합니다. `run-e2e.ps1`은 이 디렉터리에 다음을 씁니다.

- `e2e.log`: E2E 표준 출력과 표준 에러.
- `smoke.json`, `streaming.json`, `browse.json`: 설정별 JSON 리포트.
- `html-smoke`, `html-streaming`, `html-browse`: 설정별 HTML 리포트.
- `results-smoke`, `results-streaming`, `results-browse`: 실패한 테스트의 `trace.zip`과 `error-context.md`.

`Upload E2E evidence` 단계가 `if: always()`로 이 디렉터리를 `e2e-evidence` 아티팩트에 올립니다. E2E 단계가 실패해도 업로드는 실행됩니다. 보관 기간은 14일입니다. `actions/upload-artifact`는 기본으로 숨김 파일을 제외하므로 `include-hidden-files`를 켜지 않습니다.

서버 임시 데이터 디렉터리(`dvfs-e2e-data-*`)는 아티팩트에 넣지 않습니다. 이 디렉터리에는 `app.db`와 `secret.key`가 생깁니다. Go와 npm 단계의 로그는 Actions 실행 로그에 남고, 아티팩트에는 넣지 않습니다.

`run-e2e.ps1`은 suite를 시작하기 전에 아티팩트 디렉터리와 `e2e.log`를 만들 수 있는지 확인합니다. 기록할 수 없으면 suite를 실행하지 않고 0이 아닌 코드로 끝납니다.

### 로컬에서 CI 단계 재현

```powershell
./scripts/ci/prepare-ffmpeg.ps1

cd backend
go build ./...
go test ./... -count=1

cd ..\frontend
npm ci
npm run typecheck
npm run build

cd ..
./scripts/ci/embed-frontend.ps1
$env:DVFS_E2E_ARTIFACTS = Join-Path $env:TEMP ("dvfs-e2e-" + [guid]::NewGuid().ToString("N"))
./scripts/ci/run-e2e.ps1
```

`run-e2e.ps1`은 `npm run e2e`의 종료 코드를 그대로 돌려줍니다. `DVFS_E2E_ARTIFACTS`를 지정하지 않으면 `%TEMP%\dvfs-e2e-<guid>`를 씁니다. `npm run e2e`의 기본 동작은 바뀌지 않습니다.

`%TEMP%\dvfs-ffmpeg`가 남아 있는데 표시 파일 `.dvfs-ci-ffmpeg`가 없으면 `prepare-ffmpeg.ps1`이 실패합니다. 그 디렉터리가 이 스크립트의 설치가 아니면 직접 확인해 지운 뒤 다시 실행합니다.

workflow YAML은 actionlint로 검사합니다.

```powershell
actionlint .github/workflows/ci.yml
```

### 원격 미검증

저장소에 Git remote가 없어 이 workflow는 GitHub에서 실행된 적이 없습니다. 로컬에서 같은 명령과 YAML lint만 확인했습니다. GitHub-hosted runner의 실제 실행, 아티팩트 업로드, Edge와 FFmpeg 설치는 원격 실행 뒤에 확인해야 합니다.
