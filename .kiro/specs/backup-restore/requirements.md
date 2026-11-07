# Requirements Document

## Introduction

관리자가 Dropbox VFS 인스턴스의 설정과 상태를 백업 파일 하나로 내려받고, 나중에 같은 인스턴스나 다른 인스턴스에 그 파일을 올려 복원할 수 있게 합니다. 백업을 만들 때 관리자는 백업 파일을 비밀번호로 암호화할지 고를 수 있습니다.

이 문서는 다음 가정을 따릅니다. 검토하면서 바꿀 수 있습니다.

- 백업 대상은 앱 자체의 상태입니다: 관리자 계정, 설정(settings 테이블), 저장소 목록(App key, App secret, refresh token, 마운트 경로 등). Dropbox에 있는 파일 내용은 백업하지 않습니다.
- 캐시와 일시적 데이터(thumbs, hls, media_probe, sessions, oauth_states)는 백업하지 않습니다.
- `config.json`과 `secret.key`는 백업하지 않습니다. 이 두 파일은 서버 시작 시 읽는 배포 설정이고, 다른 서버로 옮기면 listen 주소나 public_url이 맞지 않을 수 있기 때문입니다. 대신 관리자가 내려받는 백업 파일에는 민감 정보를 인스턴스 마스터 키와 무관한 형태(평문 또는 Backup_Password 기반 암호화)로 담아, `secret.key`가 다른 인스턴스에서도 복원할 수 있게 합니다.
- 복원은 병합이 아니라 전체 교체입니다. 복원하면 기존 관리자 계정, 설정, 저장소 목록이 백업 내용으로 바뀝니다.
- 복원은 미리보기와 승인 두 단계로 나뉩니다. 미리보기 단계에서 발급하는 미리보기 식별자는 발급 후 10분이 지나면 만료되고, 복원 적용에 한 번만 쓸 수 있습니다.
- 복원 직전 현재 상태를 서버의 `data/backups/`에 자동 보관해, 잘못 복원했을 때 되돌릴 수 있게 합니다. 이 자동 보관 파일은 Instance_Master_Key에서 유도한 하위 키로 암호화하므로 같은 인스턴스(같은 Instance_Master_Key)에서만 복원할 수 있고, 최대 5개까지 보관합니다.
- 복원이 끝나면 진행 중인 HLS 트랜스코드 세션을 모두 중지합니다. thumbs, hls, media_probe 캐시 자체는 지우지 않습니다.
- 예약(자동 주기) 백업과 외부 저장소(Dropbox 등)로 백업 업로드는 이번 범위에 넣지 않습니다.

## Glossary

- **Backup_Service**: 현재 인스턴스 상태를 읽어 Backup_File을 만드는 백엔드 구성 요소
- **Restore_Service**: Backup_File을 검증하고 인스턴스 상태를 그 내용으로 교체하는 백엔드 구성 요소
- **Backup_Serializer**: Backup_Payload를 Backup_File 바이트로 바꾸는 구성 요소(암호화 포함)
- **Backup_Parser**: Backup_File 바이트를 읽어 Backup_Payload로 되돌리는 구성 요소(복호화 포함)
- **Admin_API**: `/api/admin` 아래의 관리자 전용 HTTP API. 로그인 세션과 CSRF 검사가 필요합니다
- **Admin_UI**: 프론트엔드 관리 페이지(관리 > 백업 화면 포함)
- **Admin**: 로그인한 관리자
- **Backup_File**: 다운로드하거나 업로드하는 백업 파일. Envelope와 Backup_Payload(평문 또는 암호문)로 이루어집니다
- **Envelope**: Backup_File의 바깥 구조. 형식 식별자, 형식 버전, 생성 시각, 앱 버전, 암호화 여부, Encryption_Mode, 암호화 매개변수를 담습니다
- **Backup_Payload**: 백업 대상 데이터(관리자 계정, 설정, 저장소 목록)를 담은 구조
- **Format_Version**: Backup_File 형식의 버전 번호. DB의 schema_version과는 별개입니다
- **Instance_Master_Key**: 인스턴스의 32바이트 마스터 키(`secret.key`, config `secret_key`, 또는 환경 변수 `DVFS_SECRET_KEY`)
- **Sensitive_Value**: App secret, refresh token, 관리자 password_hash
- **Backup_Password**: 관리자가 백업 암호화에 쓰려고 입력하는 비밀번호
- **Encryption_Mode**: 암호화 여부가 true인 Envelope에 기록하는 암호화 방식. `password`(Backup_Password에서 유도한 키 사용) 또는 `instance-key`(Instance_Master_Key에서 유도한 하위 키 사용) 중 하나입니다
- **Encrypted_Backup**: Backup_Password로 Backup_Payload를 암호화한 Backup_File(Encryption_Mode `password`)
- **Instance_Key_Backup**: Instance_Master_Key에서 유도한 하위 키로 Backup_Payload를 암호화한 Backup_File(Encryption_Mode `instance-key`). Envelope에 salt와 키 유도 매개변수 대신 nonce와 키 유도 레이블을 기록합니다
- **Plain_Backup**: Backup_Payload를 암호화하지 않은 Backup_File
- **Pre_Restore_Snapshot**: 복원 직전 Restore_Service가 현재 상태로 만들어 서버의 `data/backups/`에 보관하는 Instance_Key_Backup
- **Restore_Preview**: 복원을 적용하기 전에 Admin에게 보여 주는 백업 요약(Format_Version, Encrypted_Backup 여부, 생성 시각, 관리자 username, 설정 항목 수, 저장소별 이름과 마운트 경로)과 미리보기 식별자
- **미리보기 식별자(Preview_Id)**: Restore_Service가 Restore_Preview 응답에 담아 발급하는 값. 미리보기 단계에서 검증한 Backup_Payload를 가리키며, 발급한 로그인 세션에서만, 발급 후 10분 안에, 복원 적용에 한 번만 쓸 수 있습니다
- **HLS 트랜스코드 세션**: 동영상 재생을 위해 서버가 실행 중인 HLS 변환 작업

