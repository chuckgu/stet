# 써 보는 길 (6단계 첫 부분) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 한 사람이 자기 PC에서 자기 DB(대개 원격의 매니지드 Postgres, 계정 하나)에 Stet을 10분 안에 붙여, 에이전트의 쓰기가 몇 행에 닿았는지를 터미널에서 한 줄씩 본다.

**Architecture:** 프록시가 업스트림에 붙을 때 `upstream` 주소의 TLS 파라미터(pgx가 읽는 것과 같은 해석)를 따라 SSLRequest와 TLS 인사를 하고, 그 위에서 지금처럼 바이트를 중계한다. 클라이언트 쪽은 평문으로 남기고, 서버가 TLS 연결에서 제안하는 `SCRAM-SHA-256-PLUS`만 인증 방식 목록에서 뺀다. 게이트는 기록한 판단을 알리는 자리 하나를 내주고, `stet proxy`가 그것을 한 줄로 만들어 대기열을 거쳐 표준 출력에 쓴다. `stet init`은 읽기 전용으로 DB를 살펴 설정 파일을 쓴다. 엔진(판정, 상태 조립, 기록)은 바꾸지 않는다.

**Tech Stack:** Go 1.27, `crypto/tls`, `github.com/jackc/pgx/v5`(pgconn, pgproto3, pgxpool), `go.yaml.in/yaml/v3`, 테스트 서버 PostgreSQL 14.18, Docker 28, bash.

