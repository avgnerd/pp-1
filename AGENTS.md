<!-- context7 -->
Use Context7 MCP to fetch current documentation whenever the user asks about a library, framework, SDK, API, CLI tool, or cloud service — even well-known ones like React, Next.js, Prisma, Express, Tailwind, Django, or Spring Boot. This includes API syntax, configuration, version migration, porting code between libraries, platforms, or SQL dialects, library-specific debugging, setup instructions, and CLI tool usage. Use even when you think you know the answer — your training data may not reflect recent changes. Prefer this over web search for library docs.

Do not use for: refactoring, debugging, or reviewing code that does not depend on a library or platform API, or general programming concepts.

## Steps

1. Always start with `resolve-library-id` using the library name and what to look up in the library's documentation, unless the user provides an exact library ID in `/org/project` format
2. Pick the best match (ID format: `/org/project`) by: exact name match, description relevance, code snippet count, source reputation (High/Medium preferred), and benchmark score (higher is better). If results don't look right, try alternate names or queries (e.g., "next.js" not "nextjs", or rephrase the question). Use version-specific IDs when the user mentions a version
3. `query-docs` with the selected library ID and what to look up in the library's documentation (not single words), scoped to a single concept. If the question spans multiple distinct concepts (e.g. routing and auth and caching), make a separate `query-docs` call per concept with the same library ID, unless the question is about how the concepts interact — combined queries dilute ranking and return shallow results for each topic
4. Answer using the fetched docs
<!-- context7 -->


### Dropbox API (`dropbox-api-docs` MCP) ###

Dropbox HTTP API v2, OAuth 2.0 인증·토큰 갱신, 파일/폴더 탐색·검색·스트리밍·다운로드·조작 엔드포인트 및 에러/레이트 리밋 처리 코드를 작성·수정·디버깅할 때는 사전 학습 지식에 의존하지 말고 반드시 `dropbox-api-docs` MCP 서버의 `searchDocs` 도구(`https://docs.dropboxapi.com` 공식 문서 검색)를 먼저 호출하여 최신 스펙을 확인하세요.

- **문서 검색 규칙 (`searchDocs`)**:
  - `query`는 단일 키워드가 아닌 구체적인 엔드포인트명이나 개념 단위로 작성하세요 (예: `"oauth2 token_access_type offline refresh_token"`, `"files/list_folder cursor has_more"`, `"files/get_temporary_link"`).
  - 서로 다른 기능(예: OAuth 토큰 갱신과 썸네일 배치 조회)을 하나의 질의에 섞지 말고 개념별로 나누어 `searchDocs`를 호출하며, 필요 시 `topK`(기본 8, 최대 20)를 조정하세요.
- **핵심 구현 영역별 공식 문서 검증 항목**:
  - **다중 계정 OAuth 2.0 및 토큰 자동 갱신**: 백그라운드/장기 접근을 위해 인가 URL에 `token_access_type=offline`을 지정해 `refresh_token`을 발급받고, 단기 `access_token` 만료(`expires_in` 및 `401` 응답) 시 `https://api.dropbox.com/oauth2/token`(`grant_type=refresh_token`)으로 갱신하는 규격과 필수 스코프(`account_info.read`, `files.metadata.read`, `files.content.read`, `files.content.write`)를 확인합니다.
  - **파일 목록·검색·스트리밍·다운로드·조작 엔드포인트**:
    - 디렉토리 조회 및 커서 페이지네이션: `/2/files/list_folder`, `/2/files/list_folder/continue` (`cursor`, `has_more`)
    - 파일/폴더 검색: `/2/files/search_v2`, `/2/files/search/continue_v2`
    - 스트리밍·미리보기·다운로드: `/2/files/get_temporary_link`(비디오/오디오 스트리밍 및 직접 다운로드용 임시 링크), `/2/files/download`, `/2/files/download_zip`(폴더 ZIP 다운로드), `/2/files/get_thumbnail_batch`(이미지 썸네일 일괄 조회)
    - 파일/폴더 관리: `/2/files/move_v2`(이름 변경·이동), `/2/files/copy_v2`, `/2/files/delete_v2` 및 대량 작업용 배치 엔드포인트(`*_batch_v2`, `*_batch/check_v2`)
  - **호스트 구분·헤더 인코딩·레이트 리밋(`429`)**: RPC 호출(`api.dropboxapi.com`)과 Content 다운로드/업로드 호출(`content.dropboxapi.com` 및 `Dropbox-API-Arg` 헤더 JSON 이스케이프 규칙)을 명확히 구분하고, `429 Too Many Requests` 응답의 `retry_after`(`Retry-After` 헤더) 기반 재시도/백오프 처리 지침을 준수합니다.