## Requirements

### Requirement 1: 백업 파일 만들기

**User Story:** 관리자로서, 현재 설정과 저장소 연결 정보를 파일로 내려받고 싶습니다. 그래야 서버를 옮기거나 데이터가 망가졌을 때 다시 인증하지 않고 되살릴 수 있습니다.

#### Acceptance Criteria

1. WHEN Admin이 Admin_API로 백업 생성을 요청하면, THE Backup_Service SHALL 관리자 계정(username, password_hash, is_default), settings 테이블의 모든 키와 값, 상태(status)와 인증 여부에 관계없이 등록된 모든 저장소를 담은 Backup_File을 응답 본문으로 반환한다
2. WHEN 등록된 저장소가 0개인 상태에서 Admin이 백업 생성을 요청하면, THE Backup_Service SHALL 저장소 목록을 빈 목록으로 담은 Backup_File을 반환한다
3. THE Backup_Service SHALL 저장소마다 name, mount_path, root_path, app_key, App secret, refresh token, account_id, account_email, account_name, cache_ttl_min, status, auth_mode, redirect_uri, authorized_at 값을 Backup_Payload에 포함하고, 값이 비어 있는 필드(예: 아직 인증하지 않은 저장소의 refresh token, authorized_at)는 생략하지 않고 빈 값으로 포함한다
4. THE Backup_Service SHALL thumbs, hls, media_probe, sessions, oauth_states 데이터와 Instance_Master_Key, `config.json`과 `secret.key`의 내용을 Backup_Payload에서 제외한다
5. WHEN Backup_Service가 Backup_Payload를 만들면, THE Backup_Service SHALL Instance_Master_Key로 암호화되어 저장된 App secret과 refresh token을 복호화한 평문 값으로 Backup_Payload에 담아, Backup_Payload에 Instance_Master_Key로 암호화된 값이 하나도 남지 않게 한다
6. IF 비어 있지 않은 App secret 또는 refresh token 중 하나라도 Instance_Master_Key로 복호화하지 못하면, THEN THE Backup_Service SHALL 백업 생성을 중단하고, Backup_File을 일부라도 반환하거나 디스크에 쓰지 않으며, 복호화에 실패한 저장소의 mount_path를 포함하고 Sensitive_Value는 포함하지 않는 오류를 반환한다
7. WHEN Backup_Service가 Backup_File을 반환하면, THE Admin_API SHALL 파일 이름이 `dvfs-backup-YYYYMMDD-HHMMSS.json`(시각은 Envelope 생성 시각을 UTC로 표기)인 첨부 파일로 응답한다
8. WHEN Backup_Service가 백업을 생성하면, THE Backup_Service SHALL 관리자 계정, 설정, 저장소 목록을 하나의 읽기 트랜잭션 안에서 읽어, 백업 도중 다른 요청이 커밋한 변경은 Backup_Payload에 전부 반영되거나 전혀 반영되지 않게 한다
9. WHEN Backup_Service가 백업을 생성하면, THE Backup_Service SHALL 관리자 계정, 설정, 저장소 목록, 세션 중 어떤 데이터도 수정하지 않는다
10. IF 백업 생성 중 데이터베이스 읽기가 실패하면, THEN THE Backup_Service SHALL 백업 생성을 중단하고, Backup_File을 일부라도 반환하지 않으며, 백업 생성 실패를 나타내는 오류를 반환한다

### Requirement 2: 백업 암호화 선택

