# Dropbox VFS

여러 Dropbox 계정을 가상 마운트 경로(예: `/path/to`)에 연결해 하나의 웹 탐색기로 보는 자체 호스팅 앱입니다.
통합 검색, 미리보기(이미지·비디오·오디오·텍스트·PDF·Office·압축 목록), Video.js v10 플레이어(필요하면 FFmpeg HLS 변환, VR 180/360),
다중 파일 ZIP 다운로드, 파일 조작, 관리자 페이지를 제공합니다.

- 백엔드: Go (Gin), SQLite (`modernc.org/sqlite`, CGO 없음), 프론트엔드를 바이너리에 내장
- 프론트엔드: React, Vite, TypeScript, TanStack Router/Query/Virtual, Tailwind CSS 4, shadcn/ui (Base UI), Zustand, react-hook-form + zod, lightGallery 3, Video.js 10, three.js

첫 로그인은 **admin / 1234** 입니다. 로그인한 뒤 관리 > 프로필에서 바로 바꾸세요.

## Windows에서 실행

필요한 것: Go 1.27 이상, Node.js 20 이상.

```powershell
.\scripts\run-local.ps1            # FFmpeg 자동 준비, 빌드, 실행, 브라우저 열기 (http://localhost:8080)
.\scripts\run-local.ps1 -Port 9000 # 다른 포트
.\scripts\run-local.ps1 -SkipBuild # 이미 만든 bin\dvfs.exe 실행
```

- FFmpeg가 `tools\ffmpeg`에도 PATH에도 없으면 BtbN의 win64 GPL 빌드를 받아 SHA-256을 확인한 뒤 `tools\ffmpeg`에 풉니다. `-NoFFmpeg`를 주면 건너뜁니다(원본 재생만 가능).
- 데이터는 `data\`에 저장됩니다: `config.json`, `app.db`, `secret.key`(토큰 암호화 키), `thumbs\`, `hls\`.

개발 모드(Vite 핫 리로드, :5173):

```powershell
.\scripts\dev.ps1        # 실제 Dropbox
.\scripts\dev.ps1 -Fake  # 메모리 속 가짜 Dropbox(:9900). App key app1, app2, app3에 샘플 파일이 있습니다
```

## Oracle Cloud (Ampere A1) + Portainer

1. Portainer > Stacks > Add stack > **Repository**를 고르고 이 저장소 URL과 Compose path `deploy/docker-compose.yml`을 넣습니다.
2. Deploy하면 서버(arm64)에서 직접 이미지를 빌드합니다. 이미지에는 libx264가 들어 있는 Alpine FFmpeg가 포함됩니다.
3. 보안 목록(Security List)과 서버 방화벽에서 TCP 8080을 엽니다.
4. `http://<서버 IP>:8080`으로 접속합니다. 데이터는 `dvfs-data` 볼륨에 남습니다.

나중에 도메인이나 리버스 프록시(HTTPS)를 붙이면 스택 환경 변수 `DVFS_PUBLIC_URL=https://files.example.com`을 넣으세요.
그러면 링크 복사가 그 주소를 쓰고, 세션 쿠키에 Secure가 붙고, public_url 인증 방식을 쓸 수 있습니다.
프록시를 거친다면 `data/config.json`의 `trusted_proxies`에 프록시 IP를 넣으세요.

## Dropbox 앱 준비 (저장소마다)

1. https://www.dropbox.com/developers/apps 에서 **Create app** > Scoped access > **Full Dropbox**.
2. Permissions 탭에서 `account_info.read`, `files.metadata.read`, `files.content.read`, `files.content.write`를 켜고 Submit.
3. Settings 탭 > OAuth 2 > Redirect URIs에 `http://localhost:8080/api/oauth/callback`을 추가합니다(포트를 바꿨다면 그 포트).
4. 사이트의 관리 > 저장소 > 저장소 추가에서 App key, App secret, 마운트 경로를 넣고 '저장하고 인증하기'를 누릅니다.

인증 방식:

| 방식 | 언제 | 동작 |
| --- | --- | --- |
| localhost (기본) | 이 PC에서 열었거나 `ssh -L 8080:localhost:8080 <서버>`로 접속 | 승인 후 자동으로 연결됩니다 |
| localhost + URL 붙여넣기 | 서버 IP로 접속 | 승인 뒤 브라우저가 내 PC의 localhost로 가서 연결 오류가 납니다. 그 주소창 URL 전체를 저장소 화면에 붙여넣으면 연결됩니다 |
| public_url | `DVFS_PUBLIC_URL`이 HTTPS일 때 | `<public_url>/api/oauth/callback`을 App Console에 등록해야 합니다 |
| 코드 붙여넣기 | Redirect URI를 등록하고 싶지 않을 때 | Dropbox 화면에 나온 코드를 붙여넣습니다 |

모든 방식이 `token_access_type=offline`으로 refresh token을 받으므로 다시 로그인할 필요가 없습니다. 토큰과 App secret은 `secret.key`로 AES-GCM 암호화해 저장합니다.
`secret.key`를 잃어버리면 모든 저장소를 다시 인증해야 하니 `data` 폴더를 백업하세요.

## 주요 동작

- **마운트 경로**: 같은 경로나 서로 상위·하위인 경로는 쓸 수 없습니다(`/path/to`가 있으면 `/path`, `/path/to/x` 불가, `/path/to2` 가능). `/path`는 가상 폴더로 보입니다.
- **폴더 크기**: 목록에서는 계산하지 않고 `—`로 표시합니다. 폴더 이름을 우클릭해 '폴더 크기'를 고를 때만 계산하고, 결과는 저장소 캐시 만료 시간 동안 기억합니다. 하위 항목을 바꾸면 지워집니다.
- **파일 전송 방식**(관리 > 설정 > 시스템): 직접 연결은 Dropbox 임시 링크로 보내 서버 트래픽이 없고, 서버 경유는 모든 바이트를 서버가 Range 요청으로 중계합니다. 변환 재생과 ZIP은 항상 서버를 거칩니다.
- **재생**: 브라우저가 재생할 수 있으면 원본, 컨테이너만 안 되면(MKV 등) HLS로 다시 담기, 코덱이 안 되면(WMV 등) 또는 1080p/720p를 고르면 H.264로 변환합니다. 동시 변환 수 기본값은 2입니다. 일시정지한 지 60초가 지나면 변환을 멈추고 자리를 내주며, 다시 재생할 때 자리를 다시 잡습니다(그 사이 자리가 다 차면 재생이 이어지지 않습니다). 탭마다 변환 세션이 따로 있어 한 탭을 닫아도 다른 탭 재생에는 영향이 없습니다.
- **끊김 자동 전환**: 원본 재생 중 4초 넘게 프레임이 10% 이상 버려지면(예: B프레임 순서 정보가 잘못 기록된 H.264 파일) 보던 위치에서 변환 재생으로 바꿉니다. 화질 메뉴의 '변환 재생'으로 직접 바꿀 수도 있으며, 이 선택은 그 파일에만 적용됩니다.
- **VR**: 파일 이름(`180`, `360`, `_LR`, `_TB`, `SBS`)과 ffprobe 메타데이터로 감지하고, 설정 메뉴의 VR에서 바꿀 수 있습니다. 드래그·방향키로 둘러보고 휠로 확대·축소합니다.
- **링크 복사**: `public_url`(없으면 현재 주소) + 가상 경로입니다. 열면 로그인 후 그 파일로 바로 갑니다.
- **팟플레이어**: 직접 연결 모드에서는 Dropbox 임시 링크, 서버 경유 모드에서는 12시간 동안 유효한 서명 URL을 넘깁니다.

## 테스트

```powershell
cd backend; go test ./...                     # ffmpeg가 PATH에 있으면 HLS 테스트도 실행
cd frontend; $env:PLAYWRIGHT_HTML_OPEN="never"; npx playwright test   # Edge, 가짜 Dropbox 사용
```

Playwright 테스트 전에 `scripts\run-local.ps1`(또는 빌드 후 `frontend\dist`를 `backend\internal\web\dist`에 복사)으로 프론트엔드를 내장해 두어야 합니다.

## 라이선스 참고

lightGallery는 GPLv3입니다. 상업적으로 쓰려면 lightGallery 상용 라이선스가 필요합니다.