### React 성능 최적화 (`vercel-react-best-practices`) ###

React 컴포넌트, 데이터 패칭, 번들 크기 최적화, 리렌더링/렌더링 및 JS 성능 개선 코드를 작성·리뷰·리팩터링할 때는 반드시 `vercel-react-best-practices` 스킬(`.agents/skills/vercel-react-best-practices/SKILL.md`, 통합 문서 `.agents/skills/vercel-react-best-practices/AGENTS.md`)과 세부 규칙 파일(`.agents/skills/vercel-react-best-practices/rules/<rule>.md`)을 참조하세요.

- **우선순위별 핵심 최적화 규칙**:
  1. **워터폴 제거 (`async-*`, CRITICAL)**: 독립적인 비동기 작업은 `Promise.all()`로 병렬화(`async-parallel`)하고, `await`는 실제 사용되는 분기로 지연(`async-defer-await`)하며, 비동기 호출 전 값싼 동기 조건을 먼저 검사(`async-cheap-condition-before-await`)하고 `Suspense` 경계를 활용(`async-suspense-boundaries`)합니다.
  2. **번들 크기 최적화 (`bundle-*`, CRITICAL)**: 배럴(barrel) 파일을 통한 import를 피하고 원본 경로에서 직접 import(`bundle-barrel-imports`)하며, 무거운 컴포넌트는 동적 import로 분리(`bundle-dynamic-imports`)하고, 조건부 기능은 활성화 시점에 로드(`bundle-conditional`)하며, 번들러가 정적 분석 가능한 경로를 유지(`bundle-analyzable-paths`)합니다.
  3. **클라이언트 패칭·리렌더링·렌더링 (`client-*`, `rerender-*`, `rendering-*`, `js-*`)**: 파생 상태는 `useEffect`가 아닌 렌더 중 계산(`rerender-derived-state-no-effect`)하고, 컴포넌트 내부에 인라인 컴포넌트를 정의하지 않으며(`rerender-no-inline-components`), 비긴급 업데이트는 `startTransition`/`useDeferredValue`를 활용하고, 조건부 렌더링 시 의도치 않은 `0` 출력 방지를 위해 `&&` 대신 삼항 연산자(`rendering-conditional-render`)를 사용합니다.


### shadcn/ui (`shadcn` 스킬 & MCP) ###

`shadcn/ui` 컴포넌트 추가·검색·수정·디버깅·스타일링·조합(채팅 UI 포함), 레지스트리 및 프리셋(`--preset`) 관리, `DESIGN.md` 기반 디자인 시스템 구축, 또는 `components.json`이 존재하는 프로젝트를 다룰 때는 반드시 `shadcn` 스킬(`.agents/skills/shadcn/SKILL.md`)과 하위 규칙 문서(`.agents/skills/shadcn/rules/`)를 먼저 참조하세요.