**User Story:** 관리자로서, 백업 파일을 비밀번호로 암호화할지 고르고 싶습니다. 그래야 파일이 유출돼도 Dropbox 토큰이 노출되지 않게 하거나, 필요하면 내용을 직접 읽을 수 있는 평문 파일을 받을 수 있습니다.

#### Acceptance Criteria

1. WHEN Admin이 백업 생성 화면을 열면, THE Admin_UI SHALL "암호화 사용"과 "암호화 미사용" 두 선택지 중 하나만 고를 수 있는 선택 컨트롤을 표시하고, 처음 선택된 값을 "암호화 사용"으로 둔다
2. WHILE 백업 생성 화면에서 "암호화 사용"이 선택되어 있는 동안, THE Admin_UI SHALL 입력 문자가 가려진 Backup_Password 입력란과 Backup_Password 확인 입력란을 표시한다
3. IF 백업 생성 요청이 암호화 사용을 지정했고 Backup_Password가 비어 있거나, 8자 미만이거나, 128자를 넘으면(유니코드 문자 단위로 셈), THEN THE Admin_API SHALL 백업 생성을 거부하고 허용 길이(8~128자)를 벗어났음을 알리는 오류를 반환하며, Backup_File을 만들지 않는다
4. IF Admin이 백업 생성 버튼을 눌렀을 때 Backup_Password와 확인 입력값이 문자 단위로 정확히 일치하지 않으면, THEN THE Admin_UI SHALL Admin_API로 백업 생성 요청을 보내지 않고, 두 값이 다르다는 오류를 확인 입력란에 표시하며, 입력한 두 값을 지우지 않고 유지한다
5. WHEN 암호화 사용을 지정한 백업 생성 요청이 검증을 통과하면, THE Backup_Serializer SHALL 메모리 비용이 64 MiB 이상인 메모리 사용형 키 유도 함수로 Backup_Password에서 256비트 키를 유도하고, 인증 암호화로 Backup_Payload 전체를 암호화하여, 결과 Encrypted_Backup의 어느 바이트 구간에도 Sensitive_Value의 평문이 나타나지 않게 한다
6. WHEN Backup_Serializer가 Encrypted_Backup을 만들면, THE Backup_Serializer SHALL 백업마다 길이 16바이트 이상의 새 무작위 salt와 새 무작위 nonce를 만들고, salt, nonce, 키 유도 매개변수를 Envelope에 평문으로 기록하여, 같은 Backup_Password와 같은 Backup_Payload로 두 번 만든 Encrypted_Backup의 salt, nonce, 암호문이 서로 다르게 한다
7. WHEN Backup_Serializer가 Encrypted_Backup을 만들면, THE Backup_Serializer SHALL Envelope의 모든 평문 필드(Format_Version, 암호화 여부, Encryption_Mode, salt, nonce, 키 유도 매개변수 포함)를 인증 암호화의 추가 인증 데이터로 묶어, 이 필드 중 어느 하나라도 1바이트 이상 바뀌거나 잘못된 Backup_Password를 쓰면 복호화가 실패하게 한다
8. WHILE 백업 생성 화면에서 "암호화 미사용"이 선택되어 있는 동안, THE Admin_UI SHALL Backup_Password 입력란을 숨기고, 백업 파일에 Dropbox 토큰과 App secret이 평문으로 들어간다는 경고와 경고 확인란을 표시하며, 경고 확인란이 체크된 동안에만 백업 생성 버튼을 활성화한다
9. THE Backup_Service SHALL Backup_Password와 Backup_Password에서 유도한 키를 디스크, 데이터베이스, 로그, Admin_API 응답 본문, 오류 메시지, Backup_File 어디에도 기록하지 않는다
10. IF 백업 생성 요청이 암호화 미사용을 지정했지만 경고 확인 여부를 확인됨으로 표시하지 않았으면, THEN THE Admin_API SHALL 백업 생성을 거부하고 평문 백업 경고 확인이 필요하다는 오류를 반환하며, Plain_Backup을 만들지 않는다
11. IF 키 유도 또는 암호화 단계가 실패하면, THEN THE Backup_Service SHALL 백업 생성을 중단하고, 암호화 실패를 알리는 오류를 Admin_API 응답으로 반환하며, 일부만 쓰인 Backup_File을 남기지 않는다

### Requirement 3: 백업 파일 형식과 파서

**User Story:** 개발자로서, 백업 파일 형식이 명확히 정의되고 쓰기와 읽기가 서로 맞기를 원합니다. 그래야 다른 버전의 앱에서도 백업을 안전하게 읽을 수 있습니다.

#### Acceptance Criteria