**Spec:** [docs/spec-v0.2-tryout.md](spec-v0.2-tryout.md). 스펙과 이 계획이 다르면 아래 "스펙 보정"에 적힌 곳만 계획을 따르고, 나머지는 스펙을 따른다. 보정은 Task 11이 스펙에 옮겨 적고, 소유자에게는 끝날 때 보고한다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 규칙 6: 섀도에서 에이전트의 문장과 그 결과는 Stet 없이 보냈을 때와 같다. 이 단계가 고치는 바이트는 TLS 업스트림에서의 AuthenticationSASL 방식 목록 하나뿐이다. 규칙 1: 출력과 `init`은 판단 경로에 들지 않는다. 규칙 5: 문장, 테이블 이름, 이유는 에이전트나 DB가 쓴 글자다. 터미널에 그대로 내지 않는다.
- **판정을 바꾸지 않는다.** `internal/decide`, 게이트의 규칙과 한도, `stet.decisions`의 스키마는 건드리지 않는다. 게이트에 더하는 것은 `Decided` 자리 하나와 `TracksKeyType`뿐이다.
- 스펙의 값. SQLSTATE `08001`(업스트림에 붙지 못함), `28000`(`stet: the server offers only channel-bound authentication, which cannot pass through stet`). 빼는 방식 이름 `SCRAM-SHA-256-PLUS`. `init`의 기본 `-config stet.yaml`, `-listen 127.0.0.1:6432`, 테이블 상한 100, `impact.max_rows: 100`, 파일 권한 `0600`, PostgreSQL 14 이상, 지우는 문장 `DROP SCHEMA stet CASCADE;`. 줄의 대기열 1,024줄, 문장 80자(79자와 `…`), 판정 칸 12글자, 버린 줄 알림 `… 37 lines dropped`. 시작 줄 `stet: watching 12 tables on 127.0.0.1:6432 (shadow: nothing is blocked)`. 묶음 `dist/stet_<버전>_<os>_<arch>.tar.gz`, `dist/checksums.txt`.
- **`sslmode`를 적지 않은 `upstream`은 `prefer`다.** 이 단계부터 프록시 구간도 서버가 TLS를 받으면 TLS로 간다. Task 1 뒤로 시험 서버가 TLS를 받으므로, `sslmode`를 적지 않은 기존 테스트의 세션은 TLS 업스트림을 지난다. 그 테스트들의 단언은 고치지 않는다.
- 테스트: `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. PostgreSQL 14.18이 이미 떠 있다(superuser `postgres`/`postgres-pw`, 데이터베이스 `stet_test`, Stet의 계정 `stet_scram`/`scram-pw`, 에이전트 `stet_md5`/`md5-pw`, `stet_clear`/`clear-pw`, 비밀번호 없는 `stet_trust`). 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다. 모든 Task는 이 명령이 모든 패키지에서 `ok`인 채로 끝난다.
- **55432의 시험 서버를 켜거나 끄지 않는다.** 그 일은 진행자(controller)가 한다. Task가 이름을 적어 허락한 곳에서만, 자기 임시 디렉터리에 `scripts/testdb.sh start <dir> 55440`으로 일회용 서버를 띄울 수 있고, 끝나면 멈추고 디렉터리를 지운다.
- **`stet_trust`를 어떤 역할에도 부여하지 않고, 어떤 역할도 `stet_trust`에 부여하지 않는다.** PUBLIC, `public` 스키마, 공유 데이터베이스 `stet_test` 자체에는 GRANT·REVOKE하지 않는다. 다른 권한이 필요한 테스트는 자기 데이터베이스와 역할을 만들고 지운다. 서버에 이미 있는 벤치마크 데이터베이스와 역할(`crm_owner`, `gate_svc`, `agent_v`, `agent_i`, `agent_x`, `agent_sync`, 템플릿 `crm_base`)은 읽고 복제할 수만 있다.
- TDD: 동작이 바뀌는 곳은 실패하는 테스트를 먼저 쓰고, 돌려서 실패를 본 뒤에 구현한다. 스크립트, Dockerfile, 워크플로, 문서는 Task가 적은 확인 명령으로 대신한다.
- `internal/proxy`를 건드리는 Task는 `internal/proxy/latency_test.go`(p95 2ms)도 돌린다. 그 파일의 기존 테스트 함수는 고치지 않는다.
- 기존 테스트의 단언은 고치지 않는다. 테스트의 헬퍼를 새 시그니처(`proxy.New`)에 맞추는 것은 단언 변경이 아니다. 단언을 고쳐야 통과하는 테스트가 나오면 멈추고 보고한다.
- 새 Go 의존성을 더하지 않는다. `go.mod`의 `require`는 그대로다.
- **바깥에 내보내지 않는다.** `git push`, 릴리스, 이미지 올리기(`docker push`), 태그 만들기를 하지 않는다. Docker는 이 기기에서 빌드하고 띄우는 데만 쓴다.
- 비밀번호(DB 주소의 것)는 로그, 에러, 표준 출력에 내지 않는다. 나가는 곳은 `init`이 쓰는 설정 파일 하나다. API 키(`~/.config/stet/*.env`)는 읽지 않는다. 이 단계는 모델을 부르지 않는다.
- README.md, docs/concept-v0.2.md, docs/decisions.md(소유자의 문서)와 앞 단계의 계획 문서는 고치지 않는다.
- 문서와 YAML·SQL 스키마 주석은 한국어, 코드의 주석·식별자·에러 메시지·로그·프로그램의 출력(`init`이 쓰는 파일의 주석 포함)은 영어다.
- 브랜치 `v0.2-hardening`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고 마지막 줄은 `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`이다. 어느 모델이 쓰든 이 줄을 바꾸지 않는다.

## Review Focus

스펙이 암시하지만 수용 기준에 줄이 없는 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **터미널을 조작하는 글자.** 에이전트가 쓴 문장에 ESC 순서, CR, 백스페이스, 줄바꿈, 방향 제어 문자가 들어 있다. 한 판단은 화면에서 한 줄이고, 앞 줄을 지우거나 가짜 줄을 만들지 못한다. (Task 6 `TestLineHasNoControlCharacters`)
2. **비밀번호가 새는 곳.** 주소를 못 읽을 때의 에러, 접속 실패, 시작할 때의 경고, `init`의 안내. 설정 파일 밖 어디에도 비밀번호가 없다. (Task 5 `TestTransportWarningsNameNoPassword`, Task 10 `TestInitOutputNeverShowsThePassword`)
3. **YAML이 달리 읽는 이름.** 테이블 이름 `null`, `true`, `on`, `no`, `y`, 예약어 `user`, 퍼센트 인코딩된 `#`·`:`가 든 비밀번호. `init`이 쓴 파일을 `config.Load`가 같은 값으로 읽는다. (Task 10 `TestInitConfigRoundTripsOddNames`)
4. **답하지 않거나 엉뚱하게 답하는 업스트림.** SSLRequest 뒤의 침묵, TLS 인사 도중의 끊김, 깨진 인증 메시지. 그 세션 하나만 끝나고 프록시는 계속 받는다. (Task 3 `TestSilentUpstreamEndsOnlyThatSession`, Task 4 `TestStripPlus`의 깨진 본문)
5. **중간에 끊긴 `init`.** 표준 입력이 닫혀 있다(EOF), 파이프인데 `-yes`가 없다, 쓰는 도중에 죽는다. 파일은 다 쓰였거나 없다. 묻지 못했으면 쓰지 않는다. (Task 10 `TestInitDeclinedWritesNothing`, `TestInitWritesTheFileWhole`)

## 스펙 보정

코드에 맞춰 보니 스펙이 정하지 않았거나 스펙의 예와 규칙이 어긋난 곳이다.

| # | 스펙 | 확인한 것 | 보정 (Task) |
|---|---|---|---|
| P1 | 3장의 예 `… and sslmode=require needs it` | 프록시는 `sslmode`의 이름을 모른다. pgx가 해석한 TLS 설정만 받는다 | 문구는 `stet: upstream does not accept TLS, and the sslmode of upstream needs it`다 (3) |
| P2 | 6장 "칸 사이는 공백 둘", 1장의 예는 공백 하나와 열 맞춤 | 규칙과 예가 다르다 | 규칙을 따른다. 모든 칸 사이가 공백 둘이고, 판정만 12글자로 채운다. 1장의 예를 고친다 (6, 11) |
| P3 | 6장의 행 수 셋 | 실행하지 않은 쓰기에 상한 아래의 정확한 세기가 있을 수 있고, 상한에서 멈춘 세기에 추정이 함께 있을 수 있다 | 실제 행 수 → `N rows`. 없으면: 상한에서 멈췄고 추정이 있으면 `~E rows`, 추정이 없으면 `N+ rows`(비율은 뺀다), 방법이 `estimate`면 `~N rows`, 그 밖의 세기는 `N rows` (6) |
| P4 | 4장의 고르는 조건 | 파티션(자식)도 일반 테이블이고 부모의 기본 키를 물려받는다. 키 컬럼 이름도 YAML과 SQL에 그대로 적힌다 | 파티션은 고르지 않고 세지도 않는다(부모를 고른다). 키 컬럼 이름도 테이블 이름과 같은 글자 조건을 본다 (9) |
| P5 | 6장 "줄바꿈과 연속된 공백을 공백 하나로" | 문장에는 다른 제어 문자도 올 수 있다(Review Focus 1) | 제어 문자(Cc), U+2028·U+2029, 방향 제어(U+202A–202E, U+2066–2069)를 공백으로 본다. 테이블 이름, 이유, 연쇄 테이블 이름에도 한다 (6) |
| P6 | 6장이 끝낼 때를 말하지 않는다 | 받는 쪽이 멈춘 출력을 끝까지 기다리면 `stet`이 끝나지 않는다 | 닫을 때 대기열을 2초까지만 기다린다 (6) |
| P7 | 6장 "표준 출력이 터미널일 때" | 표준 라이브러리로는 문자 장치인지만 안다. `/dev/null`도 문자 장치다 | 문자 장치면 터미널로 본다. 새 의존성을 더하지 않는다. `/dev/null`로 보낸 줄은 해가 없고, `init`은 EOF를 "아니오"로 읽는다 (7, 10) |
| P8 | 4장 6 "TLS와 채널 바인딩 파라미터를 지우고" | 목록이 없다 | 이름이 `ssl`로 시작하는 파라미터, `channel_binding`, `gssencmode`, `requiressl`을 지운다. 남은 파라미터는 키 순서로 적고 `sslmode=disable`을 더한다. `listen`의 호스트가 비었거나 `0.0.0.0`·`::`면 `127.0.0.1`로 적는다 (10) |

**스펙이 구현에 맡겨 이 계획이 정한 것.** `proxy.New`는 주소 대신 `proxy.Upstream`을 받는다. TLS 설정은 `config`가 `pgconn.ParseConfig`의 결과에서 꺼내고 클라이언트 인증서를 뺀다. 목록을 고치는 탭은 서버의 첫 ReadyForQuery에서 자기 자신을 안쪽 탭으로 바꾼다. 줄은 `internal/lines`가 만들고 쓴다. `init`의 원격 판단은 호스트가 `localhost`이거나 루프백 IP인지로 한다. 묶음 안에는 `stet_<버전>_<os>_<arch>/` 디렉터리 하나가 있다. 실행 이미지는 `gcr.io/distroless/base-debian12:nonroot`다.

## File Structure

| 파일 | 책임 |
|---|---|
| `scripts/testdb.sh` | 시험 서버의 인증서와 `ssl=on` (수정) |
| `internal/config/config.go` | `upstream`의 TLS 해석과 거부, `send_fields` 조건 (수정) |
| `internal/pgtest/tls.go` | 통합 테스트가 함께 쓰는 것: 시험 서버의 인증서 (신규) |
| `internal/proxy/upstream.go` | `Upstream`, SSLRequest와 TLS 인사 (신규) |
| `internal/proxy/auth.go` | AuthenticationSASL 목록 고치기, 인증 동안만 있는 탭 (신규) |
| `internal/proxy/proxy.go` | `New`, `handle`, `forwardCancel`을 위에 잇기 (수정) |
| `internal/lines/format.go`, `writer.go` | 판단 한 줄 만들기, 대기열을 거쳐 쓰기 (신규) |
| `internal/gate/gate.go`, `stale_catalog.go` | `Decided`, `TracksKeyType` (수정) |
| `cmd/stet/main.go` | 명령 `init`·`version`, `-decisions`, 경고, 잇기 (수정) |
| `cmd/stet/transport.go` | 시작할 때의 경고 (신규) |
| `cmd/stet/version.go` | `stet version` (신규) |
| `cmd/stet/initdb.go` | `init`이 DB에서 읽는 것: `sslmode` 고르기, 확인, 테이블 고르기 (신규) |
| `cmd/stet/init.go` | `init` 명령: 묻기, 설정 쓰기, 안내 (신규) |
| `scripts/release.sh`, `Dockerfile`, `.dockerignore`, `.github/workflows/release.yml` | 설치 산출물 (신규) |
| `docs/quickstart.md` (신규), `stet.example.yaml`, `docs/spec-v0.1.md`, `docs/spec-v0.2-tryout.md` | 문서 (수정) |

---

### Task 0: 계획 커밋

- [ ] 이 문서를 커밋한다. `Docs: plan for the try-out path`.

---

### Task 1: 시험 서버에 TLS

**Files:** Modify `scripts/testdb.sh`.

이 Task는 일회용 서버를 띄워도 된다(55440, 자기 임시 디렉터리). 55432의 서버는 건드리지 않는다.

- [ ] **Step 1:** `start`가 `$dir/server.crt`와 `$dir/server.key`가 없으면 만든다. 새 디렉터리든 이미 있던 디렉터리든 같다. 자기 서명, CN `localhost`, SAN `DNS:localhost`와 `IP:127.0.0.1`, 3650일, 키는 `0600`. `openssl req`에 설정 파일을 주는 방식으로 만든다(OpenSSL과 LibreSSL에서 모두 된다). `pg_ctl`의 `-o`에 `-c ssl=on`을 더한다. `pg_hba.conf`와 계정은 그대로다. 머리 주석에 TLS를 적는다.
- [ ] **Step 2: 확인.** 임시 디렉터리에서 `scripts/testdb.sh start <dir> 55440` 뒤에:
  - `psql "host=127.0.0.1 port=55440 dbname=stet_test user=stet_scram sslmode=require" -Atc "select ssl from pg_stat_ssl where pid = pg_backend_pid()"`(`PGPASSWORD=scram-pw`) → `t`
  - 같은 것을 `sslmode=disable`로 → `f`
  - `sslmode=verify-full sslrootcert=<dir>/server.crt`로 `host=127.0.0.1`과 `host=localhost` → 둘 다 `t`
  - 멈추고, 인증서 두 파일을 지우고, 다시 `start` → 파일이 다시 생기고 `sslmode=require`가 `t`
  - 멈추고 디렉터리를 지운다. `lsof -i :55440`이 비어 있다.
- [ ] **Step 3:** 전체 테스트 명령이 통과한다(55432는 아직 TLS가 없고, 달라질 것이 없다).
- [ ] **Step 4:** 커밋 `Test database: accept TLS with a self-signed certificate`.

**진행자의 일 (Task 1 뒤, Task 2 전).** 55432의 서버를 `scripts/testdb.sh stop <dir>`, `start <dir> 55432`로 다시 띄운다. `sslmode=require`로 `pg_stat_ssl`이 `t`인지, 벤치마크 역할과 `crm_base`가 그대로인지 보고, 전체 테스트를 돌려 원장에 적는다.

---

### Task 2: 설정 — `upstream`의 TLS와 `send_fields`

**Files:** Modify `internal/config/config.go`. Test `internal/config/config_test.go`, `cmd/stet/main_test.go`.

**Interfaces — Produces:**

```go
// in Config:

// UpstreamTLS is the TLS setting of the proxy's leg to upstream, read from
// the same URL parameters Stet's own connections read (pgconn.ParseConfig).
// nil: plaintext. It never carries a client certificate: sslcert and sslkey
// are for Stet's own connections only.
UpstreamTLS *tls.Config
// UpstreamPlainFallback: sslmode is prefer, or not written. A session
// continues in plaintext when the server declines TLS.
UpstreamPlainFallback bool

// Transport says how well the leg to upstream is protected.
type Transport int

const (
	TransportPlain      Transport = iota // sslmode=disable
	TransportPreferred                   // prefer, or not written: TLS when the server takes it, unverified
	TransportUnverified                  // require without sslrootcert
	TransportVerified                    // verify-ca, verify-full, require with sslrootcert
)

func (c *Config) UpstreamTransport() Transport
```

`Parse`가 `pgconn.ParseConfig(c.Upstream)`을 부른다. 첫 호스트의 TLS 설정을 복제해 `Certificates`와 `GetClientCertificate`를 비운다. 같은 호스트와 포트의 대체 설정이 평문이면 `UpstreamPlainFallback`이다. `UpstreamAddr()`은 그대로 둔다.

거부(설정 검증 에러, 모두 `upstream: `으로 시작한다):

- 첫 설정이 평문이고 같은 호스트의 대체 설정이 TLS(`sslmode=allow`): `sslmode=allow is not supported: it connects in plaintext first, which a relayed session cannot retry; use disable, prefer, require, verify-ca or verify-full`
- `SSLNegotiation == "direct"`: `sslnegotiation=direct is not supported: stet asks for TLS with an SSLRequest; remove it`
- `pgconn.ParseConfig`의 에러(없는 `sslrootcert` 파일 등): 그 에러. pgconn은 주소의 비밀번호를 가린다.

`resolveTable`은 모델이 없는지(`model.adapter`가 `none`)를 받아, 그때만 `send_fields` 없는 테이블을 받는다. 모델이 있으면 에러 문구는 지금과 같다.

- [ ] **Step 1: 실패하는 테스트.** `PGSSLMODE` 등 환경 변수의 영향을 받지 않게 테스트에서 `t.Setenv`로 비운다.
  - `TestUpstreamTLSBySSLMode`: `disable` → `nil`, 대체 없음. 적지 않음과 `prefer` → 설정 있음, `UpstreamPlainFallback`, `InsecureSkipVerify`. `require` → 대체 없음, `InsecureSkipVerify`, `VerifyPeerCertificate == nil`. `require`와 `sslrootcert` → `VerifyPeerCertificate != nil`. `verify-ca` → 같다. `verify-full` → `InsecureSkipVerify`가 거짓, `ServerName`이 호스트. 호스트가 이름이면 `require`에서도 `ServerName`이 그 이름이고, IP면 비어 있고, `sslsni=0`이면 비어 있다. 인증서 파일은 테스트가 `t.TempDir()`에 만든다.
  - `TestUpstreamTLSCarriesNoClientCertificate`: `sslcert`와 `sslkey`를 준 주소 → `UpstreamTLS.Certificates`가 비고 `GetClientCertificate`가 `nil`. 같은 주소의 `pgconn.ParseConfig`에는 인증서가 하나 있다.
  - `TestUpstreamRejectsAllowAndDirect`: 위의 두 문구.
  - `TestUpstreamTransport`: 넷.
  - `TestSendFieldsOptionalWithoutAModel`: `adapter: none`에 `send_fields` 없는 테이블 → 읽힌다. 모델(`jev`, 고정 버전)에 `send_fields`가 없으면 `tables.contacts.send_fields: required: list the columns the model may see`.
  - `cmd/stet`의 `TestRecordsWithoutSendFields`(통합): `adapter: none`, `send_fields` 없음, 섀도로 띄우고 한 행을 고친다. 판단이 `commit`으로 기록되고, 기록의 `state` 안의 `proposed`와 `current`에 컬럼 값이 없고(`{}`이거나 `null`), `proposed` 컬럼에는 쓴 값이 있다.
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/config/`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Config: read upstream TLS parameters; send_fields is optional without a model`.

---

### Task 3: 프록시 — 업스트림에 TLS로 붙기

**Files:** Create `internal/proxy/upstream.go`, `internal/pgtest/tls.go`. Modify `internal/proxy/proxy.go`, `internal/proxy/export_test.go`, `cmd/stet/main.go`, `proxy.New`를 부르는 모든 곳(`internal/proxy`, `internal/gate`, `internal/lab/bench`의 테스트와 코드). Test `internal/proxy/upstream_test.go`, `internal/proxy/proxy_test.go`.

**Interfaces — Consumes:** `config.Config.UpstreamTLS`, `UpstreamPlainFallback` (Task 2).

**Interfaces — Produces:**

```go
// package proxy

// Upstream says how the proxy reaches the server.
type Upstream struct {
	Addr string // host:port
	// TLS, when set, is asked for with an SSLRequest before anything else is
	// sent to the server. nil: plaintext.
	TLS *tls.Config
	// PlainFallback lets a session go on in plaintext when the server
	// answers the SSLRequest with N (sslmode=prefer).
	PlainFallback bool
}

func New(up Upstream, log *slog.Logger, gate Gate, policy Policy) *Server

// dial connects to the server, with TLS when u asks for it. secure reports
// whether the connection it returns is TLS. An error's text is what the
// client is told after "stet: ".
func (u Upstream) dial() (conn net.Conn, secure bool, err error)

// package pgtest (used by tests only)

// ServerCert returns the certificate the test server at addr presents and
// the path of a PEM file of it in t.TempDir(). It fails the test, saying to
// start the server again with scripts/testdb.sh, when the server does not
// accept TLS.
func ServerCert(t testing.TB, addr string) (cert *x509.Certificate, pemPath string)
```

`dial`의 절차는 스펙 3장의 1~3이다. SSLRequest의 답은 연결에서 바로 한 바이트만 읽는다(버퍼 없음). SSLRequest부터 인사 끝까지 `dialTimeout`의 기한을 걸고 끝나면 지운다. `dialTimeout`은 변수로 바꾸고 `export_test.go`에 `SetDialTimeout`을 둔다. `handle`과 `forwardCancel`이 `dial`을 쓴다. `handle`은 실패하면 경고 로그를 남기고 `FATAL` `08001`을 준다. `forwardCancel`은 로그만 남긴다. `cmd/stet/main.go`는 `proxy.Upstream{Addr: cfg.UpstreamAddr(), TLS: cfg.UpstreamTLS, PlainFallback: cfg.UpstreamPlainFallback}`을 넘긴다.

에러 문구(`stet: ` 뒤):

- TCP: 지금과 같다. `cannot connect to upstream <addr>: <err>`
- 답이 `N`이고 평문을 허용하지 않음: `upstream does not accept TLS, and the sslmode of upstream needs it`
- 답이 `S`도 `N`도 아님: `upstream answered the TLS request with <%q>, not S or N`
- 인사 실패(인증서 확인 포함), 기한 넘김: `TLS to upstream <addr> failed: <err>`

- [ ] **Step 1: 실패하는 테스트.** 테스트 헬퍼 `tlsUpstream(t) proxy.Upstream`(시험 서버, `RootCAs`는 `pgtest.ServerCert`, `ServerName`은 호스트)과 `startProxyTo(t, up, gate, policy)`를 더한다. 기존 `startProxyWith(t, addr, …)`는 `proxy.Upstream{Addr: addr}`로 잇는다. 가짜 업스트림은 테스트 안의 리스너다.
  - `TestPassthroughOverTLS`, `TestWatchedPassthroughOverTLS`: `scenarios`의 모든 줄을 `tlsUpstream`으로 돌린다(인증 방식 넷, 틀린 비밀번호, 취소, COPY, 큰 결과, 알림 등).
  - `TestSessionOverTLSIsTLSOnTheServer`: 프록시를 거친 세션에서 `select ssl from pg_stat_ssl where pid = pg_backend_pid()` → 참. 평문 `Upstream`에서는 거짓.
  - `TestUpstreamDeclinesTLS`: `N`으로 답하고 그 뒤를 시험 서버로 평문 중계하는 가짜. `PlainFallback`이 거짓 → 클라이언트가 `08001`과 위의 문구를 받는다. 참 → 로그인하고 `select 1`이 된다.
  - `TestUpstreamCertificateIsChecked`: `RootCAs`가 다른 인증서 → `08001`. `ServerName`이 `wrong.example` → `08001`. `InsecureSkipVerify` → 붙는다.
  - `TestOddTLSAnswerIsRefused`: `E`로 답하는 가짜 → `08001`.
  - `TestSSLAnswerIsReadAlone`: `S`와 평문 AuthenticationOk·ReadyForQuery를 한 번에 써 보내는 가짜 → 클라이언트는 `08001`을 받고, AuthenticationOk를 받지 않는다.
  - `TestSilentUpstreamEndsOnlyThatSession`: SSLRequest를 받고 답하지 않는 가짜와 인사 도중에 끊는 가짜(`SetDialTimeout`으로 기한을 줄인다) → 그 세션이 `08001`로 끝나고, 같은 프록시에 붙는 다음 세션도 같은 답을 제때 받는다(프록시가 서지 않는다). 프록시를 닫으면 `Close`가 돌아온다.
  - `TestCancelOverTLSNeedsTLS`: `PlainFallback`이 거짓인데 `N`으로 답하는 가짜에 취소 요청을 보내면 가짜가 CancelRequest를 받지 않는다.
- [ ] **Step 2:** 돌려서 실패를 본다. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'TLS|Upstream|SSL'`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령과 `latency_test.go`가 통과한다.
- [ ] **Step 5:** 커밋 `Proxy: reach upstream over TLS`.

---

### Task 4: 프록시 — 인증 방식 목록

**Files:** Create `internal/proxy/auth.go`. Modify `internal/proxy/proxy.go`. Test `internal/proxy/auth_internal_test.go`, `internal/proxy/upstream_test.go`, `internal/proxy/latency_test.go`.

**Interfaces — Consumes:** `Upstream.dial`의 `secure`, `tlsUpstream`, `startProxyTo` (Task 3).

**Interfaces — Produces:**

```go
// stripPlus looks at the body of an Authentication message. When it is an
// AuthenticationSASL (code 10) that lists SCRAM-SHA-256-PLUS, replace is the
// whole message (type, length, body) with the same list without it, and left
// is how many mechanisms remain. changed false: the message is forwarded as
// the server sent it. A body it cannot read is not changed.
func stripPlus(body []byte) (replace []byte, left int, changed bool)

// authTap returns the server-side tap of a session whose client leg is
// plaintext and whose upstream leg is TLS: inner (nil for a session that is
// only relayed) and, until the server's first ReadyForQuery, a look at each
// Authentication message. At that ReadyForQuery, once inner has seen it, the
// tap's fields become inner's (all nil without one), so nothing is added per
// message after authentication. refused returns the FATAL to give the client
// when the tap ended the session, else nil.
func authTap(inner *tap) (t *tap, refused func() []byte)
```

`pump`는 `want`가 `nil`인 탭을 아무것도 원하지 않는 탭으로 본다. `handle`은 `secure`일 때만 서버 쪽 탭을 `authTap`으로 싸고, `relay`의 `ended`는 `refused()`가 있으면 그것을, 없으면 세션의 것을 준다. 보는 세션이든 아니든, 섀도든 enforce든 같다. 평문 업스트림의 경로는 한 글자도 바뀌지 않는다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestStripPlus`: `[SCRAM-SHA-256-PLUS, SCRAM-SHA-256]` → `[SCRAM-SHA-256]`, 남은 것 1. `[SCRAM-SHA-256]` → 바뀌지 않음. `[SCRAM-SHA-256-PLUS]` → 남은 것 0. `[SCRAM-SHA-256-PLUS, OAUTHBEARER, SCRAM-SHA-256]` → 순서 그대로 둘. 코드 0, 3, 5, 11, 12 → 바뀌지 않음. 깨진 본문(4바이트 미만, 끝의 0이 없음, 이름 뒤 0이 없음) → 바뀌지 않음. 바뀐 메시지의 길이 칸이 본문과 맞는다.
  - `TestAuthTapBecomesInner`: `authTap(nil)`은 `Z`를 본 뒤 `want`, `see`, `each`가 모두 `nil`이다. `authTap(inner)`는 `Z`를 `inner.see`에 한 번 넘기고, 그 뒤에는 `R`을 원하지 않으며 `inner`가 원하는 것만 원한다. `Z` 전에는 `inner`가 원하는 메시지와 `each`가 그대로 `inner`에 간다.
  - `TestSASLListOverTLSHasNoPlus`(날 프로토콜 클라이언트): 시험 서버에 TLS로 직접 붙어 `stet_scram`으로 시작하면 첫 메시지의 목록에 `SCRAM-SHA-256-PLUS`가 있다(이것이 없으면 테스트는 실패한다). `tlsUpstream` 프록시를 거치면 목록이 `[SCRAM-SHA-256]`이다.
  - `TestSASLListOverPlaintextIsTheServers`: 평문 `Upstream`의 프록시를 거쳐 받은 첫 메시지의 바이트가 평문으로 직접 받은 것과 같다.
  - `TestOnlyChannelBoundAuthIsRefused`: TLS를 받고 `[SCRAM-SHA-256-PLUS]`만 제안하는 가짜 → 클라이언트가 `FATAL` `28000`, `stet: the server offers only channel-bound authentication, which cannot pass through stet`를 받는다. 게이트가 없을 때와 모든 세션을 보는 게이트가 있을 때 둘 다.
  - `TestPsqlLogsInThroughTLSUpstream`: `psql`(`PATH`에 없으면 그 사실을 말하며 건너뛴다)이 `sslmode=disable`로 프록시를 거쳐 `stet_scram`, `stet_md5`, `stet_clear`, `stet_trust`로 `select 1`을 한다. 비밀번호는 `PGPASSWORD`로 준다. 틀린 비밀번호는 `password authentication failed for user "stet_scram"`이 표준 에러에 온다.
  - `TestTLSUpstreamLatencyBudget`(`latency_test.go`에 새 함수와, 필요하면 새 헬퍼. 기존 함수는 그대로): 직접은 TLS로 서버에, 프록시는 평문으로 `tlsUpstream` 프록시에. 더해진 p95가 2ms 이하다. 값을 로그에 낸다.
- [ ] **Step 2:** 돌려서 실패를 본다. `psql` 테스트는 `-v`로 돌려 건너뛰지 않았음을 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령과 `latency_test.go`가 통과한다. 보고서에 `TestTLSUpstreamLatencyBudget`의 로그 줄을 적는다.
- [ ] **Step 5:** 커밋 `Proxy: drop SCRAM-SHA-256-PLUS from the mechanisms a plaintext client is offered`.

---

### Task 5: `stet proxy` — 시작할 때의 경고와 끝에서 끝까지의 TLS

**Files:** Create `cmd/stet/transport.go`. Modify `cmd/stet/main.go`. Test `cmd/stet/transport_test.go`, `cmd/stet/tls_test.go`.

**Interfaces — Consumes:** `config.Config.UpstreamTransport()` (Task 2), `pgtest.ServerCert` (Task 3).

**Interfaces — Produces:**

```go
// loopbackHost reports whether host is this machine: "localhost" or a
// loopback IP address. An empty or unspecified host (0.0.0.0, ::) is not.
func loopbackHost(host string) bool

// transportWarnings returns what stet warns of at start about the two legs
// (spec-v0.2-tryout 3). It stops nothing.
func transportWarnings(cfg *config.Config) []string
```

`run`은 `stet proxy listening`을 적기 전에 이 경고들을 `log.Warn`으로 낸다. 게이트가 있든 없든 낸다. 문구(`<host>`와 `<listen>`만 채운다. 주소 전체를 넣지 않는다):

1. 업스트림 호스트가 루프백이 아니고 `TransportPlain`이나 `TransportPreferred`: `upstream <host> is not this machine and its sslmode allows plaintext: statements, results and "password"-method passwords can cross the network unencrypted; fix: add sslmode=verify-full to upstream (and sslrootcert=<CA file> when the server's CA is not a public one)`
2. 루프백이 아니고 `TransportUnverified`: `upstream <host> is not this machine and its certificate is not checked (sslmode=require without sslrootcert): the connection is encrypted, but to whoever answers; fix: use sslmode=verify-full (and sslrootcert=<CA file> when the server's CA is not a public one)`
3. `listen`의 호스트가 루프백이 아님: `listen <listen> is not a loopback address: the connection between clients and stet is not encrypted; fix: listen on 127.0.0.1, or keep clients and stet on a network you trust`

- [ ] **Step 1: 실패하는 테스트.**
  - `TestLoopbackHost`: `localhost`, `127.0.0.1`, `127.0.0.2`, `::1` → 참. `0.0.0.0`, `::`, 빈 글자, `db.example.com`, `10.0.0.5` → 거짓.
  - `TestTransportWarnings`: 조합마다 어느 경고가 나오는지. 루프백 업스트림과 루프백 `listen`은 어떤 `sslmode`든 경고가 없다. 원격의 `verify-full`은 경고가 없다.
  - `TestTransportWarningsNameNoPassword`: 비밀번호가 든 주소에서 나온 모든 경고에 그 비밀번호가 없다.
  - `TestSessionsReachTheServerOverTLS`(통합): `sslmode`가 `require`, `verify-ca`, `verify-full`(`sslrootcert`는 `pgtest.ServerCert`의 파일)인 `upstream`으로 띄운다. 테이블 하나를 지킨다(`adapter: none`, `impact.max_rows`). pgx와 `psql`이 평문으로 Stet에 붙어 `stet_scram`으로 `pg_stat_ssl`이 참임을 보고, 한 행을 고친 쓰기가 `stet.decisions`에 기록된다. 로그에 경고가 없다.
  - `TestRelayedSessionShowsNoClientCertificate`(통합): 클라이언트 인증서를 청하는(`tls.RequestClientCert`) 가짜 TLS 업스트림. `upstream`에 `sslmode=require`, `sslcert`, `sslkey`(테스트가 만든다)를 주고 테이블 없이 띄운다. 세션 하나를 열면 가짜가 본 인사에 클라이언트 인증서가 없다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `stet proxy: warn about unencrypted and unverified legs`.

---

### Task 6: 판단 한 줄 — 만들기와 쓰기

**Files:** Create `internal/lines/format.go`, `internal/lines/writer.go`. Test `internal/lines/format_test.go`, `internal/lines/writer_test.go`. DB가 필요 없다.

**Interfaces — Produces:**

```go
// Package lines turns recorded decisions into the lines stet proxy prints.
package lines

// Format returns the line for d, without a newline, its time in loc. It
// reads only what d holds, and never fails: what it cannot read it leaves out.
func Format(d *store.Decision, loc *time.Location) string

const QueueSize = 1024

// Writer writes lines to w from a queue of QueueSize, on its own goroutine.
type Writer struct{ /* … */ }