- **프로젝트 컨텍스트 및 공식 문서 우선 확인 (`info` & `docs` / `shadcn` MCP)**:
  - 작업 전 프로젝트 패키지 러너(`npx shadcn@latest`, `pnpm dlx shadcn@latest`, `bunx --bun shadcn@latest`)로 `shadcn@latest info --json`을 실행해 프로젝트 설정(`base`: `radix` vs `base`, `style`, `tailwindVersion`, `tailwindCssFile`, `iconLibrary`, `aliases`, `resolvedPaths`, `isRSC`)을 확인하고 절대 추측하지 마세요.
  - 컴포넌트 사용·생성·수정·디버깅 전 `npx shadcn@latest docs <component>`로 공식 문서/예제 URL을 조회하거나, `shadcn` MCP 서버 도구(`get_project_registries`, `list_items_in_registries`, `search_items_in_registries`, `view_items_in_registries`, `get_item_examples_from_registries`, `get_add_command_for_items`, `get_audit_checklist` — 상세 가이드 `.agents/skills/shadcn/mcp.md`)를 활용하세요.
  - 커뮤니티/서드파티 레지스트리 추가 시 사용자가 레지스트리를 명시하지 않았다면 임의로 추측하지 말고 먼저 질문하며, 추가된 파일의 하드코딩된 경로(`@/components/ui/...`)와 아이콘 import를 프로젝트의 `aliases` 및 `iconLibrary`에 맞게 수정하세요.
- **핵심 스타일링·구조·조합 규칙 (`.agents/skills/shadcn/rules/`)**:
  - **스타일링 (`rules/styling.md`)**: `space-x-*`/`space-y-*` 대신 `flex`(`flex-col`)와 `gap-*`을 사용하고, 가로·세로가 같은 크기는 `w-* h-*` 대신 `size-*`를 사용합니다. 원시 색상(`bg-blue-500`)이나 수동 `dark:` 오버라이드 대신 시맨틱 토큰(`bg-primary`, `text-muted-foreground`)을 사용하고, 오버레이 컴포넌트에 수동 `z-index`를 지정하지 마세요.
  - **폼 및 입력 (`rules/forms.md`)**: 폼 레이아웃은 `FieldGroup` + `Field`(`FieldLabel`, `FieldDescription`) 조합을 사용하고, 검증/비활성 상태는 `Field`의 `data-invalid`/`data-disabled`와 컨트롤의 `aria-invalid`/`disabled`로 표현합니다. 입력창 내 버튼·아이콘 결합은 `InputGroup`(`InputGroupInput`, `InputGroupAddon`)을, 2~7개 옵션 선택은 `ToggleGroup`을 사용합니다.
  - **컴포넌트 구조 및 Base/Radix 분기 (`rules/composition.md`, `rules/base-vs-radix.md`, `rules/icons.md`, `rules/chat.md`)**:
    - `SelectItem`/`DropdownMenuItem`/`CommandItem`은 반드시 각 `*Group` 안에 배치하고, `Dialog`/`Sheet`/`Drawer`는 접근성을 위해 `*Title`(숨김 시 `className="sr-only"`)을 필수로 포함하며, `Avatar`는 항상 `AvatarFallback`을 함께 구성합니다.
    - 커스텀 트리거 합성은 프로젝트 `base`에 따라 Radix는 `asChild`, Base UI는 `render` prop을 사용하며, 토스트 역시 Base UI는 `toast` 컴포넌트, Radix/Aria는 `sonner`를 사용합니다.
    - `Button` 내부 아이콘은 크기 클래스(`size-4` 등)를 넣지 않고 `data-icon="inline-start"` 또는 `"inline-end"` 속성을 지정하며, 로딩 상태는 `Spinner` + `data-icon` + `disabled`로 조합합니다.
    - 채팅/대화형 UI는 수동 스크롤 훅이나 커스텀 말풍선 `div` 대신 `MessageScroller`, `Message`, `Bubble`, `Attachment`, `Marker`, `Trace` 프리미티브를 조합합니다.
  - **컴포넌트 업데이트 및 프리셋 전환**: 기존 컴포넌트 업데이트 시 사용자 승인 없이 `--overwrite`를 사용하지 말고 `--dry-run` 및 `--diff <file>`로 변경점을 확인해 로컬 커스터마이징을 병합하세요. 프리셋 전환 시에는 사용자에게 `overwrite`, `partial`(`--only theme,font`), `merge`, `skip` 중 방식을 먼저 확인합니다.
- **세부 레퍼런스(`.agents/skills/shadcn/`)**: CLI 플래그·프리셋(`cli.md`), 테마·CSS 변수 커스터마이징(`customization.md`), 레지스트리 제작(`registry.md`), `DESIGN.md` 기반 디자인 시스템 및 쇼케이스 페이지 구축(`design-system.md`, `design-system-page.md`).