1. THE Backup_Serializer SHALL Backup_File을 최상위가 JSON 객체 하나인 UTF-8 JSON 문서로 만들고, Envelope에 형식 식별자 문자열 `dvfs-backup`, 1 이상의 정수인 Format_Version, 초 단위까지의 UTC 생성 시각(RFC 3339, `Z` 접미사), 비어 있지 않은 앱 버전 문자열, 불리언 암호화 여부를 기록하며, 암호화 여부가 true이면 Encryption_Mode(`password` 또는 `instance-key`)를 함께 기록한다
2. WHEN Backup_Serializer가 Plain_Backup을 만들면, THE Backup_Serializer SHALL 암호화 여부를 false로 기록하고, 관리자 계정, 설정, 저장소 목록을 담은 Backup_Payload를 Envelope 안의 JSON 객체로 기록한다
3. WHEN Backup_Serializer가 Encrypted_Backup 또는 Instance_Key_Backup을 만들면, THE Backup_Serializer SHALL 암호화 여부를 true로, Encryption_Mode를 Encrypted_Backup이면 `password`, Instance_Key_Backup이면 `instance-key`로 기록하고, 암호화된 Backup_Payload를 Envelope 안의 표준 base64 문자열(RFC 4648 표준 알파벳, 패딩 포함)로 기록하며, Backup_File 어디에도 Backup_Payload나 Sensitive_Value의 평문을 남기지 않는다
4. WHEN 기준 5를 모두 통과하는 Backup_File이 주어지고, Encrypted_Backup이면 만들 때 쓴 것과 같은 Backup_Password가, Instance_Key_Backup이면 만들 때 쓴 것과 같은 Instance_Master_Key가 함께 주어지면, THE Backup_Parser SHALL Backup_Payload 객체와 Envelope의 Format_Version, 생성 시각, 앱 버전, 암호화 여부를 함께 반환한다
5. IF Backup_File이 비어 있거나, 올바른 UTF-8 JSON 객체가 아니거나, 형식 식별자가 `dvfs-backup`이 아니거나, Format_Version이 1 이상의 정수가 아니거나, 생성 시각이 RFC 3339 형식이 아니거나, 기준 1의 필드 중 하나라도 없거나, 암호화 여부가 true인데 Encryption_Mode가 `password`, `instance-key` 중 하나가 아니거나, Encryption_Mode별 필수 필드(`password`이면 salt, nonce, 키 유도 매개변수, `instance-key`이면 nonce와 키 유도 레이블) 중 하나라도 없거나, 암호화 여부와 Backup_Payload 형태(false이면 JSON 객체, true이면 base64 문자열)가 맞지 않거나, base64 디코딩이 실패하거나, 평문 또는 복호화한 Backup_Payload에 관리자 계정, 설정, 저장소 목록 중 하나라도 없으면, THEN THE Backup_Parser SHALL Backup_Payload를 일부도 반환하지 않고 "백업 파일 형식이 아닙니다" 오류를 반환한다
6. FOR ALL 유효한 Backup_Payload(Requirement 4의 검증 규칙을 통과하며, 저장소 0개와 한글 등 비ASCII 값을 포함한 경우 포함)와 Requirement 2의 길이 규칙을 통과한 Backup_Password에 대해, Backup_Serializer로 Encrypted_Backup을 만든 뒤 같은 Backup_Password로 Backup_Parser가 읽은 결과 SHALL 원래 Backup_Payload와 같다. 여기서 "같다"는 관리자 계정 필드, 설정의 모든 키와 값, 저장소 목록의 항목 수와 순서, 각 저장소의 모든 필드 값(Sensitive_Value는 바이트 단위)이 일치한다는 뜻이다 (왕복 속성)
7. FOR ALL 유효한 Backup_Payload(기준 6과 같은 정의)에 대해, Backup_Serializer로 Plain_Backup을 만든 뒤 Backup_Parser가 읽은 결과 SHALL 기준 6의 "같다" 정의에 따라 원래 Backup_Payload와 같다 (왕복 속성)
8. FOR ALL 유효한 Backup_Payload에 대해, 같은 Backup_Payload와 Backup_Password로 두 번 만든 Encrypted_Backup SHALL salt 값, nonce 값, 암호문이 각각 서로 다르다 (salt와 nonce 재사용 없음)
9. IF 기준 5를 통과한 Encrypted_Backup 또는 Instance_Key_Backup의 인증 복호화가 실패하면(틀린 Backup_Password, 다른 Instance_Master_Key, 바뀐 암호문, 또는 추가 인증 데이터로 묶인 Envelope 필드 변경), THEN THE Backup_Parser SHALL Backup_Payload를 일부도 반환하지 않고, 형식 오류와 구별되는 복호화 실패 오류를 반환한다
10. IF Encrypted_Backup이 주어졌는데 Backup_Password가 없거나 빈 문자열이면, THEN THE Backup_Parser SHALL 복호화를 시도하지 않고, Envelope의 Format_Version, 생성 시각, 앱 버전, 암호화 여부와 함께 비밀번호가 필요하다는 오류를 반환한다