func NewWriter(w io.Writer) *Writer

// Offer queues line. It never blocks: with the queue full the line is
// dropped and counted, and "… N lines dropped" is written once there is room.
func (w *Writer) Offer(line string)

// Close writes what is queued and stops. It gives up after 2 seconds when w
// does not take it.
func (w *Writer) Close()
```

**줄의 모양.** 칸은 `시각`, `판정`, `OP 테이블`, `사실들`, `[이유들]`, `문장`이고 사이는 공백 둘이다. 빈 칸은 그 앞의 공백과 함께 뺀다.

- 시각: `d.CreatedAt`을 `loc`에서 `15:04:05`.
- 판정: `d.Mode == "shadow"`면 `commit`→`ok`, `hold`→`would hold`, `escalate`→`would escalate`, `reject`→`would reject`, `unsupported`→`unsupported`. 그 밖의 모드면 `ok`, `hold`, `escalate`, `reject`, `unsupported`. 모르는 판정은 그 글자 그대로. `%-12s`로 채운다(넘으면 자르지 않는다).
- OP 테이블: `d.Op`을 대문자로, 공백 하나, `d.TargetTable`. 빈 것은 뺀다.
- 사실들: 쉼표와 공백으로 잇는다. 순서는 행 수, 비율, 연쇄, 읽기 확인, 실행이다.
  - 행 수와 그 수 `n`: `d.RowsAffected`가 있으면 `n rows`. 없고 `d.Impact`(`*decide.Impact`나 `decide.Impact`)가 있으면 보정 P3대로. 하나면 `1 row`, `~1 row`.
  - 비율: `n`이 0보다 크고 `Impact.TableRows`가 0보다 크면 `~P% of table`. `P`는 반올림한 정수이고 100을 넘으면 100, 0으로 내려가면 `<1% of table`. `N+ rows`일 때는 뺀다.
  - 연쇄: `cascades into deals, notes`. `Impact.Cascades`의 테이블 이름을 순서대로, 같은 이름은 한 번만.
  - 읽기 확인: `d.ReadCheck`(`*decide.ReadCheck`나 값)의 결과가 `fresh`면 `fresh read`, `stale`이면 `stale read`.
  - 실행: `d.Refused`가 있으면 `refused <코드>`. 없고 `d.Executed`가 거짓이면 `not executed`.
- 이유들: `d.Reasons`를 쉼표와 공백으로 이어 대괄호에.
- 문장: 보정 P5의 글자와 모든 공백 문자의 연속을 공백 하나로 바꾸고 양끝을 다듬는다. 80자(룬)를 넘으면 79자와 `…`.

- [ ] **Step 1: 실패하는 테스트.** `CreatedAt`은 UTC 14:02:11, `loc`은 `time.UTC`.
  - `TestFormatLines`, 줄마다 정확한 글자:
    - 섀도 `commit`, `update`, `contacts`, `RowsAffected` 1, `ReadCheck` `fresh`, 실행됨 → `14:02:11  ok            UPDATE contacts  1 row, fresh read  update contacts set status='customer' where id = 7`
    - 섀도 `hold`, `RowsAffected` 300, `Impact{TableRows: 1000}`, 이유 `impact:rows` → `14:02:11  would hold    UPDATE contacts  300 rows, ~30% of table  [impact:rows]  update contacts set status='churned' where company_id < 3`
    - 섀도 `hold`, `delete`, `RowsAffected` 200, `TableRows` 1000, 연쇄 `deals` → `14:02:11  would hold    DELETE contacts  200 rows, ~20% of table, cascades into deals  [impact:rows]  delete from contacts where id between 1 and 200`
    - enforce `hold`, `Refused` `ST001`, `Impact{Method: "count", Rows: 101, Capped: true, Cap: 101, Estimate: 257, TableRows: 1000}` → 사실들이 `~257 rows, ~26% of table, refused ST001`, 판정 칸이 `hold        `.
    - 같은 것에 `Estimate`가 없으면 `101+ rows, refused ST001`.
    - `Impact{Method: "estimate", Rows: 40, TableRows: 100000}`, 거부됨 → `~40 rows, <1% of table, refused ST002`(코드는 준 대로).
    - `Impact{Method: "count", Rows: 12}`, 거부됨 → `12 rows, refused ST001`.
    - 섀도 `reject`, `ReadCheck` `stale`, 이유 `stale_read` → `14:02:11  would reject  UPDATE contacts  1 row, stale read  [stale_read]  …`. 섀도 `escalate` → 판정 칸 `would escalate`(14글자, 뒤에 공백 둘).
    - 보냈지만 실패(`Executed` 거짓, `Refused` 없음, `RowsAffected` 없음, `Impact` 없음) → 사실들이 `not executed`.
    - `unsupported`, 이유 `multi_statement`, `Op`과 테이블이 비어 있음 → `14:02:11  unsupported   [multi_statement]  <문장>`.
    - 사실도 이유도 없으면 `시각  판정  OP 테이블  문장`.
    - `TableRows` 0이나 없음 → 비율 없음. 행 수가 테이블보다 크면 `~100% of table`.
  - `TestFormatCutsTheStatement`: 줄바꿈·탭·겹친 공백이 든 문장이 한 줄이 된다. 정확히 80룬은 그대로, 81룬은 79룬과 `…`. 한글이 든 문장도 룬으로 센다.
  - `TestLineHasNoControlCharacters`: 문장에 `\x1b[2J`, `\r`, `\b`, `\n14:00:00  ok  …`(가짜 줄), U+202E, U+2028이 있어도 결과에 제어 문자와 그 글자들이 하나도 없고 줄바꿈이 없다. 테이블 이름, 이유, 연쇄 테이블 이름에 넣어도 같다.
  - `TestFormatIgnoresWhatItCannotRead`: `Impact`와 `ReadCheck`가 다른 타입(`map[string]any`, 글자)이어도 패닉 없이 그 사실만 빠진다. `d`의 모든 포인터가 `nil`이어도 된다.
  - `TestWriterWritesInOrder`: 넣은 줄이 줄바꿈과 함께 순서대로 나온다.
  - `TestWriterDropsWhenFull`: 쓰기가 막힌 `io.Writer`. 한 줄을 넣어 쓰는 쪽이 막히게 하고, `QueueSize`줄을 더 넣고, 37줄을 더 넣는다. 막힌 것을 풀면 앞의 `1+QueueSize`줄, 그다음 `… 37 lines dropped`, 그 뒤에 넣은 줄이 차례로 나온다. 하나만 버렸으면 `… 1 line dropped`.
  - `TestOfferNeverBlocks`: 쓰기가 막힌 채 `Offer` 5,000번이 1초 안에 돌아온다.
  - `TestCloseGivesUpOnABlockedWriter`: 쓰기가 막힌 채 `Close`가 3초 안에 돌아온다. 막히지 않았으면 대기열의 줄이 모두 나온 뒤에 돌아온다.
  - `TestWriterSurvivesWriteErrors`: 에러를 돌려주는 `io.Writer`에서도 `Offer`와 `Close`가 돌아온다.
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/lines/`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** `go test -race ./internal/lines/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Lines: one readable line per recorded decision`.

---

### Task 7: 게이트의 알림 자리와 `stet proxy -decisions`

**Files:** Modify `internal/gate/gate.go`, `cmd/stet/main.go`. Test `internal/gate/gate_test.go`, `cmd/stet/lines_test.go`.

**Interfaces — Consumes:** `lines.Format`, `lines.NewWriter`, `Writer.Offer`, `Writer.Close` (Task 6).

**Interfaces — Produces:**

```go
// in gate.Gate:

// Decided, when set, is called with each decision once the gate has tried to
// record it, whether or not the insert succeeded. Set it before the gate is
// handed to the proxy. It runs where the write was judged, so it must not
// block, and it must not keep or change d.
Decided func(d *store.Decision)

// cmd/stet

// decisionsOn reports whether stet proxy prints decision lines: flag is
// auto, on or off; auto means only when out is a character device.
func decisionsOn(flag string, out *os.File) (bool, error)

// runWith is run with decision lines written to out; nil: none.
func runWith(ctx context.Context, cfg *config.Config, ln net.Listener, log *slog.Logger, out io.Writer) error
```

`judge`는 `g.store.Insert`를 시도한 바로 뒤, `settleHeld` 앞에서 `Decided`를 부른다. `run(ctx, cfg, ln, log)`는 시그니처를 그대로 두고 `runWith(…, nil)`을 부른다. `runProxy`는 `-decisions`(기본 `auto`)를 받고, 다른 값이면 `-decisions: "x" must be auto, on or off`로 끝낸다. `out`이 있으면: 받기 시작할 때 `stet: watching <N> tables on <listen> (shadow: nothing is blocked)`(enforce면 `(enforce)`, 하나면 `1 table`)를 `out`에 바로 쓰고, `Decided`를 `w.Offer(lines.Format(d, time.Local))`로 잇고, 게이트를 닫은 뒤에 `Writer`를 닫는다. 로그는 지금처럼 표준 에러다.

- [ ] **Step 1: 실패하는 테스트.**
  - `internal/gate`의 `TestDecidedIsToldOfEachDecision`: 쓰기 둘 → `Decided`가 두 번, 기록된 것과 같은 `ID`·`Verdict`로. `Decided`가 `nil`이면 지금과 같다.
  - `TestDecisionsFlag`: `on`→참, `off`→거짓, `auto`와 보통 파일·파이프→거짓, 다른 값→에러.
  - `TestDecisionLinePerJudgedWrite`(통합): 자기 데이터베이스에 `contacts` 1,000행과 그것을 `ON DELETE CASCADE`로 가리키는 `deals`를 만들고 `ANALYZE`한다. `adapter: none`, `impact.max_rows: 100`, `reads`, `stale`, 섀도, `out`은 버퍼. 한 행을 읽고 고치기, 300행 고치기, 200행 지우기 → `out`의 첫 줄이 `stet: watching 1 table on <addr> (shadow: nothing is blocked)`이고, 이어서 세 줄이 `ok            UPDATE contacts  1 row, fresh read  …`, `would hold    UPDATE contacts  300 rows, ~30% of table  [impact:rows]  …`, `would hold    DELETE contacts  200 rows, ~20% of table, cascades into deals  [impact:rows]  …`로(시각만 정규식) 나온다. `run`(`out` 없음)으로 띄우면 아무것도 쓰지 않는다.
  - `TestEnforceLineShowsTheRefusal`(통합): enforce에서 한도를 넘는 쓰기 → 줄의 판정이 `hold`이고 사실에 `refused ST001`이 있다. 시작 줄이 `(enforce)`로 끝난다.
  - `TestLineWhenTheRecordFails`(통합, 자기 데이터베이스): 띄운 뒤 superuser로 `stet.decisions`의 이름을 바꾸고 쓰기 하나 → 줄이 나오고 로그에 `decision not recorded`가 있다.
  - `TestBlockedOutputDoesNotHoldWrites`(통합): `out`이 영원히 막힌 `io.Writer`. 프록시를 거친 쓰기 2,000개가 모두 끝나고 기록된다. 닫을 때 `runWith`가 10초 안에 돌아온다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. `usage`에 `-decisions`를 적는다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `stet proxy: print a line per judged write`.

---

### Task 8: 설치 산출물 — `stet version`, 묶음, 이미지, 워크플로

**Files:** Create `cmd/stet/version.go`, `scripts/release.sh`, `Dockerfile`, `.dockerignore`, `.github/workflows/release.yml`. Modify `cmd/stet/main.go`. Test `cmd/stet/version_test.go`.

**Interfaces — Produces:**

```go
// Set at build time with -ldflags "-X main.version=… -X main.commit=…".
var version, commit = "dev", ""

// versionLine is what stet version prints:
// "stet <version> (commit <commit>, <go version>)". Without a commit set at
// build time it uses the build's vcs.revision (12 characters), or "unknown".
func versionLine() string
```

- **`scripts/release.sh <버전>`.** 버전은 `^[0-9]+\.[0-9]+\.[0-9]+([-.][0-9A-Za-z.]+)?$`만 받는다(앞의 `v` 없음). `CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=<버전> -X main.commit=<git rev-parse --short=12 HEAD>"`. `dist/stet_<버전>_<os>_<arch>.tar.gz` 안에 디렉터리 `stet_<버전>_<os>_<arch>/` 하나가 있고 그 안에 `stet`, `LICENSE`, `README.md`, `stet.example.yaml`이 있다. `os`와 `arch`는 `go env GOOS GOARCH`다. `dist/checksums.txt`에 `<sha256>  <파일 이름>` 줄을 넣는다. 같은 파일의 줄이 있으면 바꾸고 다른 줄은 둔다. `shasum -a 256`이 없으면 `sha256sum`을 쓴다.
- **`Dockerfile`.** 빌드 단계 `golang:1.27-bookworm`, 실행 단계 `gcr.io/distroless/base-debian12:nonroot`. 두 단계의 Debian 판이 같아야 한다(빌드 쪽 glibc가 더 새면 실행되지 않는다). 그 태그가 없으면 둘 다 한 판 올린 짝(`-trixie`, `base-debian13`)을 쓴다. 빌드 인자 `VERSION`(기본 `dev`), `COMMIT`. `ENTRYPOINT ["/usr/local/bin/stet"]`, `EXPOSE 6432`. `.dockerignore`는 `.git`, `bin`, `dist`, `.superpowers`를 뺀다.
- **`.github/workflows/release.yml`.** `on: workflow_dispatch`(입력 `version`)와 `push: tags: ['v*']`. `permissions: contents: read`. 행렬: `macos-15`, `macos-15-intel`, `ubuntu-24.04`, `ubuntu-24.04-arm`. 단계: checkout, setup-go(`go-version-file: go.mod`), `go test ./...`(통합 테스트는 건너뛴다), 버전은 입력이나 태그에서 `v`를 뗀 것, `scripts/release.sh`, `dist/*`를 `actions/upload-artifact`로 올린다. 릴리스를 만들거나 이미지를 올리는 단계는 없다.

- [ ] **Step 1: 실패하는 테스트.** `TestVersionLine`: `version`과 `commit`을 바꿔 가며 `stet 0.2.0 (commit abcdef123456, go1.…)`, 기본값이면 `stet dev (commit `으로 시작한다. `stet version`이 그 줄을 내고 0으로 끝난다. `usage`에 `version`이 있다.
- [ ] **Step 2:** 돌려서 실패를 보고 구현한다.
- [ ] **Step 3: 묶음 확인.** `scripts/release.sh 0.2.0-test` → 묶음을 임시 디렉터리에 풀어 `./stet_0.2.0-test_<os>_<arch>/stet version`이 `stet 0.2.0-test (commit `으로 시작한다. 네 파일이 있다. `shasum -a 256 -c dist/checksums.txt`가 통과한다. 한 번 더 돌려도 `checksums.txt`에 그 파일의 줄이 하나다. `scripts/release.sh v1`과 인자 없는 호출은 0이 아닌 값으로 끝난다. `git status`에 `dist/`가 보이지 않는다. 끝나면 `dist/`를 지운다.
- [ ] **Step 4: 이미지 확인.** `docker build --build-arg VERSION=0.2.0-test -t stet:tryout-test .` → `docker run --rm stet:tryout-test version`이 `stet 0.2.0-test`로 시작한다. `docker run --rm --entrypoint id stet:tryout-test`가 안 되면(쉘이 없는 이미지) `docker inspect -f '{{.Config.User}}' stet:tryout-test`가 root가 아니다. 이어서 이미지를 띄워 쓰기를 판단시킨다: `stet_test`에 `stet_scram`으로 이름이 겹치지 않는 테이블 하나를 만들고, 임시 설정(`listen: 0.0.0.0:6432`, `upstream`의 호스트는 `host.docker.internal:55432`, `adapter: none`, 그 테이블에 `impact.max_rows`)을 읽기 전용으로 마운트해 `-p 127.0.0.1:16432:6432`로 띄운다(Linux에서는 `--add-host host.docker.internal:host-gateway`를 더한다). `127.0.0.1:16432`로 `psql`이 붙어 한 행을 고치고, `stet.decisions`에 그 테이블의 줄이 생겼는지 본다. 컨테이너가 55432에 붙지 못하면(접속 규칙이 `127.0.0.1`만 받는다) 임시 디렉터리에 55440의 일회용 서버를 띄우고 그 서버의 `pg_hba.conf`에 줄을 더해 쓴다. 55432의 서버 설정은 건드리지 않는다. 끝나면 컨테이너, 이미지 `stet:tryout-test`, 테이블, 임시 파일, 일회용 서버를 지운다. 보고서에 명령과 출력을 적는다.
- [ ] **Step 5: 워크플로 확인.** YAML로 읽히는지만 본다(`go run`으로 `go.yaml.in/yaml/v3`에 읽히거나 `ruby -ryaml`). 돌려 보지 못했다고 보고서에 적는다.
- [ ] **Step 6:** 전체 명령이 통과한다. 커밋 `Add stet version, the release bundle script, a Dockerfile and a build workflow`.

---

### Task 9: `stet init` — DB에서 읽는 것

**Files:** Create `cmd/stet/initdb.go`. Modify `internal/gate/stale_catalog.go`. Test `cmd/stet/initdb_test.go`, `internal/gate/stale_catalog_test.go`.

**Interfaces — Consumes:** `openPool`, `readOnly`, `loopbackHost` (Task 5).

**Interfaces — Produces:**

```go
// package gate

// TracksKeyType reports whether stale-write detection tracks a table whose
// key column has base type oid (keyTypes).
func TracksKeyType(oid uint32) bool

// cmd/stet

// maxInitTables is how many tables stet init writes. A variable for tests.
var maxInitTables = 100

// remoteHost reports whether host is another machine. A variable so a test
// can have the test server, which is on this machine, taken for a remote one.
var remoteHost = func(host string) bool { return !loopbackHost(host) }

// chooseSSLMode returns the upstream address stet init writes for the
// address raw it was given, having connected with it through connect, and a
// notice for the user when it settled for less than verify-full ("" else).
func chooseSSLMode(ctx context.Context, raw string, connect func(ctx context.Context, addr string) error) (upstream, notice string, err error)

type initTable struct {
	Name, PK string
	Stale    bool    // the key column's base type is one stale tracks
	Rows     float64 // pg_class.reltuples; -1: never analyzed
}

type initFacts struct {
	VersionNum     int    // server_version_num
	Version        string // server_version
	User, Database string
	SchemaExists   bool // the stet schema is there
	CanCreate      bool // User has CREATE on Database
	Tables         []initTable    // chosen, sorted by name
	Skipped        map[string]int // reason → how many tables
	Dropped        int            // chosen but over maxInitTables
}

// readInitFacts reads, in one read-only transaction, what stet init needs.
// only, when not empty, names the tables to take; a name that cannot be
// taken is an error that says why, all of them in one error.
func readInitFacts(ctx context.Context, pool *pgxpool.Pool, only []string) (*initFacts, error)
```

**`chooseSSLMode`** (스펙 4장 1). `postgres://`, `postgresql://`가 아니면 `the database address must start with postgres:// or postgresql://`. `sslmode`가 있으면 그대로 한 번 붙어 본다(`allow`는 Task 2의 문구로 거부). 없고 호스트가 루프백이면 그대로 한 번. 없고 원격이면 `sslmode=verify-full`을 더해 붙어 본다. 인증서 확인 에러(`*tls.CertificateVerificationError`, `x509.UnknownAuthorityError`, `x509.HostnameError`, `x509.CertificateInvalidError`를 `errors.As`로)면 `sslmode=require`로 다시 붙고, 되면 알림 `the server's certificate could not be checked (<에러>), so upstream uses sslmode=require: the connection is encrypted, but the server is not verified. To verify it, add sslrootcert=<the service's CA file> and sslmode=verify-full to upstream in the config file.`을 돌려준다. 서버가 TLS를 거절했으면(pgconn의 `server refused TLS connection`) `the server does not accept TLS; to connect in plaintext anyway, write sslmode=disable in the address yourself`. 그 밖의 접속 에러는 서버의 문구 뒤에 줄을 바꿔 `if this address goes through a connection pooler, use the direct connection address instead`를 붙인다. `sslmode`는 주소의 다른 글자를 바꾸지 않고 질의 끝에 더한다. 어떤 에러와 알림에도 비밀번호가 없다.

**`readInitFacts`** (스펙 4장 2~3, 보정 P4). 후보는 `relkind`가 `r`이나 `p`이고 `relispartition`이 아니며, 스키마가 `pg_catalog`, `information_schema`, `pg_toast`, `stet`, 임시 스키마가 아닌 것이다. 고르는 조건과 빼는 이유(`Skipped`의 키, `-tables` 에러의 글자)는 다음 순서로 처음 걸리는 것이다.

1. `to_regclass(quote_ident(relname))`이 그 테이블이 아니다 → `not reached by search_path`
2. 이름이 `^[a-z_][a-z0-9_]*$`가 아니다 → `name needs quoting`
3. 유효한 기본 키가 없거나 컬럼이 둘 이상이다 → `no single-column primary key`
4. 키 컬럼 이름이 같은 글자 조건이 아니다 → `key column name needs quoting`
5. `has_table_privilege(oid, 'SELECT')`가 거짓이다 → `not readable by this account`

`Stale`은 키 컬럼의 타입에서 도메인을 걷어 낸 바탕 타입(`catalogQuery`의 `base`와 같은 방식)이 `TracksKeyType`일 때다. 고른 것이 `maxInitTables`를 넘으면 `reltuples`가 큰 순서(같으면 이름 순서)로 그만큼만 두고 나머지 수를 `Dropped`에 적는다. `only`가 있으면 그 이름들만 보고, 조건에 걸리거나 없는(`not found`) 이름을 `table "x": <이유>` 줄로 모두 모아 에러로 돌려준다. 이때 상한은 적용하지 않는다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestTracksKeyType`: `int4`(23), `uuid`(2950), `text`(25) → 참. `date`(1082), `numeric`(1700) → 거짓.
  - `TestChooseSSLMode`(가짜 `connect`. 원격인지는 주소의 호스트를 `remoteHost`에 물어 정한다): 위 규칙의 줄마다. 원격에서 `verify-full`이 되면 주소가 `…?sslmode=verify-full`(이미 질의가 있으면 `&sslmode=verify-full`)이고 `connect`가 한 번 불린다. 확인 에러 뒤 `require`가 되면 두 번 불리고 알림이 있다. 비밀번호 `s3cr3t-pw`가 든 주소의 모든 에러와 알림에 그 글자가 없다.
  - `TestChooseSSLModeAgainstTheTestServer`(통합): 시험 서버를 원격으로 보게 하면(테스트가 `remoteHost`를 바꾼다) 인증서가 시스템의 루트에 없으므로 `require`로 내려가고 알림이 온다. `sslrootcert`와 `sslmode=verify-full`을 직접 적으면 그대로 붙는다.
  - `TestReadInitFacts`(통합, 자기 데이터베이스와 자기 역할): 다음을 만들고 결과를 본다. 정수 키 테이블 → 골라지고 `Stale`. `date` 키 → 골라지고 `Stale`이 거짓. `int4` 위의 도메인 키 → `Stale`. 복합 키와 키 없는 테이블 → `no single-column primary key`. 다른 역할의 것이고 권한이 없는 테이블 → `not readable by this account`. `"Mixed"` → `name needs quoting`. 키 컬럼 `"Id"` → `key column name needs quoting`. `search_path` 밖 스키마의 테이블 → `not reached by search_path`. 두 스키마에 같은 이름 → 앞의 것만 한 번. 파티션을 나눈 테이블 → 부모만 골라지고 파티션은 어디에도 세지 않는다. 뷰, 구체화된 뷰, 시퀀스 → 세지 않는다. 테이블 `user` → 골라진다. `VersionNum`이 140000 이상, `User`·`Database`가 맞고, `stet` 스키마가 없으면 `SchemaExists`가 거짓이다. 데이터베이스의 소유자가 아닌 역할은 `CanCreate`가 거짓이다.
  - `TestReadInitFactsCap`(통합): `maxInitTables`를 3으로 두고 행 수가 다른 테이블 다섯(`ANALYZE`) → 큰 셋이 이름 순서로, `Dropped`가 2.
  - `TestReadInitFactsOnly`(통합): `only`에 맞는 이름 둘 → 그 둘만. 없는 이름과 복합 키 테이블을 섞으면 에러에 `table "nope": not found`와 `table "pairs": no single-column primary key`가 함께 있다.
  - `TestReadInitFactsWritesNothing`(통합): 부른 뒤에도 `stet` 스키마가 없다. 읽기 전용 트랜잭션에서 돈다(`SHOW transaction_read_only`를 같은 트랜잭션에서 읽는 자리를 테스트가 볼 수 있게 한다).
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `stet init: read what the database says about its tables`.

---

### Task 10: `stet init` — 묻고, 쓰고, 알린다

**Files:** Create `cmd/stet/init.go`. Modify `cmd/stet/main.go`. Test `cmd/stet/init_test.go`.

**Interfaces — Consumes:** `chooseSSLMode`, `readInitFacts`, `initFacts`, `initTable` (Task 9), `version` (Task 8), `openPool`.

**Interfaces — Produces:**

```go
// runInit is stet init. env reads an environment variable; interactive says
// whether stdin is a terminal it can ask on.
func runInit(args []string, stdin io.Reader, stdout io.Writer, interactive bool, env func(string) string) error

// renderConfig returns the config file stet init writes.
func renderConfig(upstream, listen string, tables []initTable, version string, day time.Time) ([]byte, error)

// agentURL returns the address to give the agent: upstream's, at listen,
// without TLS and channel-binding parameters, with sslmode=disable and the
// password shown as <password>.
func agentURL(upstream, listen string) (string, error)
```

**순서.** ① 플래그(`-config stet.yaml`, `-listen 127.0.0.1:6432`, `-tables a,b,c`, `-yes`, `-force`)와 주소(마지막 인자 하나, 없으면 `DATABASE_URL`)를 읽는다. 주소가 없으면 `no database address: set DATABASE_URL, or pass the address as the last argument`. 주소 뒤에 인자가 더 있으면 `flags go before the address`. `-listen`은 `net.SplitHostPort`로 읽혀야 한다. ② `-config`의 파일이 이미 있고 `-force`가 없으면 붙기 전에 `<경로> already exists; pass -force to overwrite it`로 끝낸다. ③ `chooseSSLMode`(`connect`는 `openPool`로 붙었다 닫는 것). 알림이 있으면 낸다. ④ `openPool`과 `readInitFacts`. `VersionNum < 140000`이면 `PostgreSQL 14 or newer is needed; this server is <Version>`. `stet` 스키마가 없고 `CanCreate`가 거짓이면 `the stet schema is not there and "<User>" may not create it; as the database's owner run: GRANT CREATE ON DATABASE <Database> TO <User>;`(이름은 인용 규칙대로). 고른 테이블이 없으면 `no table can be watched`와 이유별 수. ⑤ 찾은 것을 알린다: 고른 수, 이유별로 뺀 수, `Dropped`가 있으면 그 수와 `-tables`로 고르는 법. ⑥ 스키마가 없으면 아래 안내를 내고, `-yes`가 없으면 묻는다. `interactive`가 거짓이면 묻지 않고 `this is not a terminal, so stet init cannot ask; read the lines above and pass -yes to continue`로 끝낸다. `y`나 `yes`(대소문자 무관)만 "예"다. EOF와 그 밖의 답은 `stopped; nothing was written`으로 끝낸다. ⑦ 파일을 쓴다. 같은 디렉터리의 임시 파일(`0600`)에 다 쓰고 이름을 바꾼다. `-force`가 없으면 그 사이에 생긴 파일을 덮어쓰지 않는다. ⑧ 다음 할 일을 알린다.

**안내** (⑥):

```
stet proxy will create one schema, "stet", in database "<Database>" as "<User>" the first time it starts.
It changes nothing else. To remove it later: DROP SCHEMA stet CASCADE;
Continue? [y/N] 
```

**마지막 출력** (⑧):

```
wrote <경로>: <N> tables, shadow mode, no model

next:
  stet proxy -config <경로>

then give the agent this address instead of the database's. The user and password are the same as before:
  <agentURL>
```

**쓰는 파일** (스펙 4장 "쓰는 설정"). `yaml.Node`로 만들어 YAML이 달리 읽을 글자(테이블 이름 `null`·`true`·`on`, 주소의 특수 문자)는 인코더가 따옴표를 치게 한다. 테이블은 이름 순서다. 테이블마다 `pk`, `impact.max_rows: 100`, 값 없는 `reads`, 그리고 `Stale`이면 값 없는 `stale`. 머리 주석:

```
# Written by stet init (stet <version>) on <2006-01-02>.
# Shadow mode: stet records what it would do and blocks nothing.
# Every key is described in stet.example.yaml and in docs/spec-v0.1.md, chapter 9.
# This file holds the database password: keep it private.
```

`agentURL`은 보정 P8대로다. 주소에 비밀번호가 없으면 `<password>`도 없다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestAgentURL`: `postgres://app:pw@db.example.com:5432/crm?sslmode=verify-full&sslrootcert=/x/ca.pem&channel_binding=require&application_name=a`와 `127.0.0.1:6432` → `postgres://app:<password>@127.0.0.1:6432/crm?application_name=a&sslmode=disable`. 비밀번호 없음 → `postgres://app@127.0.0.1:6432/crm?sslmode=disable`. `listen`이 `0.0.0.0:7000`, `:7000`, `[::]:7000` → 호스트 `127.0.0.1`, 포트 7000. `[::1]:6432` → 그대로. 인코딩된 사용자 이름(`a%40b`)은 그대로다. `gssencmode`, `requiressl`, `sslsni`도 지워진다.
  - `TestRenderConfig`: 스펙의 예와 같은 모양(키와 값)이고 `config.Parse`가 읽는다: 섀도, `listen`, `upstream`, `adapter: none`, 테이블마다 `PK`, `Impact.MaxRows == 100`, `Reads != nil`, `Stale`은 준 대로. 머리 주석 넷이 있다. `send_fields`, `agents`, `undo`가 없다.
  - `TestInitConfigRoundTripsOddNames`: 테이블 `null`, `true`, `on`, `no`, `y`, `user`와 비밀번호에 `%23`·`%3A`가 든 주소 → `config.Parse`가 같은 테이블 이름들과 같은 `Upstream`을 돌려준다.
  - `TestInitThenProxy`(통합, 자기 데이터베이스, 계정 하나가 소유): `contacts` 1,000행과 `deals`(연쇄)를 만들고 `ANALYZE`. `runInit`을 `-yes`, `-config <임시>/stet.yaml`, `-listen 127.0.0.1:<빈 포트>`로 부른다(`sslmode=disable`을 적은 주소). 파일 권한이 `0600`이다. 출력에 `wrote `, `stet proxy -config `, 그리고 `agentURL`의 글자가 있다. `init` 뒤에도 `stet` 스키마가 없다. `config.Load`로 읽어 띄우면 로그에 `level=WARN`이 없다. 출력의 접속 문자열에서 `<password>`만 진짜 비밀번호로 바꿔 `psql`이 붙고, 한 행 고치기·300행 고치기·200행 지우기가 `commit`, `hold`, `hold`로 기록된다.
  - `TestInitAsks`: 스키마가 없고 `interactive`일 때 답 `y\n` → 쓴다. `n\n`, 빈 줄, EOF → `stopped; nothing was written`, 파일 없음(`TestInitDeclinedWritesNothing`). `interactive`가 거짓이고 `-yes`가 없으면 안내를 낸 뒤 위의 문구로 끝나고 파일이 없다. 스키마가 이미 있으면 묻지 않는다.
  - `TestInitRefusesToOverwrite`: 파일이 있으면 DB에 붙기 전에 끝난다(닿지 않는 주소를 줘도 같은 에러다). `-force`면 덮어쓰고 권한이 `0600`이다(전에 `0644`였어도).
  - `TestInitWritesTheFileWhole`: 쓰는 도중의 실패(쓸 수 없는 디렉터리)에서 반쯤 쓰인 파일도 임시 파일도 남지 않는다.
  - `TestInitOutputNeverShowsThePassword`: 성공, 접속 실패(틀린 비밀번호, 닿지 않는 호스트), 못 읽는 주소, 거부(`-yes` 없음)의 모든 출력과 에러에 비밀번호가 없다.
  - `TestInitStops`: 주소 없음, 주소 뒤의 플래그, 못 읽는 `-listen`, `-tables`의 없는 이름, 고를 테이블이 없는 데이터베이스(이유별 수가 출력에 있다), 스키마를 만들 수 없는 계정(문구에 `GRANT CREATE ON DATABASE`가 있다). `VersionNum`이 130000인 `initFacts`를 판단하는 자리의 단위 테스트 → `PostgreSQL 14 or newer is needed`.
  - `TestInitReportsWhatItLeftOut`(통합): `maxInitTables`를 넘는 데이터베이스 → 출력에 뺀 수와 `-tables`가 있다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. `main`에 `init`을 잇고(`interactive`는 표준 입력이 문자 장치인지), `usage`에 적는다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Add stet init: write a shadow config from the database`.

---

### Task 11: 문서 — quickstart, 설정 예, 만든 대로의 스펙

**Files:** Create `docs/quickstart.md`. Modify `stet.example.yaml`, `docs/spec-v0.1.md`(9장), `docs/spec-v0.2-tryout.md`.

- [ ] **Step 1: `docs/quickstart.md`**(한국어). 순서: 무엇을 보게 되는가(줄 셋의 예) → 설치(묶음을 푸는 법, `docker build`, 소스에서 `go build`. 게시된 릴리스나 이미지가 있다고 쓰지 않는다) → `stet init`(`DATABASE_URL`을 권하는 이유, 묻는 것, `-tables`, `-yes`) → `stet proxy` → 에이전트의 접속 주소 바꾸기 → 줄 읽는 법(판정 낱말, 사실, 이유) → 빈 DB에서 해 보기(`contacts` 1,000행과 `deals`를 만드는 SQL, 쓰기 셋) → 기록 보기(`stet.decisions`) → 지우는 법 → 한계(스펙 10장 전부. "통제가 아니다"를 맨 앞에) → 기록에 남는 값(`send_fields`가 없어도 `proposed` 컬럼·`statement`·`params`에 값이 남는다). Docker로 띄울 때는 `-decisions on`과 `listen: 0.0.0.0:6432`, 포트를 `127.0.0.1`에만 여는 것을 적는다.
- [ ] **Step 2: 그대로 따라 한다.** 묶음(`scripts/release.sh 0.2.0-test`)을 임시 디렉터리에 풀고, 시험 서버에 자기 데이터베이스와 계정 하나를 만들어, 문서의 명령을 처음부터 끝까지 그대로 친다. `stet proxy`는 터미널이 있어야 줄을 내므로 `script -q /dev/null …`로 띄운다. 문서의 예와 같은 세 줄(시각만 다르다)을 본다. 다르면 문서나 코드 가운데 틀린 쪽을 고친다(코드를 고칠 일이면 멈추고 보고한다). 끝나면 데이터베이스, 계정, `dist/`, 임시 파일을 지운다. 보고서에 친 명령과 본 출력을 적는다.
- [ ] **Step 3: `stet.example.yaml`.** `upstream`의 TLS 파라미터(`sslmode`의 뜻, `sslrootcert`, 적지 않으면 `prefer`), 받지 않는 것(`allow`, `sslnegotiation=direct`), `send_fields`가 선택이 되는 조건, 루프백이 아닌 `listen`의 뜻. `cmd/stet`의 `TestExampleConfigIs…`가 그대로 통과한다.
- [ ] **Step 4: `docs/spec-v0.1.md` 9장.** `send_fields`와 `upstream` 파라미터의 줄을 지금대로 고친다. 4장과 8장의 `send_fields` 문장이 어긋나면 함께 고친다.
- [ ] **Step 5: `docs/spec-v0.2-tryout.md`.** 상태 줄을 "구현"으로 바꾸고, 보정 P1~P8을 해당 장에 옮겨 적고(1장의 예를 실제 줄로), 진행 중에 원장에 적힌 판단(Ruling) 가운데 스펙을 바꾼 것을 옮긴다. 12장의 수용 기준을 근거와 함께 체크한다: 테스트 이름, Task 4의 지연 값, Task 8의 묶음·이미지 출력, Step 2의 결과. 확인하지 못한 둘(워크플로, 매니지드 서비스)은 체크하지 않고 이유를 적는다. `sslmode`를 적지 않은 기존 설정이 이 판부터 TLS를 먼저 청한다는 것을 3장에 적는다.
- [ ] **Step 6:** 소유자의 문서에서 낡는 줄을 찾아(고치지 않는다) 보고서에 파일과 줄 번호로 적는다: README.md, docs/concept-v0.2.md, docs/decisions.md의 TLS·설치·`send_fields`에 관한 줄.
- [ ] **Step 7:** 전체 명령이 통과한다. 커밋 `Docs: quickstart, and the try-out spec as built`.