### 프론트엔드 디자인 (`frontend-design`) ###

새로운 UI를 설계·구현하거나 기존 화면의 시각적 디자인을 리디자인할 때는 `frontend-design` 스킬(`.agents/skills/frontend-design/SKILL.md`)을 참조하여 정형화된 AI 템플릿 인상을 피하고 의도적이고 차별화된 디자인을 적용하세요.

- **도메인 기반의 2단계 디자인 프로세스**:
  - **1단계 (디자인 플랜 수립)**: 코드를 작성하기 전에 제품/도메인의 성격과 대상 사용자에 맞춰 핵심 컬러 팔레트(4~6개 hex), 타이포그래피(서체 역할·위계, 본문 줄 길이 80자 미만), 레이아웃 컨셉(정렬 기준 및 ASCII 와이어프레임), 고유 디자인 원칙을 먼저 설계합니다.
  - **2단계 (AI 클리셰 검토 및 구현)**: 수립한 플랜이 흔한 AI 생성 기본값(모든 제목 위의 영문 대문자 eyebrow 라벨, 콘텐츠 성격과 무관하게 동일한 둥근 모서리·그림자를 반복하는 SaaS 카드 격자, 헤드라인의 한 단어만 색상/이탤릭 강조, 순서가 아닌 항목의 `01 / 02 / 03` 넘버링, 모든 섹션의 기계적인 페이드인 애니메이션 등)에 의존하지 않는지 검토·수정한 뒤 구현합니다.
- **절제된 시각적 포인트와 UX 라이팅**:
  - 한 화면에서 가장 핵심적인 요소 한 곳에 시각적 대담함을 집중하고 주변은 절제하며, 모바일 반응형·키보드 포커스·`prefers-reduced-motion`·색상 대비 등 기본 접근성 품질을 유지합니다.
  - UI 카피는 시스템 내부 구조가 아닌 사용자 관점의 명확한 능동태 표현(예: "Submit" 대신 "Save changes")을 사용하고, 빈 화면(Empty state)과 에러 메시지는 모호한 사과 대신 원인과 다음 행동을 구체적으로 안내합니다.


### Playwright ###

브라우저 자동화, 웹페이지 검증 및 Playwright 테스트 작업을 수행할 때는 반드시 `playwright-cli` 스킬(`.agents/skills/playwright-cli/SKILL.md`, `https://playwright.dev/agent-cli/skills`)과 작업별 하위 가이드(`.agents/skills/playwright-cli/references/`)를 먼저 참조하세요.

- **브라우저 및 실행 규칙 (`--browser=msedge` 필수)**:
  - 브라우저 세션 시작 시 반드시 Microsoft Edge(`--browser=msedge` / `--browser msedge`) 옵션을 사용합니다 (`playwright-cli open --browser=msedge [url]`).
  - 전역 `playwright-cli`가 없으면 로컬 `npx playwright cli`를 사용하며, Windows(PowerShell/`cmd.exe`)에서 쿼리 파라미터(`&`)가 포함된 URL 전달 시 `playwright-cli --% goto "https://..."` 또는 `^&`로 이스케이프합니다.
  - 작업 완료 후에는 반드시 `playwright-cli close`(또는 `close-all`)로 브라우저 세션을 정리합니다.
- **스냅샷·Ref 기반 상호작용 및 로케이터**:
  - 페이지 이동·조작 후 `playwright-cli snapshot`으로 요소 참조(`e1`, `e2` 등 ref)를 확인해 상호작용(`click`, `fill`, `press` 등)하고, 큰 스냅샷은 `playwright-cli find` 또는 `--depth`로 범위를 좁혀 조회합니다. 출력 파이핑이나 검증 값 추출 시 `--raw` 또는 `--json` 플래그를 사용합니다.
  - 각 `playwright-cli` 액션이 출력하는 Playwright TypeScript 코드(역할 기반 시맨틱 로케이터 `getByRole`, `getByLabel`, `getByTestId` 우선)와 `playwright-cli --raw generate-locator <ref>`를 사용해 테스트 코드를 작성합니다.