### Requirement 4: 복원 파일 업로드와 검증

**User Story:** 관리자로서, 복원을 적용하기 전에 백업 파일이 올바른지 확인하고 내용을 미리 보고 싶습니다. 그래야 잘못된 파일로 현재 상태를 덮어쓰지 않습니다.

#### Acceptance Criteria

1. WHEN Admin이 기준 2와 기준 4~11의 검증을 모두 통과하는 Backup_File을 업로드하면, THE Restore_Service SHALL Backup_File의 Format_Version, Encrypted_Backup 여부, 백업 생성 시각, 관리자 username, 설정 항목 수, 저장소별 이름과 마운트 경로를 포함한 Restore_Preview를 반환하며, Restore_Preview에 Sensitive_Value의 평문을 포함하지 않고, 이 단계에서 관리자 계정·settings·storages를 바꾸거나 Pre_Restore_Snapshot을 만들지 않는다
2. IF 업로드한 Backup_File이 10MB(10 × 1024 × 1024 바이트)를 넘으면, THEN THE Admin_API SHALL 10MB를 넘는 본문을 읽지 않고 크기 초과 오류를 반환한다 (정확히 10 × 1024 × 1024 바이트인 파일은 허용한다)
3. WHEN Admin이 선택한 Backup_File의 Envelope가 Encrypted_Backup을 나타내면, THE Admin_UI SHALL Backup_Password 입력란을 표시하고, 입력란이 비어 있는 동안 복원 검증 요청 버튼을 비활성화한다
4. IF Backup_Password로 Encrypted_Backup의 복호화나 무결성 검증이 실패하면, THEN THE Restore_Service SHALL 비밀번호가 틀린 경우와 파일이 손상된 경우를 구분하지 않고 "비밀번호가 틀렸거나 파일이 손상되었음"을 알리는 오류를 반환하며, Backup_Payload의 어떤 내용도 응답에 포함하지 않는다
5. IF Backup_File의 Format_Version이 현재 앱이 지원하는 최대 Format_Version보다 크면, THEN THE Restore_Service SHALL 더 새로운 버전의 앱에서 만든 백업임을 알리는 오류를 반환한다
6. WHEN Admin이 현재 앱이 지원하는 최대 Format_Version보다 작고 1 이상인 Format_Version의 Backup_File을 업로드하면, THE Restore_Service SHALL Backup_Payload를 현재 Format_Version 구조로 변환한 뒤 기준 7~9의 검증을 적용한다
7. IF Backup_Payload의 설정 값 중 하나 이상이 settings의 Validate() 범위·열거형 규칙을 통과하지 못하면, THEN THE Restore_Service SHALL 실패한 모든 설정 키와 키별 실패 이유를 한 번의 오류 응답에 포함해 반환한다
8. IF Backup_Payload의 저장소 마운트 경로 중 두 개가 같거나 경로 구간 단위로 서로 상위·하위 관계이면(예: `/path/to`는 `/path`, `/path/to/x`와 충돌하고 `/path/to2`와는 충돌하지 않음), THEN THE Restore_Service SHALL 충돌하는 모든 마운트 경로 쌍을 포함한 오류를 반환한다
9. IF Backup_Payload의 관리자 username 또는 password_hash가 없거나 빈 문자열이거나 공백 문자로만 이루어져 있으면, THEN THE Restore_Service SHALL 비어 있는 필드 이름을 포함한 관리자 계정 오류를 반환한다
10. IF Backup_Parser가 Backup_File을 Envelope로 해석하지 못하거나 Envelope에 정수 Format_Version이 없거나 Format_Version이 1 미만이면, THEN THE Restore_Service SHALL 백업 파일 형식이 올바르지 않음을 알리는 오류를 반환한다
11. IF Admin이 Backup_Password 없이 Encrypted_Backup의 검증을 요청하면, THEN THE Restore_Service SHALL Backup_Password가 필요함을 알리는 오류를 반환한다
12. IF 기준 2, 4, 5, 7, 8, 9, 10, 11의 오류 중 하나라도 발생하면, THEN THE Restore_Service SHALL Restore_Preview를 반환하지 않고 관리자 계정·settings·storages를 업로드 전과 같은 상태로 유지한다

### Requirement 5: 복원 적용

**User Story:** 관리자로서, 확인한 백업으로 인스턴스 상태를 한 번에 바꾸고 싶습니다. 그래야 복원이 중간에 실패해도 반쯤 바뀐 상태가 남지 않습니다.

#### Acceptance Criteria