- **Playwright 테스트 실행·생성·디버깅 (Plan → Generate → Heal)**:
  - 테스트 실행 시 HTML 리포트가 자동으로 열려 프로세스가 대기하지 않도록 `PLAYWRIGHT_HTML_OPEN=never npx playwright test`를 사용합니다.
  - 테스트 생성(`references/test-generation.md`) 및 실패 테스트 디버깅/복구(`references/playwright-tests.md`) 시 백그라운드로 `PLAYWRIGHT_HTML_OPEN=never npx playwright test ... --debug=cli`를 실행하고, 출력된 세션(`tw-XXXX`)에 `playwright-cli attach tw-XXXX`로 연결해 실시간 상태를 검사·수정한 뒤 백그라운드 프로세스를 종료합니다 (`networkidle` 대기나 임의의 sleep 추가 금지).
- **작업별 하위 레퍼런스(`.agents/skills/playwright-cli/references/`)**:
  - 테스트 실행 및 디버깅(`playwright-tests.md`), 테스트 계획·생성·복구(`test-generation.md`), 네트워크 모킹(`request-mocking.md`), 임의 스크립트 실행(`running-code.md`), 세션 관리(`session-management.md`), 스토리지·인증 상태(`storage-state.md`), 트레이싱(`tracing.md`), 비디오 녹화(`video-recording.md`), 요소 속성 검사(`element-attributes.md`), PR 아티팩트 첨부(`pr-attachments.md`).


### Video.js ###

비디오/오디오 플레이어(`@videojs/react`, `@videojs/html` v10) 및 `<video>`/`<audio>` 관련 작업을 수행할 때는 반드시 `videojs` 스킬(`.agents/skills/videojs/SKILL.md`, `https://github.com/videojs/skills`)과 하위 가이드(`.agents/skills/videojs/references/installation.md`, `hosting.md`, `feedback.md`)를 먼저 참조하세요.

- **문서 우선 참조 (사전 학습 지식 의존 금지)**: Video.js 10은 단순 버전 업이 아닌 전면 재설계(rebuild)이므로 학습 데이터의 Video.js 8 지식을 사용하지 마세요.
  1. 패키지 내장 문서 우선: `node_modules/@videojs/<react|html>/docs/llms.txt`에서 필요한 `.md` 문서만 찾아 읽으세요.
  2. 패키지 미설치 시 라이브 문서: `https://videojs.org/docs/framework/<react|html>/llms.txt` (문서 URL 끝에 `.md`를 붙여 참조하며 `llms-full.txt` 전체 로드는 지양).
  3. 둘 다 접근 불가 시 `node_modules/@videojs/*/dist`의 타입 정의를 확인하고 임의로 API를 추측하지 마세요.
- **설치 및 설정 초기화**: 기억에 의존해 설치 코드를 작성하지 마세요. 설치·구성 변경 전 `.agents/skills/videojs/references/installation.md`를 읽고 앱 디렉토리에서 `npx @videojs/cli agents init`을 실행해 프레임워크·버전에 맞는 설치 옵션과 정확한 import 경로를 확인하세요.
- **Video.js 10 조합 모델 (React)**:
  - **Player**(`<VideoPlayer>`) + **Skin / UI**(`<VideoSkin>` 또는 `<Container>`와 UI 컴포넌트) + **Media**(`<Video>`, `<Audio>`, `<HlsVideo>` 등 및 필요한 playback adapter) + 선택적 **Extension** 구조로 조합합니다.
  - 스킨 스타일시트는 한 번만 import하며, 기능·버튼 추가 및 상태 기반 커스텀 컨트롤은 `usePlayer`(HTML은 `PlayerController`)와 커스텀 UI/스킨 가이드를 따르세요.
- **Video.js 8 레거시 유입 금지**: `video.js` 패키지(v8), `videojs()` 호출, 옵션 객체, `registerPlugin`, `class="video-js"`, `data-setup`, 그리고 스킨/커스텀 UI가 컨트롤을 제공할 때 미디어 요소에 네이티브 `controls` 속성을 넣는 방식을 절대 사용하지 마세요.