1. WHEN Admin이 확인 대화 상자에서 복원을 승인해 Admin_API가 복원 승인 요청을 받으면, THE Restore_Service SHALL 관리자 계정, 설정, 저장소 목록을 Backup_Payload 내용으로 전체 교체하여, 교체 후 Backup_Payload에 없는 설정 키와 저장소가 남지 않고 저장소 수가 Backup_Payload의 저장소 수(0개 포함)와 같게 한다
2. THE Restore_Service SHALL 관리자 계정, 설정, 저장소 목록의 교체와 oauth_states 전체 삭제를 하나의 데이터베이스 트랜잭션으로 수행하여, 같은 시점의 다른 요청이 복원 전 상태 또는 복원 후 상태 중 하나만 읽고 일부만 바뀐 상태를 읽지 않게 한다
3. IF 복원 트랜잭션 안의 어느 단계(기존 행 삭제, 새 행 기록, App secret·refresh token 암호화, 커밋)든 실패하면, THEN THE Restore_Service SHALL 트랜잭션을 롤백해 관리자 계정, 설정, 저장소 목록, oauth_states를 복원 요청 직전 값 그대로 유지하고, 로그인 세션과 메모리의 설정·저장소 상태를 바꾸지 않은 채 실패한 단계를 나타내는 오류를 반환한다
4. WHEN Restore_Service가 저장소를 기록하면, THE Restore_Service SHALL Backup_Payload의 App secret과 refresh token을 현재 인스턴스의 Instance_Master_Key로 암호화해 저장하여, 복원 후 현재 Instance_Master_Key로 복호화한 값이 Backup_Payload의 값과 같고 Backup_File을 만든 인스턴스의 Instance_Master_Key에 의존하지 않게 한다
5. WHILE 복원 하나가 진행 중이면(복원 승인 요청을 받은 시점부터 Pre_Restore_Snapshot 저장, 복원 트랜잭션, 런타임 상태 반영이 끝나거나 실패해 응답을 반환할 때까지), THE Admin_API SHALL 다른 복원 승인 요청을 409 상태 코드와 다른 복원이 진행 중임을 나타내는 오류로 거부하고, 거부한 요청으로 인스턴스 상태나 Pre_Restore_Snapshot을 바꾸지 않는다
6. WHEN Admin이 Restore_Preview 화면에서 복원 적용 버튼을 누르면, THE Admin_UI SHALL 복원 승인 요청을 보내기 전에 "현재 관리자 계정, 설정, 저장소 목록이 백업 내용으로 바뀝니다"라는 문구와 승인 버튼, 취소 버튼이 있는 확인 대화 상자를 표시한다
7. THE Restore_Service SHALL 복원 승인 요청을 Restore_Preview 응답에 포함해 반환한 미리보기 식별자로만 받고, 그 식별자가 가리키는, 미리보기 단계에서 검증한 Backup_Payload를 변경 없이 적용하며, 승인 단계에서 Backup_File 재업로드나 Backup_Password 재입력을 요구하지 않는다
8. IF Admin이 확인 대화 상자에서 취소 버튼을 누르거나 대화 상자를 닫으면, THEN THE Admin_UI SHALL 복원 승인 요청을 보내지 않고 Restore_Preview 화면을 그대로 유지한다
9. IF 복원 승인 요청의 미리보기 식별자가 존재하지 않거나, 발급 후 10분이 지났거나, 이미 한 번 복원 적용에 사용되었거나, 요청한 로그인 세션이 아닌 다른 세션에서 발급된 것이면, THEN THE Restore_Service SHALL 복원을 적용하지 않고 Pre_Restore_Snapshot도 만들지 않은 채 Backup_File을 다시 업로드해야 함을 나타내는 오류를 반환한다

### Requirement 6: 복원 후 런타임 상태 반영

**User Story:** 관리자로서, 복원이 끝나면 서버를 다시 시작하지 않아도 복원된 설정과 저장소가 바로 쓰이기를 원합니다.

#### Acceptance Criteria

1. WHEN 복원 트랜잭션이 커밋되면, THE Restore_Service SHALL Admin_API가 복원 응답을 반환하기 전에 메모리 설정 Store의 모든 값을 복원된 설정 값으로 바꾸고, 모든 값을 바꾼 뒤 등록된 설정 변경 리스너를 각각 한 번씩 호출하여, 응답 이후 첫 설정 조회부터 복원된 값이 반환되게 한다
2. WHEN 복원 트랜잭션이 커밋되면, THE Restore_Service SHALL Admin_API가 복원 응답을 반환하기 전에 메모리에 있는 모든 저장소별 Dropbox 클라이언트, 목록 캐시 항목, 폴더 크기 캐시 항목을 버리고 복원된 저장소 목록의 저장소마다 Dropbox 클라이언트를 새로 만들어, 응답 이후 첫 요청부터 복원된 저장소 목록과 자격 증명만 사용되게 한다
3. WHEN 복원 트랜잭션이 커밋되면, THE Restore_Service SHALL 복원을 요청한 세션을 포함해 sessions 테이블의 모든 로그인 세션을 삭제하고, 복원 성공 응답은 요청한 클라이언트에 그대로 전달하며, 그 뒤 복원 전에 발급된 세션으로 들어오는 모든 요청은 인증되지 않은 요청으로 거부되게 한다
4. WHEN Admin_API가 복원 성공 응답을 반환하면, THE Admin_UI SHALL 로그인 화면으로 이동하고, 관리자가 로그인을 시도하거나 메시지를 닫을 때까지 로그인 화면에 "복원이 끝났습니다. 백업에 저장된 관리자 계정으로 다시 로그인하세요"라는 메시지를 표시한다
5. WHEN 복원 트랜잭션이 커밋되면, THE Restore_Service SHALL thumbs 파일 캐시와 hls 파일 캐시의 파일, media_probe 테이블의 행을 하나도 삭제하거나 변경하지 않는다
6. IF 복원된 저장소 중 하나 이상에 대해 Dropbox 클라이언트를 만들 수 없으면, THEN THE Restore_Service SHALL 커밋된 복원 데이터를 되돌리지 않고 나머지 저장소의 클라이언트를 계속 만들며, 클라이언트를 만들지 못한 저장소를 사용할 수 없는 상태로 표시하고, 해당 저장소 이름 목록을 Admin_API 복원 응답에 포함한다
7. WHEN 복원 트랜잭션이 커밋되면, THE Restore_Service SHALL Admin_API가 복원 응답을 반환하기 전에 진행 중인 모든 HLS 트랜스코드 세션을 중지하여, 응답 이후의 재생 요청은 새로 만든 저장소 클라이언트로 시작한 트랜스코드 세션에서만 처리되게 한다

### Requirement 7: 복원 전 자동 보관

**User Story:** 관리자로서, 잘못된 백업으로 복원했을 때 바로 이전 상태로 돌아가고 싶습니다.

#### Acceptance Criteria

1. WHEN Admin이 복원을 승인하면, THE Restore_Service SHALL 복원 진행 상태(Requirement 5의 5항)에 들어간 뒤 복원 트랜잭션을 시작하기 전에, Requirement 1의 1~4항과 같은 범위(관리자 계정, 설정 전체, 저장소 목록 전체, 캐시와 일시적 데이터 제외)의 현재 상태로 Pre_Restore_Snapshot을 만들어 `data/backups/` 디렉터리에 저장하고, 이 디렉터리가 없으면 먼저 만든다
2. THE Restore_Service SHALL Pre_Restore_Snapshot의 Backup_Payload(Requirement 1의 5항처럼 복호화한 App secret과 refresh token 포함) 전체를 Instance_Master_Key에서 기존 하위 키(예: `dvfs-encrypt-v1`)와 다른 유도 레이블로 만든 하위 키로, 스냅숏마다 새 nonce를 쓰는 인증 암호화로 암호화하고, Envelope에 Encryption_Mode `instance-key`와 그 유도 레이블, nonce를 기록해 Instance_Key_Backup 형식으로 저장한다
3. WHERE 서버가 Linux에서 실행되면, THE Restore_Service SHALL `data/backups/` 디렉터리를 0700 권한으로, Pre_Restore_Snapshot 파일을 0600 권한으로 만든다
4. IF `data/backups/` 디렉터리 생성, Pre_Restore_Snapshot 내용 생성(Requirement 1의 6항의 복호화 실패 포함), 암호화, 파일 쓰기 중 하나라도 실패하면, THEN THE Restore_Service SHALL 복원 트랜잭션을 시작하지 않고, 쓰다 만 스냅숏 파일을 `data/backups/`에 남기지 않으며, 인스턴스 상태를 바꾸지 않고, 복원 진행 상태를 해제한 뒤 스냅숏 저장 실패 오류를 반환한다
5. WHEN 새 Pre_Restore_Snapshot을 저장한 뒤 `data/backups/`의 Pre_Restore_Snapshot이 5개를 넘으면, THE Restore_Service SHALL Envelope 생성 시각이 가장 이른 것부터 삭제해 5개만 남기고, Restore_Service가 만든 Pre_Restore_Snapshot이 아닌 파일은 개수에 넣지 않고 삭제하지 않는다
6. THE Admin_UI SHALL 백업 화면에 Pre_Restore_Snapshot 목록을 최신 생성 시각 순으로 표시하고, 항목마다 생성 시각(브라우저 현지 시간, 초 단위)과 파일 크기(KB 또는 MB, 소수점 첫째 자리)를 보여 주며, 항목마다 그 스냅숏으로 복원하는 동작을 제공하고, 보관된 스냅숏이 0개이면 스냅숏이 없다는 안내 문구를 표시한다
7. WHEN Admin이 Pre_Restore_Snapshot으로 복원을 요청하면, THE Restore_Service SHALL Backup_Password 입력을 요구하지 않고 업로드한 Backup_File과 같은 검증, 미리보기, 승인, 적용 절차(새 Pre_Restore_Snapshot 저장 포함)를 따르며, 5항의 보관 개수 정리로 원본 스냅숏 파일이 삭제되더라도 미리보기에서 검증한 내용으로 복원을 끝낸다
8. IF Admin이 요청한 Pre_Restore_Snapshot 식별자가 Admin_API가 반환한 Pre_Restore_Snapshot 목록에 없는 값(경로 구분자나 `..`가 들어간 값 포함)이면, THEN THE Admin_API SHALL `data/backups/` 안팎의 어떤 파일도 읽지 않고 스냅숏을 찾을 수 없다는 오류를 반환한다
9. IF Pre_Restore_Snapshot을 현재 Instance_Master_Key로 복호화하지 못하거나 파일 형식이 맞지 않으면, THEN THE Restore_Service SHALL 미리보기 단계에서 복원을 거부하고, 인스턴스 상태를 바꾸지 않은 채 스냅숏을 읽을 수 없다는 오류를 반환한다
10. IF 5항의 보관 개수 정리 중 오래된 Pre_Restore_Snapshot 삭제가 실패하면, THEN THE Restore_Service SHALL 복원을 계속 진행하고 삭제에 실패한 파일 이름을 서버 로그에 기록한다