### lightGallery ###

이미지/비디오 라이트박스 및 갤러리(`lightgallery`, `@lightgallery/react`, `@lightgallery/vue`, `@lightgallery/angular`) 관련 작업을 수행할 때는 반드시 `lightgallery` 스킬(`.agents/skills/lightgallery/SKILL.md`)을 참조하세요.

- **핵심 적용 규칙**:
  - **CSS 명시적 import 필수**: 프레임워크 패키지는 스타일시트를 자동 포함하지 않으므로 `lightgallery/css/lightgallery.css`와 사용하는 각 플러그인의 CSS(`lg-zoom.css`, `lg-thumbnail.css` 등 또는 `lightgallery-bundle.css`)를 반드시 import합니다.
  - **플러그인 Opt-in 및 기본 레시피**: 별도 지시가 없으면 이미지 갤러리에 `zoom`, `thumbnail` 플러그인을 기본 적용하고, 클릭한 썸네일에서 확대·복귀하는 애니메이션(zoom-from-origin)이 동작하도록 모든 이미지 아이템에 원본 크기인 `lgSize: 'WIDTH-HEIGHT'`(`data-lg-size`)를 지정합니다.
  - **React 주의사항**: `@lightgallery/react`에서 `open` prop의 제어(controlled)/비제어(uncontrolled) 모드를 컴포넌트 생명주기 동안 혼용하지 마세요.


# Project Instructions for AI Code Assistant with gopls-mcp

## What gopls-mcp does (and what it doesn't)

gopls-mcp is **strictly a semantic Go layer** built on top of gopls's type
checker. It exposes six tools — that's the whole surface area:

| Task | Tool |
|------|------|
| Jump to definition | `go_definition` |
| Find interface implementations | `go_implementation` |
| Find symbol references | `go_symbol_references` |
| Trace call relationships | `go_get_call_hierarchy` |
| Analyze package dependencies | `go_get_dependency_graph` |
| Preview a symbol rename | `go_dryrun_rename_symbol` |

These cannot be replaced by `Grep` + `Read`, because Go's type system makes
interfaces, scopes, and identity invisible to text search.

For *anything else* — listing packages, reading `go.mod`, searching symbol
names, running `go build`, scanning files — use your native tools (`Glob`,
`Grep`, `Read`, `Bash`). They are already optimal for those jobs, so we
deliberately don't ship duplicates.

<!-- Marker: AUTO-GEN-START -->
### Semantic tools (Exclusive - no grep/Read fallback)
| Task | Tool |
|------|------|
| Find interface implementations | go_implementation |
| Trace call relationships | go_get_call_hierarchy |
| Find symbol references | go_symbol_references |
| Jump to definition | go_definition |
| Analyze dependencies | go_get_dependency_graph |
| Preview renaming | go_dryrun_rename_symbol |

<!-- Marker: AUTO-GEN-END -->

## Routing rules

1. **Semantic relationship?** Use the table above. Never fall back to grep —
   you'd miss interface satisfaction, shadowing, cross-package identity.
2. **Text search / file listing / build / mod?** Use `Grep` / `Glob` /
   `Read` / `Bash`. There is no gopls-mcp tool for these.
3. **Output**: present `file:line` locations, signatures, and docs cleanly.
   Never dump raw JSON.

## Tool-specific constraints

* **`go_implementation`**: Interfaces and types only. **Not** for functions.
* **General locator parameters**:
  * `symbol_name`: bare identifier, no package prefix
    (`"Start"`, not `"Server.Start"`).
  * `context_file`: an absolute path to the file you are currently reading
    or where the symbol appears. The resolver uses this for scope and
    import disambiguation.

## Error handling

If a semantic tool returns nothing, do **not** silently fall back to grep:

1. Verify the symbol name is spelled exactly as in source (case-sensitive).
2. Confirm `context_file` is correct and inside a Go module.
3. For `go_implementation`, check that `parent_scope` points at the
   interface, not the concrete type.

Only after these checks fail should you fall back to manual `Read` of the
relevant file — and only as diagnosis, not as a substitute capability.