### Requirement 8: 접근 제어와 기록

**User Story:** 관리자로서, 백업과 복원은 로그인한 관리자만 할 수 있고 민감 정보가 로그에 남지 않기를 원합니다.

#### Acceptance Criteria

1. THE Admin_API SHALL 백업 생성, 복원 미리보기, 복원 적용, Pre_Restore_Snapshot 조회와 다운로드의 모든 요청을 `/api/admin` 경로 아래에서만 받고, 각 요청에 유효한 관리자 세션과 유효한 CSRF 토큰을 모두 요구한다
2. IF 요청에 관리자 세션이 없거나, 세션이 만료되었거나, 세션이 서버에서 확인되지 않으면, THEN THE Admin_API SHALL 401 상태 코드를 반환하고, 응답 본문에 Backup_File, Backup_Payload, Restore_Preview, Pre_Restore_Snapshot 내용을 넣지 않으며, 데이터베이스와 저장소 설정을 변경하지 않는다
3. WHEN 백업 생성 성공, 복원 적용 성공, 복원 미리보기 실패, 복원 적용 실패 중 하나가 일어나면, THE Backup_Service와 Restore_Service SHALL 해당 사건마다 서버 로그 항목 1건을 남기고, 그 항목에 다음을 포함한다: 초 단위 이상 정밀도의 시각, 동작 종류(백업 생성, 복원 적용, 복원 미리보기 실패, 복원 적용 실패 중 하나), 암호화 형태(Encrypted_Backup, Instance_Key_Backup, Plain_Backup 중 하나, 판별 전에 실패하면 "알 수 없음"), 저장소 수(0 이상의 정수, 판별 전에 실패하면 "알 수 없음"), 요청 IP, 그리고 실패 사건이면 실패 원인 분류(크기 초과(Requirement 4의 2항), Backup_Password 누락(Requirement 4의 11항), 복호화 실패(Requirement 4의 4항과 Requirement 7의 9항, 비밀번호 불일치와 파일 손상을 구분하지 않음), 파일 형식 오류, 지원하지 않는 Format_Version, Backup_Payload 검증 오류(Requirement 4의 7~9항), 미리보기 식별자 무효 또는 만료(Requirement 5의 9항), 스냅숏 저장 실패(Requirement 7의 4항), 적용 중 오류 중 하나)
4. THE Backup_Service, Restore_Service, Admin_API SHALL Sensitive_Value, Backup_Password, Instance_Master_Key, Backup_Payload 본문을 서버 로그 항목, Admin_API 오류 응답, Admin_UI에 표시되는 오류 메시지에 어떤 부분 문자열로도 포함하지 않으며, 이 규칙은 성공 경로와 실패 경로 모두에 적용된다
5. WHEN Admin_API가 Backup_File을 응답 본문으로 반환하면(백업 생성 결과와 Pre_Restore_Snapshot 다운로드 포함), THE Admin_API SHALL 그 응답에 `Cache-Control: no-store` 헤더를 붙인다
6. IF 유효한 관리자 세션이 있는 요청에 CSRF 토큰이 없거나 토큰이 세션과 일치하지 않으면, THEN THE Admin_API SHALL 요청을 거부하고, 백업 생성, 복원 미리보기, 복원 적용 중 어느 것도 실행하지 않으며, 데이터베이스와 저장소 설정을 변경하지 않고, 응답 본문에 백업 데이터를 넣지 않는다
