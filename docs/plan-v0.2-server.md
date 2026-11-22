# 서버에 놓기 (6단계 둘째 부분) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 에이전트는 Stet이 내준 토큰으로 TLS 위에서 Stet에 로그인하고, Stet이 그 계정의 진짜 비밀번호로 DB에 로그인해 세션을 이어 준다. 에이전트는 진짜 비밀번호를 모르므로 Stet을 건너뛸 수 없다.

**Architecture:** 프록시가 클라이언트의 SSLRequest에 TLS로 답하고, 보관을 켜면 시작 패킷 뒤에 서버 쪽 SCRAM(`internal/scram`)으로 클라이언트를 직접 인증한다. 인증이 되면 `internal/custody`가 pgx(`pgconn`)로 업스트림에 로그인하고 연결을 넘겨받는다(`Hijack`). 프록시는 서버가 로그인 끝에 보냈을 메시지(AuthenticationOk, ParameterStatus, BackendKeyData, ReadyForQuery)를 만들어 **업스트림에서 온 바이트의 앞머리로** 붙이고, 그 뒤는 지금의 중계에 그대로 넣는다. 세션과 게이트의 코드는 바꾸지 않는다. 자격 증명은 파일 하나(`agents.credentials`)에 있고 `stet agent`가 쓴다.

**Tech Stack:** Go 1.27, `crypto/tls`, `crypto/hmac`·`crypto/sha256`·`crypto/pbkdf2`·`crypto/subtle`, `github.com/jackc/pgx/v5`(pgconn, pgproto3), `go.yaml.in/yaml/v3`, `golang.org/x/term`(새 의존성), 테스트 서버 PostgreSQL 14.18(TLS), Docker 28.

**Spec:** [docs/spec-v0.2-server.md](spec-v0.2-server.md). 스펙과 이 계획이 다르면 아래 "스펙 보정"에 적힌 곳만 계획을 따르고, 나머지는 스펙을 따른다. 보정은 Task 9가 스펙에 옮겨 적고, 소유자에게는 끝날 때 보고한다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 규칙 6: 보관을 켠 Stet에서도 섀도의 문장과 결과는 직접 붙었을 때와 같다. 규칙 5: 시작 패킷의 계정 이름과 파라미터, 인증 메시지는 믿지 않는 입력이다. 규칙 7: `internal/proxy`는 자격 증명 파일과 pgconn의 로그인을 모른다. `Login` 인터페이스만 안다.
- **비밀은 어디에도 적지 않는다.** 토큰, 토큰의 검증값, 진짜 비밀번호는 로그(`-v` 포함), 에러, 표준 출력, 판단 기록, 패닉 메시지에 나오지 않는다. 나가는 곳은 자격 증명 파일과, 토큰을 만든 순간의 표준 출력 한 번이다. pgx가 돌려준 에러와 서버가 준 글자는 제어 문자를 걸러 적고, 주소를 인용한 에러는 그대로 적지 않는다.
- **보관을 켜지 않은 Stet은 이 단계 전과 같다.** `listen_tls`와 `agents.credentials`를 적지 않았을 때 클라이언트가 받는 바이트, 로그의 기존 키, 기존 테스트의 단언이 그대로다. 단언을 고쳐야 통과하는 기존 테스트가 나오면 멈추고 보고한다. 헬퍼를 새 시그니처에 맞추는 것은 단언 변경이 아니다.
- **판정을 바꾸지 않는다.** `internal/decide`, `internal/gate`, `internal/proxy/session.go`의 판단 경로, `stet.decisions`의 스키마는 건드리지 않는다.
- 스펙의 값. 토큰 `stet_` + base64url 43글자(무작위 32바이트). 검증값 `SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>`, 반복 4096, 방식 이름 `SCRAM-SHA-256`. SQLSTATE와 문구: `28P01` `password authentication failed for user "<계정>"`, `0A000` `stet: replication connections are not supported`, `28000` `stet: authentication exchange failed`, `08001` `stet: could not open a database session for "<계정>"`, `28000` `stet: this server requires TLS; connect with sslmode=require or stronger`. 시간 제한: 클라이언트와의 교환 10초, 업스트림 로그인 5초, 시작할 때 계정마다 5초·합쳐 30초. 파일 권한: 자기 파일 `0600` 이하, root 파일 `0640` 이하.
- 테스트: `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. PostgreSQL 14.18이 TLS와 평문을 모두 받으며 떠 있다(superuser `postgres`/`postgres-pw`, 데이터베이스 `stet_test`, `stet_scram`/`scram-pw`, `stet_md5`/`md5-pw`, `stet_clear`/`clear-pw`, 비밀번호 없는 `stet_trust`). 서버의 인증서는 `pgtest.ServerCert`로 얻는다. 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다. 모든 Task는 이 명령이 모든 패키지에서 `ok`인 채로 끝난다.
- **55432의 시험 서버를 켜거나 끄거나 고치지 않는다.** `stet_trust`를 어떤 역할에도 부여하지 않고 어떤 역할도 `stet_trust`에 부여하지 않는다. PUBLIC, `public` 스키마, `stet_test` 자체에는 GRANT·REVOKE하지 않는다. 다른 권한이나 계정이 필요한 테스트는 겹치지 않는 이름으로 자기 데이터베이스와 역할을 만들고 지운다. 벤치마크의 역할과 데이터베이스(`crm_owner`, `gate_svc`, `agent_v`, `agent_i`, `agent_x`, `agent_sync`, `crm_base`)는 건드리지 않는다.
- 소유자의 Supabase 프로젝트에는 붙지 않는다. 확인은 로컬 TLS 서버로만 한다.
- TDD: 동작이 바뀌는 곳은 실패하는 테스트를 먼저 쓰고, 돌려서 실패를 본 뒤에 구현한다.
- `internal/proxy`를 건드리는 Task는 `latency_test.go`(p95 2ms)도 돌린다. 그 파일의 기존 테스트 함수는 고치지 않는다.
- 새 Go 의존성은 `golang.org/x/term` 하나다(Task 8).
- 바깥에 내보내지 않는다. `git push`, 릴리스, 태그, `docker push`를 하지 않는다.
- README.md, docs/concept-v0.2.md, docs/decisions.md와 앞 단계의 계획 문서는 고치지 않는다.
- 문서와 YAML·SQL 스키마 주석은 한국어, 코드의 주석·식별자·에러 메시지·로그·프로그램의 출력(자격 증명 파일의 주석 포함)은 영어다.
- 브랜치 `v0.2-server`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고 마지막 줄은 `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`이다. 어느 모델이 쓰든 이 줄을 바꾸지 않는다.

## Review Focus

스펙이 암시하지만 수용 기준에 줄이 없는 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **깨졌거나 악의적인 인증 메시지.** 길이 칸이 수 기가바이트인 메시지, SASL 응답 자리에 온 Query, 깨진 base64, 다시 쓴 nonce, 제안하지 않은 채널 바인딩(`p=`)의 요청, 한 바이트씩 천천히 보내는 클라이언트. 그 세션 하나만 제때 끝나고 메모리를 잡지 않는다. (Task 1 `TestServerRefusesBrokenMessages`, Task 6 `TestBrokenAuthEndsOnlyThatSession`)
2. **로그인 도중에 끝나는 것.** 업스트림에 로그인하는 동안 프록시가 닫힌다, 클라이언트가 증명을 보낸 직후 끊긴다, 업스트림이 로그인 직후 끊는다. 업스트림 연결이 남지 않고 `Close`가 돌아온다. (Task 6 `TestLoginInterruptedLeavesNothing`)
3. **다시 읽을 때 이상한 파일.** 반쯤 쓰인 파일, 빈 파일, 권한이 풀린 파일, 사라진 파일, 같은 초에 두 번 바뀐 파일. 쓰던 것을 계속 쓰고, 두 번째 변경도 본다. (Task 2 `TestStoreKeepsTheLastGoodFile`, `TestStoreSeesTwoChangesInOneSecond`)
4. **계정 이름의 글자.** 대소문자만 다른 두 계정, 63바이트를 넘는 이름, 제어 문자가 든 이름, YAML이 달리 읽는 이름(`null`, `on`). 비교는 바이트 그대로이고, 로그에는 걸러 적고, 파일은 같은 이름으로 다시 읽힌다. (Task 2 `TestFileRoundTripsOddAccountNames`, Task 6 `TestAccountNamesAreComparedByteForByte`)
5. **비밀이 새는 곳.** pgx의 접속 에러, `-v`의 디버그 로그, `stet agent`의 에러, 시작할 때의 확인, 로그인 거절의 로그. (Task 3 `TestLoginErrorsCarryNoSecrets`, Task 7 `TestNoSecretInAnyLog`, Task 8 `TestAgentOutputShowsTheTokenOnce`)

## 스펙 보정

| # | 스펙 | 확인한 것 | 보정 (Task) |
|---|---|---|---|
| V1 | 8장 "설정 검증(`config.Load`)"에서 `agents.roles`와 파일의 계정을 비교한다 | `config.Parse`는 바이트만 읽고 파일을 열지 않는다. 테스트 수백 개가 그렇게 쓴다 | 그 비교와 파일의 권한·모양 확인은 `stet proxy`가 시작할 때 한다. 다르면 시작하지 않는 것은 같다. `config`는 파일 없이 볼 수 있는 것(업스트림의 서버 확인, `listen`의 TLS, 키의 짝)만 본다 (4, 7) |
| V2 | 6장·3장의 경로 | 상대 경로를 무엇에 대해 읽는지 없다 | `agents.credentials`, `listen_tls.cert`, `listen_tls.key`의 상대 경로는 설정 파일이 있는 디렉터리에 대해 읽는다(`config.Load`). `config.Parse`는 적힌 그대로 둔다 (4) |
| V3 | 5장 "게이트는 이 세션을 처음부터 인증된 세션으로 본다" | 세션은 서버의 BackendKeyData와 첫 ReadyForQuery, enforce에서는 ParameterStatus를 서버 쪽 흐름에서 읽는다 | 프록시가 만든 로그인 끝의 메시지들을 업스트림 연결의 앞머리로 붙여 지금의 중계에 넣는다. 세션은 그것을 서버가 보낸 것으로 읽는다. 세션의 코드는 바꾸지 않는다 (6) |
| V4 | 4장 "클라이언트와의 교환은 10초" | 인증 메시지의 크기 한도가 없다 | 인증 동안 클라이언트의 메시지는 본문 16KiB까지만 받는다. 넘으면 `28000`으로 끝낸다. 버퍼 없이 읽는다(인증 뒤의 바이트를 미리 읽지 않는다) (6) |
| V5 | 4장 | 프록시를 닫을 때 진행 중인 업스트림 로그인 | `Server.Close`가 진행 중인 로그인을 취소한다(컨텍스트). 5초를 기다리지 않는다 (6) |

**스펙이 구현에 맡겨 이 계획이 정한 것.** 서버 쪽 SCRAM은 `internal/scram`, 자격 증명 파일과 업스트림 로그인은 `internal/custody`에 둔다. 프록시는 `Server`의 필드 둘(`ClientTLS`, `Login`)로 받는다. `loopbackHost`는 `internal/config`로 옮긴다. 가짜 검증값은 프로세스가 시작할 때 만든 무작위 32바이트와 계정 이름의 HMAC에서 만든다. 다시 읽기는 경로의 `os.Stat`(바뀐 시각, 크기, 같은 파일인지)으로 본다.

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/scram/verifier.go`, `server.go` | 검증값과 토큰, 서버 쪽 SCRAM 교환 (신규, DB 없음) |
| `internal/custody/file.go` | 자격 증명 파일: 읽기, 권한 규칙, 온전히 쓰기 (신규) |
| `internal/custody/store.go` | 도는 동안의 계정들: 다시 읽기, 환경 변수의 비밀번호 (신규) |
| `internal/custody/login.go` | pgconn으로 업스트림에 로그인하고 넘겨받기. `proxy.Login`의 구현 (신규) |
| `internal/config/config.go` | `listen_tls`, `agents.credentials`, 보관의 검증, `LoopbackHost` (수정) |
| `internal/proxy/clienttls.go` | SSLRequest에 TLS로 답하기, 평문 거절 (신규) |
| `internal/proxy/login.go` | `Login`, `Opened`, 클라이언트 인증의 흐름, 앞머리 붙이기 (신규) |
| `internal/proxy/proxy.go` | `handle`과 `readStartup`을 위에 잇기 (수정) |
| `internal/pgtest/tls.go` | 테스트용 Stet 인증서 (수정) |
| `cmd/stet/custody.go` | 시작할 때의 확인, 인증서 다시 읽기, 잇기 (신규) |
| `cmd/stet/agent.go` | `stet agent` (신규) |
| `cmd/stet/main.go`, `transport.go` | 명령, 로그 줄, 경고 (수정) |
| `docs/deploy.md` (신규), `docs/quickstart.md`, `stet.example.yaml`, `docs/spec-v0.1.md`, `docs/spec-v0.2-server.md` | 문서 (수정) |

---

### Task 0: 계획 커밋

- [ ] 이 문서를 커밋한다. `Docs: plan for the server deployment`.

---

### Task 1: 서버 쪽 SCRAM과 토큰

**Files:** Create `internal/scram/verifier.go`, `internal/scram/server.go`. Test `internal/scram/verifier_test.go`, `internal/scram/server_test.go`.

**Interfaces — Produces:**

```go
// Package scram is the server side of SCRAM-SHA-256 (RFC 5802, 7677) as
// PostgreSQL speaks it, and the verifiers it checks clients against.
package scram

const (
	Mechanism  = "SCRAM-SHA-256"
	Iterations = 4096
)

// Verifier is what a server keeps of a password: enough to check a client's
// proof, not enough to log in as the client.
type Verifier struct {
	Iterations           int
	Salt                 []byte
	StoredKey, ServerKey []byte
}

// ParseVerifier reads "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>"
// (base64), the form PostgreSQL keeps in pg_authid.
func ParseVerifier(s string) (Verifier, error)
func (v Verifier) String() string

// NewVerifier derives the verifier of password under salt.
func NewVerifier(password string, salt []byte, iterations int) Verifier

// NewToken returns a new token, "stet_" and 43 base64url characters of 32
// random bytes, and its verifier under a random 16-byte salt.
func NewToken() (token string, v Verifier, err error)

// Mock returns a verifier for an account that has none, so that an exchange
// with it looks like any other and always fails: the same account and secret
// give the same salt.
func Mock(account string, secret []byte) Verifier

// Server runs the server side of one exchange.
type Server struct{ /* … */ }

// NewServer starts an exchange against v. mock makes Final fail whatever the
// proof. nonce, when not nil, supplies the server's nonce (tests).
func NewServer(v Verifier, mock bool, nonce func() string) *Server

// First takes the data of a SASLInitialResponse (the client-first-message)
// and returns the server-first-message.
func (s *Server) First(clientFirst []byte) (serverFirst []byte, err error)

// Final takes the client-final-message. ok false with a nil error: the
// message was well formed and the proof wrong. An error: it could not be read.
func (s *Server) Final(clientFinal []byte) (serverFinal []byte, ok bool, err error)
```

채널 바인딩 표시는 `n`과 `y`를 받고 `p=`는 에러다. 클라이언트 최종 메시지의 `c=`는 처음 보낸 표시와 같아야 한다. 인가 신원(`a=`)은 받지 않는다. 클라이언트가 적은 사용자 이름은 보지 않는다(PostgreSQL과 같다). 증명과 nonce의 비교는 `crypto/subtle`로 한다. `Server`는 한 번만 쓴다. `First` 전의 `Final`, 두 번째 `First`는 에러다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestRFC7677Vector`: 사용자 `user`, 비밀번호 `pencil`, salt `W22ZaJ0SNY7soEsUEjb6gQ==`, 4096, 클라이언트 nonce `rOprNGfwEbeRWgbNEkqO`, 서버 nonce `%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0`. 클라이언트 첫 메시지 `n,,n=user,r=rOprNGfwEbeRWgbNEkqO` → 서버 첫 메시지 `r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096`. 클라이언트 최종 `c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,p=dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=` → `ok`, 서버 최종 `v=6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=`.
  - `TestVerifierRoundTrip`: `ParseVerifier(v.String())`가 같은 값. 깨진 글자(다른 방식 이름, 반복 수 0·음수·글자, 조각 수가 다름, 깨진 base64)는 에러.
  - `TestNewToken`: `stet_`로 시작하고 길이 48, 뒤 43글자가 base64url. 두 번 부르면 다르다. 돌려준 검증값이 그 토큰으로 `Final`을 통과시킨다(테스트 안의 클라이언트 쪽 계산으로).
  - `TestWrongPasswordFails`: 다른 비밀번호의 증명 → `ok` 거짓, 에러 없음, 서버 최종 메시지 없음.
  - `TestMockNeverSucceeds`: `Mock`의 salt는 같은 계정·비밀값에서 같고 다른 계정에서 다르다. 서버 첫 메시지의 모양이 진짜 검증값의 것과 같다(같은 필드, 같은 반복 수, salt 길이 16). 어떤 증명으로도 `ok`가 거짓이다.
  - `TestServerRefusesBrokenMessages`: `p=tls-server-end-point,,…`, `a=`가 든 표시, nonce가 없는 첫 메시지, 서버 nonce가 다른 최종 메시지, `c=`가 처음의 표시와 다른 최종 메시지(`y,,`로 시작해 `c=biws`), 깨진 base64의 증명, 길이가 32가 아닌 증명, 빈 메시지, 1MiB짜리 메시지 → 모두 에러이고 패닉이 없다. `y,,`로 시작해 `c=eSws`면 받는다.
  - `TestServerIsUsedOnce`: `First` 전의 `Final`, 두 번째 `First`, 두 번째 `Final` → 에러.
  - `TestAcceptsAPostgresVerifier`(통합. `STET_TEST_PG`가 없으면 건너뛴다): superuser로 `pg_authid`에서 `stet_scram`의 `rolpassword`를 읽어 `ParseVerifier`로 읽고, 비밀번호 `scram-pw`의 증명으로 `Final`이 `ok`다. 다른 비밀번호는 아니다. 우리 계산이 PostgreSQL의 것과 같다는 확인이다.
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/scram/`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** `go test -race ./internal/scram/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Scram: the server side of SCRAM-SHA-256, verifiers and tokens`.

---

### Task 2: 자격 증명 파일과 도는 동안의 계정들

**Files:** Create `internal/custody/file.go`, `internal/custody/store.go`. Test `internal/custody/file_test.go`, `internal/custody/store_test.go`. DB가 필요 없다.

**Interfaces — Consumes:** `scram.Verifier`, `scram.ParseVerifier` (Task 1).

**Interfaces — Produces:**

```go
// Package custody keeps the agents' tokens and real database passwords, and
// logs in to the database for them.
package custody

// Agent is one account in the credentials file.
type Agent struct {
	Token       string    `yaml:"token"`                  // a scram verifier
	Password    string    `yaml:"password,omitempty"`     // the real password, or
	PasswordEnv string    `yaml:"password_env,omitempty"` // the environment variable that holds it
	Added       time.Time `yaml:"added"`
	TokenIssued time.Time `yaml:"token_issued"`
}

// File is the credentials file.
type File struct {
	Agents map[string]Agent `yaml:"agents"`
}

// CheckMode applies the rule for who may read the file: owned by uid with no
// access for group or others, or owned by root with at most group read.
func CheckMode(mode fs.FileMode, owner, uid uint32) error

// Read reads and validates the file at path, CheckMode included.
func Read(path string) (*File, error)

// Write puts f at path whole: a temporary file in the same directory, then a
// rename. A file already there keeps its owner, group and mode; when that
// cannot be kept, nothing is written. A new file is the caller's, 0600.
func Write(path string, f *File) error

// Entry is an account as the running proxy uses it.
type Entry struct {
	Token    scram.Verifier
	Password string // resolved: the file's, or the environment's at start
}

// Store is the accounts of a running proxy. The set is fixed when it is
// opened; tokens and passwords follow the file.
type Store struct{ /* … */ }

// OpenStore reads path. env reads the environment, once, for password_env.
func OpenStore(path string, env func(string) string, log *slog.Logger) (*Store, error)

// Accounts returns the accounts the store was opened with, sorted.
func (s *Store) Accounts() []string

// Lookup returns account's entry. It reads the file again first when the
// file changed. ok false: not an account, or removed since.
func (s *Store) Lookup(account string) (e Entry, ok bool)
```

검증(`Read`): 계정마다 `token`이 `scram.ParseVerifier`로 읽히고, `password`와 `password_env` 가운데 정확히 하나가 있다. 계정 이름은 비어 있지 않고 63바이트 이하다. 에러는 계정 이름과 무엇이 틀렸는지 말하고, 값은 말하지 않는다. `Write`는 머리 주석 `# Written by stet agent. It holds database passwords: keep it private.`을 쓰고 계정을 이름 순서로 쓴다. 이름은 YAML이 같은 글자로 다시 읽게 쓴다.

`Store`: `Lookup`은 경로를 `os.Stat`하고, 바뀐 시각·크기·같은 파일인지(`os.SameFile`) 가운데 하나라도 다르면 `Read`한다. 다시 읽지 못하면 쓰던 것을 계속 쓰고 에러 로그를 한 번 남긴다(같은 실패가 이어지는 동안 되풀이하지 않는다). 연 뒤에 생긴 계정은 받지 않고, 처음 볼 때 `account "x" was added after stet started; restart stet proxy to accept it`를 경고로 한 번 남긴다. 지운 계정은 `ok` 거짓이다. `password_env`의 값은 열 때 읽은 것을 쓴다. 다시 읽은 파일이 다른 변수 이름을 가리키면 그 계정은 쓰던 값을 계속 쓰고 경고한다. `Lookup`은 여러 고루틴에서 불린다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestCheckMode`: 자기 파일 `0600`·`0400` 통과, `0640`·`0644`·`0660` 거부. root 파일(`owner` 0) `0640`·`0600`·`0440` 통과, `0644`·`0660` 거부. 다른 사용자의 파일은 권한과 상관없이 거부.
  - `TestReadValidates`: 읽히는 파일. `token` 없음, 깨진 `token`, `password`와 `password_env`가 둘 다 있음, 둘 다 없음, 빈 이름, 64바이트 이름, 모르는 키 → 에러에 계정 이름이 있고 비밀번호와 검증값의 글자가 없다. 권한 `0644`인 파일 → 거부.
  - `TestWriteIsWholeAndKeepsTheMode`: 새 파일은 `0600`. 이미 있는 `0400` 파일을 바꿔 쓰면 `0400` 그대로다. 쓸 수 없는 디렉터리에서는 에러이고 임시 파일이 남지 않는다. 쓴 것을 `Read`하면 같은 값이다.
  - `TestFileRoundTripsOddAccountNames`: `null`, `on`, `Agent`, `agent`, 공백이 든 이름, 63바이트 이름 → `Write` 뒤 `Read`가 같은 이름들을 돌려준다.
  - `TestStoreFollowsTheFile`: 토큰을 바꿔 쓰면 다음 `Lookup`이 새 검증값을 준다. 비밀번호도 같다. 계정을 지우면 `ok` 거짓. 열고 나서 더한 계정은 `ok` 거짓이고 경고가 한 번이다.
  - `TestStoreKeepsTheLastGoodFile`: 반쯤 쓰인(깨진 YAML) 파일, 빈 파일, 권한 `0644`로 풀린 파일, 지워진 파일 → `Lookup`이 쓰던 값을 주고 에러 로그는 실패가 이어지는 동안 한 번이다. 파일이 다시 온전해지면 새 값을 준다.
  - `TestStoreSeesTwoChangesInOneSecond`: 같은 바뀐 시각(`os.Chtimes`로 맞춘다)과 같은 크기로 두 번 바꿔 쓴다(이름 바꾸기로) → 두 번째 값도 본다.
  - `TestStorePasswordEnv`: 열 때의 환경 값이 `Entry.Password`다. 변수가 비어 있으면 `Password`가 비고, 그 계정을 `OpenStore`가 경고한다. 뒤에 환경이 바뀌어도 값은 그대로다.
  - `TestStoreIsSafeForConcurrentUse`: 파일을 바꿔 쓰는 동안 여러 고루틴이 `Lookup`한다(`-race`).
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/custody/`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** `go test -race ./internal/custody/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Custody: the credentials file and the accounts of a running proxy`.

---

### Task 3: 업스트림에 로그인하고 넘겨받기

**Files:** Create `internal/custody/login.go`, `internal/proxy/login.go`(타입만). Test `internal/custody/login_test.go`.

**Interfaces — Consumes:** `custody.Store`, `Entry` (Task 2), `scram.Mock`, `scram.Verifier` (Task 1), `config.Config.UpstreamTLS` (앞 단계), `pgtest.ServerCert`.

**Interfaces — Produces:**

```go
// package proxy (login.go)

// Opened is an upstream session the proxy did not log in to itself.
type Opened struct {
	Conn      net.Conn          // logged in and idle
	PID       uint32            // the server process
	SecretKey []byte            // its cancel key
	Params    map[string]string // the settings the server reported at login
	TxStatus  byte              // from the server's ReadyForQuery
	Unread    []byte            // bytes already read from Conn after that, if any
}

// Login authenticates clients for the proxy and opens their sessions.
type Login interface {
	// Verifier returns what to check account's token against. known false:
	// there is no such account; the verifier is a mock.
	Verifier(account string) (v scram.Verifier, known bool)
	// Open logs in upstream as account and hands the session over. params
	// are the client's startup parameters without user, database and
	// replication. Its error is for the log, never for the client.
	Open(ctx context.Context, account, database string, params map[string]string) (*Opened, error)
}

// package custody (login.go)

// Login is the proxy.Login of a credentials store.
type Login struct{ /* … */ }

// NewLogin returns the Login of store over upstream, the config's upstream
// URL, reached with tlsConfig (nil: plaintext) and plainFallback as the
// proxy's own leg is (config.UpstreamTLS, UpstreamPlainFallback).
func NewLogin(store *Store, upstream string, tlsConfig *tls.Config, plainFallback bool) (*Login, error)

// Check logs in as account and closes the session: what stet proxy does at
// start and stet agent before it writes. Its error carries no secret.
func (l *Login) Check(ctx context.Context, account, database string) error

// TryPassword is Check with a password that is not in the store yet.
func TryPassword(ctx context.Context, upstream string, tlsConfig *tls.Config, plainFallback bool, account, password, database string) error
```

`Open`: `pgconn.ParseConfig(upstream)`의 결과를 복제해 사용자, 비밀번호, 데이터베이스를 바꾸고, `RuntimeParams`를 `params`로 바꾸고, TLS를 `tlsConfig`(클라이언트 인증서 없음)로 바꾼다. `plainFallback`이 거짓이면 대체 설정을 비운다. 비밀번호가 비어 있으면 붙지 않고 에러다(환경 변수나 `.pgpass`가 쓰이지 않게). `pgconn.ConnectConfig` 뒤에 `SyncConn`과 `Hijack`. 넘겨받은 `Frontend`에 읽지 않은 바이트가 있으면 `Unread`에 담는다. 로그인 동안의 NoticeResponse는 버린다. 에러는 주소를 인용하지 않는 글자로 바꾼다: 갈래(`wrong password`, `cannot reach the server`, `certificate`, `refused TLS`, `other`)와 서버의 SQLSTATE·메시지(제어 문자를 거른 것)만 담는다. 비밀번호와 토큰은 어떤 에러에도 없다.

- [ ] **Step 1: 실패하는 테스트**(통합. 시험 서버, `sslmode=verify-full`과 `pgtest.ServerCert`).
  - `TestOpenHandsOverASession`: `stet_scram`의 진짜 비밀번호를 가진 `Store`로 `Open` → 돌려받은 `Conn`에 Query 메시지를 직접 써서 `select current_user`의 답을 읽는다(`stet_scram`). `PID`가 `pg_backend_pid()`와 같고, `Params`에 `server_version`과 `TimeZone`이 있고, `TxStatus`가 `I`다. 서버 쪽에서 그 세션이 TLS다.
  - `TestOpenPassesStartupParameters`: `application_name`과 `options=-c statement_timeout=1500`을 주면 세션에서 그 값이 보인다.
  - `TestOpenUsesOnlyTheStoredPassword`: 저장된 비밀번호가 비어 있으면, `PGPASSWORD`에 맞는 값이 있어도(`t.Setenv`) 붙지 않고 에러다. 저장된 비밀번호가 틀리면 갈래가 `wrong password`다.
  - `TestOpenWorksForEachAuthMethod`: `stet_scram`, `stet_md5`, `stet_clear` 모두 된다.
  - `TestOpenPresentsNoClientCertificate`: 클라이언트 인증서를 청하는 가짜 TLS 업스트림에서 인사에 인증서가 없다(`upstream`에 `sslcert`·`sslkey`가 있어도).
  - `TestOpenHonoursTheContext`: 답하지 않는 가짜 업스트림에서 컨텍스트를 취소하면 곧 돌아오고 연결이 남지 않는다.
  - `TestVerifierKnownAndMock`: 있는 계정은 그 검증값과 `known` 참. 없는 계정은 `known` 거짓이고 두 번 물어도 같은 검증값이다. 지운 계정은 `known` 거짓.
  - `TestLoginErrorsCarryNoSecrets`: 틀린 비밀번호, 닿지 않는 호스트, 인증서 확인 실패, TLS를 거절하는 가짜 → 에러 글자에 비밀번호와 주소 전체가 없고 갈래가 맞다.
  - `TestCheckAndTryPassword`: 맞으면 `nil`이고 세션이 남지 않는다(`pg_stat_activity`). 틀리면 에러.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Custody: log in upstream for an agent and hand the session over`.

---

### Task 4: 설정 — `listen_tls`와 `agents.credentials`

**Files:** Modify `internal/config/config.go`, `cmd/stet/transport.go`(와 `loopbackHost`를 쓰는 곳). Test `internal/config/config_test.go`.

**Interfaces — Produces:**

```go
// in Config:

// ListenTLS, when set, is the certificate the proxy answers clients' TLS
// requests with (spec-v0.2-server 3). nil: they are declined.
ListenTLS *ListenTLS
// Credentials is agents.credentials: the path of the credentials file.
// Not empty: stet authenticates the agents itself (custody).
Credentials string

type ListenTLS struct{ Cert, Key string }

// LoopbackHost reports whether host is this machine: "localhost" or a
// loopback IP address. An empty or unspecified host (0.0.0.0, ::) is not.
func LoopbackHost(host string) bool
```

`cmd/stet`의 `loopbackHost`를 지우고 `config.LoopbackHost`를 쓴다(그 테스트도 옮긴다. 단언은 그대로다).

검증(`Parse`):

- `listen_tls`가 있으면 `cert`와 `key`가 모두 있어야 한다: `listen_tls: cert and key are both required`.
- `agents.credentials`가 있으면:
  - `mode: enforce`에서 `agents.roles`가 없어도 된다(계정은 시작할 때 파일에서 온다).
  - `upstream`의 호스트가 루프백이 아니고 `UpstreamTransport()`가 `TransportVerified`가 아니면: `upstream: with agents.credentials the server must be verified, or stet could hand a database password to whoever answers; use sslmode=verify-full (and sslrootcert=<CA file> when the server's CA is not a public one)`.
  - `listen`의 호스트가 루프백이 아니고 `listen_tls`가 없으면: `listen: with agents.credentials a listen address that is not loopback needs listen_tls: on a plaintext connection whoever sits in between can take over a session once the agent has logged in`.
- `Load`는 세 경로(`agents.credentials`, `listen_tls.cert`, `listen_tls.key`)가 상대 경로면 설정 파일의 디렉터리에 대해 읽는다(보정 V2). `Parse`는 그대로 둔다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestLoopbackHost`(옮긴 것).
  - `TestListenTLS`: 둘 다 있으면 읽힌다. 하나만 있으면 위의 문구. 없으면 `ListenTLS == nil`.
  - `TestCustodyNeedsAVerifiedUpstream`: 원격 `disable`, 적지 않음, `require`(루트 없음) → 거부. 원격 `verify-full`, `verify-ca`, `require`와 `sslrootcert` → 통과. 루프백은 어떤 `sslmode`든 통과. 보관이 없으면 원격 `require`도 통과한다(이 단계 전과 같다).
  - `TestCustodyNeedsTLSOnARemoteListen`: `0.0.0.0:6432`, `:6432`, `10.0.0.5:6432`에 `listen_tls`가 없으면 거부, 있으면 통과. `127.0.0.1:6432`는 없어도 통과.
  - `TestEnforceWithCredentialsNeedsNoRoles`: enforce, `agents.credentials`만 → 읽히고 `AgentRoles`가 비어 있다. enforce, 둘 다 없음 → 지금의 에러 그대로.
  - `TestLoadResolvesPathsAgainstTheConfigFile`: 상대 경로 셋이 설정 파일의 디렉터리에 붙는다. 절대 경로는 그대로다.
  - 에러 어디에도 주소 전체가 없다(비밀번호가 든 `upstream`으로 확인).
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/config/ ./cmd/stet/ -run 'Loopback|ListenTLS|Custody|Credentials|Resolves'`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Config: listen_tls and agents.credentials, with what custody requires`.

---

### Task 5: 프록시 — 클라이언트 쪽 TLS

**Files:** Create `internal/proxy/clienttls.go`. Modify `internal/proxy/proxy.go`, `internal/pgtest/tls.go`. Test `internal/proxy/clienttls_test.go`.

**Interfaces — Produces:**

```go
// package proxy, on Server (set before Serve):

// ClientTLS, when set, is what the proxy answers a client's SSLRequest
// with. nil: the request is declined and the client goes on in plaintext.
ClientTLS *tls.Config

// package pgtest

// LocalCert makes a self-signed certificate for localhost and 127.0.0.1
// (as a DNS name too: libpq 14 does not match IP entries), writes it and its
// key as PEM files in t.TempDir(), and returns the paths and a pool that
// trusts it.
func LocalCert(t testing.TB) (certPath, keyPath string, roots *x509.CertPool)
```

`readStartup`: `ClientTLS`가 있으면 SSLRequest에 `S`로 답하고 `tls.Server`로 인사한다(시작과 같은 10초 안에). 그 뒤의 시작 패킷은 TLS 연결에서 읽는다. TLS 안에서 다시 온 SSLRequest는 거절이다(접속을 끝낸다). GSSENCRequest에는 `N`이다. `ClientTLS`가 있는데 TLS 없이 시작 패킷(취소 요청이 아닌 것)이 왔고 상대 주소가 루프백이 아니면 `FATAL` `28000`과 스펙의 문구로 끝낸다. 취소 요청은 평문으로도 받는다. `handle`의 나머지는 받은 연결(`net.Conn`)을 그대로 쓴다. 클라이언트 쪽이 TLS일 때는 `authTap`을 쓰지 않는다(스펙 3장 "보관 없이 켤 때").

- [ ] **Step 1: 실패하는 테스트.**
  - `TestClientTLSHandshake`: `ClientTLS`를 준 프록시(평문 업스트림)에 pgx가 `sslmode=verify-full`(`LocalCert`의 루트)로 붙어 `select 1`을 한다. 클라이언트 쪽 연결이 TLS다. `sslmode=disable`의 루프백 접속도 된다.
  - `TestClientTLSDeclinedWithoutACertificate`: `ClientTLS`가 없으면 SSLRequest의 답이 `N`이고, 이 단계 전과 같은 바이트다(기존 `SSLRequestIsDeclined` 시나리오가 그대로 통과한다).
  - `TestPlaintextFromElsewhereIsRefused`: 상대 주소를 판단하는 자리의 단위 테스트(루프백 통과, `10.0.0.5` 거절)와, 거절할 때 클라이언트가 받는 `28000`과 문구. 취소 요청은 거절하지 않는다.
  - `TestScenariosOverClientTLS`: `scenarios`의 줄들을 `ClientTLS`를 준 프록시(평문 업스트림)로 돌린다. 클라이언트는 `sslmode=verify-full`이다. `SSLRequestIsDeclined` 줄은 뺀다(여기서는 받는다).
  - `TestRelayWithTLSOnBothSides`: `ClientTLS`와 `tlsUpstream`. `psql`로 `stet_md5`와 `stet_clear`는 로그인한다. `stet_scram`은 기본 설정으로 로그인하지 못하고, `channel_binding=disable`로는 로그인한다(스펙 3장의 문장을 확인한다. 다르게 나오면 멈추고 보고한다). 클라이언트가 받은 AuthenticationSASL의 목록은 서버가 보낸 그대로다.
  - `TestTLSHandshakeIsBounded`: SSLRequest 뒤에 아무것도 보내지 않는 클라이언트, TLS 인사 도중에 멈춘 클라이언트 → 시간 제한 뒤에 끝나고(테스트는 제한을 줄인다) 다른 세션은 그동안 붙는다.
  - `TestSecondSSLRequestInsideTLS`: 접속이 끝난다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령과 `latency_test.go`가 통과한다.
- [ ] **Step 5:** 커밋 `Proxy: answer clients' TLS requests`.

---

### Task 6: 프록시 — 에이전트의 로그인

**Files:** Modify `internal/proxy/login.go`, `internal/proxy/proxy.go`, `internal/proxy/export_test.go`. Test `internal/proxy/login_test.go`, `internal/proxy/latency_test.go`.

**Interfaces — Consumes:** `Login`, `Opened` (Task 3), `scram.NewServer`, `Mechanism` (Task 1), `custody.NewLogin`, `OpenStore` (테스트에서), `Server.ClientTLS`, `pgtest.LocalCert` (Task 5).

**Interfaces — Produces:**

```go
// on Server (set before Serve):

// Login, when set, authenticates every client and opens its upstream
// session (spec-v0.2-server 4, 5). nil: authentication is relayed.
Login Login
```

`handle`에서 `Login`이 있을 때의 흐름. 시작 패킷을 읽고(취소 요청은 지금처럼 전한다), `startupInfo`로 계정을 읽는다.

1. 시작 패킷을 읽지 못했으면 접속을 끝낸다. `replication` 파라미터가 있으면 `0A000`.
2. `Login.Verifier(계정)`. AuthenticationSASL(`SCRAM-SHA-256` 하나)을 보내고, SASLInitialResponse와 SASLResponse를 받아 `scram.Server`로 교환한다. 클라이언트의 메시지는 버퍼 없이 읽고 본문은 16KiB까지다(보정 V4). SASLInitialResponse의 방식 이름이 다르거나, `p`가 아닌 메시지가 오거나, 교환이 에러면 `28000`과 스펙의 문구. 증명이 틀렸거나 `known`이 거짓이면 `28P01`.
3. 맞으면 AuthenticationSASLFinal을 보내고 `Login.Open`을 부른다(5초. `Server.Close`가 취소한다, 보정 V5). 실패하면 `08001`과 스펙의 문구를 주고 이유는 로그에만 남긴다.
4. 넘겨받은 연결을 지금의 추적(`track`)에 넣는다. AuthenticationOk, ParameterStatus(이름 순서), BackendKeyData, ReadyForQuery(`TxStatus`), 그리고 `Unread`를 이은 바이트를 **업스트림 연결에서 읽히는 앞머리로** 붙인다(보정 V3). 그 연결로 지금의 `relay`를 부른다. 보는 세션이면 지금의 탭을 그대로 쓴다. `authTap`은 쓰지 않는다.

`Login`이 있으면 모르는 계정의 세션을 중계하지 않는다. 로그: 로그인이 되면 INFO `agent logged in`(계정, 상대 주소, `tls`), 거절하면 WARN `agent login refused`(계정, 상대 주소, `reason`이 `unknown account`·`bad token`·`upstream login failed`·`exchange failed` 가운데 하나, 업스트림 실패면 `Open`의 에러). 계정 이름은 제어 문자를 걸러 적는다. 토큰, 검증값, 인증 메시지의 본문은 적지 않는다.

- [ ] **Step 1: 실패하는 테스트**(통합. 테스트 헬퍼가 자격 증명 파일을 `t.TempDir()`에 만들고 `custody.NewLogin`으로 잇는다. 계정은 시험 서버의 `stet_scram`·`stet_md5`·`stet_clear`이고 토큰은 `scram.NewToken`이다).
  - `TestAgentLogsInWithAToken`: pgx와 `psql`이 `sslmode=verify-full`, 계정 `stet_scram`, 비밀번호 자리에 토큰으로 붙어 `select current_user`가 `stet_scram`이고 서버 쪽에서 TLS다. `application_name`과 `options`가 전달된다. 평문 루프백으로도 된다.
  - `TestLoginRefusals`: 틀린 토큰, 모르는 계정, 진짜 비밀번호를 토큰 자리에 넣은 것, `stet_md5`의 토큰으로 `stet_scram`을 댄 것 → 모두 `28P01`과 `password authentication failed for user "<댄 이름>"`. 날 프로토콜 클라이언트로, 모르는 계정과 틀린 토큰에서 받은 메시지의 종류와 수가 같고 AuthenticationSASLContinue의 모양(필드, 반복 수, salt 길이)이 같다.
  - `TestTokenIsNoDatabasePassword`: 토큰으로 시험 서버에 직접 붙으면 서버가 거절한다.
  - `TestScenariosThroughALogin`, `TestWatchedScenariosThroughALogin`: `scenarios`의 줄들을 보관을 켠 프록시로 돌린다(인증 방식 줄과 틀린 비밀번호 줄은 위의 테스트가 대신하므로 뺀다). 취소가 전달된다.
  - `TestWatchedSessionKnowsItsAccount`: 모든 세션을 보는 기록용 게이트에서, 보관을 켠 세션의 첫 쓰기가 `Begin`에 계정 이름, 서버 프로세스 번호와 함께 온다. enforce 정책에서는 `TimeZone`·`DateStyle`·`IntervalStyle`이 채워져 있다.
  - `TestReplicationIsRefused`: `0A000`과 문구.
  - `TestBrokenAuthEndsOnlyThatSession`: 길이 칸이 1GiB인 메시지, SASL 응답 자리의 Query, 다른 방식 이름, `p=`의 요청, 한 바이트씩 보내다 멈춘 클라이언트(제한을 줄인다) → `28000`이나 제한 뒤의 종료. 그동안 다른 세션은 로그인한다. 프록시의 메모리가 메시지 크기만큼 늘지 않는다(본문을 읽기 전에 거절한다).
  - `TestLoginInterruptedLeavesNothing`: 답하지 않는 가짜 `Login.Open` 도중의 `Close` → 곧 돌아온다. 증명을 보낸 직후 끊은 클라이언트 → 업스트림 세션이 남지 않는다(`pg_stat_activity`). 로그인 직후 끊는 가짜 업스트림 → 클라이언트의 세션이 끝난다.
  - `TestUpstreamLoginFailureIsNotExplained`: 저장된 비밀번호가 틀린 계정 → 클라이언트는 `08001`과 스펙의 문구만 받고, 로그에 `wrong password` 갈래가 있다.
  - `TestAccountNamesAreComparedByteForByte`: `Stet_Scram`은 모르는 계정이다. 제어 문자가 든 이름은 로그에 걸러져 적힌다.
  - `TestNoSecretInTheProxyLog`: 위의 성공과 거절을 디버그 수준으로 돌린 로그 전체에 토큰, 검증값, 진짜 비밀번호가 없다.
  - `TestLoginLatencyBudget`(`latency_test.go`에 새 함수): 보관을 켠 세션의 `select 1`이 직접(TLS) 붙은 것보다 p95 2ms 이내로 더 든다. 로그인 한 번에 든 시간의 중앙값을 로그에 낸다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령과 `latency_test.go`가 통과한다. 보고서에 지연과 로그인 시간의 줄을 적는다.
- [ ] **Step 5:** 커밋 `Proxy: authenticate agents and open their upstream sessions`.

---

### Task 7: `stet proxy` — 보관 켜기와 시작할 때의 확인

**Files:** Create `cmd/stet/custody.go`. Modify `cmd/stet/main.go`, `cmd/stet/transport.go`. Test `cmd/stet/custody_test.go`.

**Interfaces — Consumes:** `config.Config.ListenTLS`, `Credentials` (Task 4), `custody.OpenStore`, `NewLogin`, `Login.Check` (Task 2, 3), `proxy.Server.ClientTLS`, `Login` (Task 5, 6), `pgtest.LocalCert`.

**Interfaces — Produces:**

```go
// certReloader serves the certificate at certPath and keyPath, and reads
// them again when either file changes; a reload that fails keeps the
// certificate in use and logs the error.
func certReloader(certPath, keyPath string, log *slog.Logger) (*tls.Config, error)
```

`run`(과 `runWith`)이 하는 일, 순서대로:

1. `cfg.ListenTLS`가 있으면 `certReloader`. 읽지 못하거나 짝이 맞지 않으면 시작하지 않는다. 키 파일을 소유자 말고 다른 사용자가 읽을 수 있으면 경고한다. 돌려주는 설정은 TLS 1.2 이상이다.
2. `cfg.Credentials`가 있으면 `custody.OpenStore`. 읽지 못하면 시작하지 않는다. `cfg.AgentRoles`가 비어 있으면 파일의 계정들로 채운다. 적혀 있으면 파일의 계정들과 같아야 한다. 다르면 `agents.roles and the credentials file name different accounts: only in agents.roles: […]; only in the file: […]`로 시작하지 않는다(보정 V1). 계정이 하나도 없으면 시작하지 않는다(`the credentials file has no account; add one with stet agent add`). 이것은 게이트를 열기 전에 한다. 지금의 에이전트 계정 확인이 이 목록으로 돈다.
3. 계정마다 `Login.Check`(계정마다 5초, 합쳐 30초). 되지 않으면 계정과 갈래를 경고하고 시작은 한다. 데이터베이스는 `upstream`의 것이다.
4. `listen_tls`가 있고 보관이 없고 업스트림도 TLS면 스펙 3장의 경고를 낸다.
5. `stet proxy listening` 줄에 `listen_tls=on|off`, `custody=on|off`를 더한다. 기존 키는 그대로다.
6. `proxy.Server`의 `ClientTLS`와 `Login`을 채운다.

- [ ] **Step 1: 실패하는 테스트**(통합. 자기 데이터베이스와 자기 에이전트 역할을 만든다).
  - `TestCustodyEndToEnd`: 섀도, `listen_tls`, `agents.credentials`. `psql`과 pgx가 토큰과 `sslmode=verify-full`로 붙어 게이트 대상 테이블의 한 행을 고친다. 판단이 기록되고 `login_role`이 그 계정이다. 서버 쪽에서 TLS다. 로그에 경고가 없다.
  - `TestCustodyEnforce`: enforce에서 한도를 넘는 쓰기가 `ST001`로 막히고, 검토자가 승인하면 실행된다. 낡은 쓰기 탐지(`stale`)가 읽고 쓴 행에 `fresh`를 기록한다.
  - `TestTokenRotationNeedsNoRestart`: 띄운 채로 파일의 토큰을 바꿔 쓴다(`custody.Write`). 열려 있는 세션은 계속 문장을 실행하고, 예전 토큰의 새 로그인은 `28P01`, 새 토큰은 된다. 띄운 뒤에 파일에 더한 계정은 `28P01`이고 경고가 한 번이다.
  - `TestRolesMustMatchTheFile`: 한쪽에만 있는 계정 → 시작하지 않고 위의 문구에 그 이름이 있다. 파일에 계정이 없음 → 시작하지 않는다. 권한 `0644`인 파일 → 시작하지 않는다.
  - `TestStartCheckWarnsOfAnAccountThatCannotLogIn`: 저장된 비밀번호가 틀린 계정 하나 → 경고에 계정과 `wrong password`가 있고, 다른 계정은 로그인한다.
  - `TestCertificateReload`: 인증서 파일을 다른 것으로 바꾸면 다음 인사에 새 인증서가 온다. 깨진 파일로 바꾸면 쓰던 것이 오고 에러 로그가 있다. 시작할 때 짝이 맞지 않으면 시작하지 않는다.
  - `TestListeningLineSaysCustody`: `listen_tls=on custody=on`, 둘 다 없으면 `off`, `off`.
  - `TestWarnsOfTLSOnBothSidesWithoutCustody`: 스펙 3장의 경고.
  - `TestNoSecretInAnyLog`: 위의 시나리오들을 `-v`로 돌린 로그와 표준 출력 전체(판단 줄 포함)에 토큰, 검증값, 진짜 비밀번호가 없다. `stet.decisions`의 글자 컬럼에도 없다.
  - `TestWithoutCustodyNothingChanges`: `listen_tls`와 `agents.credentials`가 없는 설정에서 `stet proxy listening` 줄의 기존 키와 값이 이 단계 전과 같고, 날 프로토콜 클라이언트가 SSLRequest에 `N`을 받는다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `stet proxy: client TLS and credential custody`.

---

### Task 8: `stet agent`

**Files:** Create `cmd/stet/agent.go`. Modify `cmd/stet/main.go`, `go.mod`, `go.sum`. Test `cmd/stet/agent_test.go`.

**Interfaces — Consumes:** `custody.Read`, `Write`, `File`, `Agent`, `TryPassword` (Task 2, 3), `scram.NewToken` (Task 1), `config.Load`, `printable`(앞 단계의 `init`이 쓰는 것).

**Interfaces — Produces:**

```go
// agentIO is what stet agent reads from and writes to.
type agentIO struct {
	stdin    io.Reader
	stdout   io.Writer
	terminal bool                       // stdin is a terminal it can prompt on
	env      func(string) string
	secret   func(prompt string) (string, error) // reads without echo; nil: read a line from stdin
	now      func() time.Time
}

// runAgent is stet agent.
func runAgent(args []string, a agentIO) error
```

`main`은 `secret`을 `golang.org/x/term`의 `ReadPassword`로, `terminal`을 표준 입력이 터미널인지로 채운다. 명령은 스펙 7장 그대로다(`add`, `token`, `password`, `remove`, `list`, 플래그 `-config`, `-password-env`). 설정은 `config.Load`로 읽고 `Credentials`가 비어 있으면 `the config has no agents.credentials; add it first`로 멈춘다. 파일이 없으면 `add`가 만든다(디렉터리는 만들지 않는다). 계정 이름의 조건은 `custody.Read`의 것과 같다. `add`와 `password`의 확인은 `custody.TryPassword`(5초, 데이터베이스는 `upstream`의 것)이고, 실패하면 갈래와 서버의 메시지(걸러서)를 보이고 아무것도 쓰지 않는다.

출력(표준 출력, 영어):

- `add`의 끝:
  ```
  token for "<계정>" (shown once; stet keeps only what it needs to check it):
    <토큰>

  next:
    - in the agent's connection string, put this server's address as the host and the token as the password; the user stays "<계정>"
    - if anything else knew this account's database password, change it in the database, then run: stet agent password <계정>
    - restart stet proxy: an account added while it runs is not accepted
  ```
  `-password-env`면 한 줄을 더한다: `stet proxy must have <NAME> in its environment`.
- `token`의 끝: 같은 토큰 블록과 `the old token stops working at the next login; no restart is needed`.
- `password`의 끝: `password for "<계정>" updated; no restart is needed`. `password_env`인 계정이면 파일은 바꾸지 않고 `"<계정>" takes its password from <NAME>; change it in stet proxy's environment and restart stet proxy`.
- `remove`의 끝: `"<계정>" removed; it is refused from the next login`.
- `list`: 머리 줄과 계정마다 한 줄(`ACCOUNT  PASSWORD  ADDED  TOKEN ISSUED`. `PASSWORD`는 `file`이나 `env <NAME>`. 시각은 UTC `2006-01-02 15:04`). 계정이 없으면 `no accounts`.
- 에러: 이미 있는 계정의 `add` → `"<계정>" is already there; use stet agent token or stet agent password`. 없는 계정의 `token`·`password`·`remove` → `no account "<계정>"`. 명령이 없거나 모르는 명령, 계정 인자가 없거나 둘 이상 → 사용법.

- [ ] **Step 1: 실패하는 테스트**(통합. 자기 역할을 만든다).
  - `TestAgentAdd`: 표준 입력으로 비밀번호를 준 `add` → 파일이 생기고(`0600`) 그 계정의 검증값과 비밀번호가 있다. 출력의 토큰으로, 그 파일로 띄운 Stet에 로그인된다. 토큰은 출력에 한 번만 나온다.
  - `TestAgentAddRefusesAWrongPassword`: 틀린 비밀번호 → 에러에 갈래가 있고 파일이 생기지 않는다(있던 파일은 바이트가 그대로다).
  - `TestAgentAddPasswordEnv`: `-password-env`로 더하면 파일에 변수 이름만 있고 값이 없다. 변수가 비어 있으면 멈춘다.
  - `TestAgentToken`, `TestAgentPassword`, `TestAgentRemove`, `TestAgentList`: 7장의 줄마다. `token` 뒤에 예전 검증값이 파일에 없다. `password`는 틀린 값이면 아무것도 바꾸지 않는다. `list`에 토큰·검증값·비밀번호가 없다.
  - `TestAgentPromptsWithoutEcho`: `terminal`이 참이면 `secret`이 한 번 불리고 그 값이 쓰인다. 거짓이면 표준 입력의 첫 줄(끝의 줄바꿈을 뗀다)이 쓰인다.
  - `TestAgentKeepsTheFileMode`: `0400`으로 바꿔 둔 파일에 `token` → 여전히 `0400`이다.
  - `TestAgentOutputShowsTheTokenOnce`: 모든 명령의 출력과 에러에 진짜 비밀번호와 검증값이 없고, 토큰은 `add`와 `token`의 출력에 한 번씩만 있다. 닿지 않는 `upstream`, 틀린 비밀번호의 에러에 주소 전체가 없다.
  - `TestAgentStops`: `agents.credentials`가 없는 설정, 이미 있는 계정, 없는 계정, 64바이트 이름, 인자 없음.
  - `usage`에 `agent`가 있다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. `go get golang.org/x/term`(이 Task가 더하는 유일한 의존성이다. 모듈 캐시에 없어 내려받아야 하면 그렇게 하고 보고서에 버전을 적는다).
- [ ] **Step 4:** 전체 명령이 통과한다. `go mod tidy` 뒤에 `go.mod`의 `require`에 더해진 것이 `golang.org/x/term`과 그것이 끌어온 것뿐인지 본다.
- [ ] **Step 5:** 커밋 `Add stet agent: issue tokens and keep the accounts' passwords`.

---

### Task 9: 문서 — 놓는 법, 설정 예, 만든 대로의 스펙

**Files:** Create `docs/deploy.md`. Modify `docs/quickstart.md`, `stet.example.yaml`, `docs/spec-v0.1.md`(9장), `docs/spec-v0.2-server.md`.

- [ ] **Step 1: `docs/deploy.md`**(한국어). 순서: 무엇이 달라지는가(에이전트는 토큰만 안다) → 놓는 자리와 네트워크 → 인증서(`listen_tls`. 사설 CA나 자기 서명 인증서를 에이전트 쪽 `sslrootcert`로 주는 법) → 설정(`agents.credentials`, 보관이 요구하는 것 둘) → `stet agent add` → 에이전트의 접속 문자열(`sslmode=verify-full`을 쓰는 이유) → 예전 비밀번호 바꾸기 → 토큰과 비밀번호 돌리기, 다시 띄워야 하는 것 → 로그에서 보는 것 → 놓는 법 둘: 서비스 계정의 바이너리, 디렉터리를 마운트한 Docker 이미지 → Kubernetes의 비밀 볼륨(돌려 보지 않았다고 적는다) → 한계(스펙 12장 전부).
- [ ] **Step 2: 그대로 따라 한다.** 시험 서버에 자기 데이터베이스와 에이전트 역할을 만들고, 문서의 두 가지 놓는 법을 처음부터 끝까지 그대로 친다. 바이너리는 `go build`한 것, Docker는 이 기기에서 빌드한 이미지다(올리지 않는다. 끝나면 이미지와 컨테이너를 지운다). 토큰으로 붙은 `psql`의 쓰기가 판단되는 것을 본다. 다르면 문서나 코드 가운데 틀린 쪽을 고친다(코드를 고칠 일이면 멈추고 보고한다). 끝나면 만든 것을 모두 지운다. 보고서에 친 명령과 본 출력을 적는다(토큰과 비밀번호는 가린다).
- [ ] **Step 3: `stet.example.yaml`, `docs/spec-v0.1.md` 9장, `docs/quickstart.md`.** `listen_tls`, `agents.credentials`와 그 조건. quickstart의 "통제가 아니다"에서 `deploy.md`로 잇는다. `cmd/stet`의 예제 설정 테스트가 그대로 통과한다.
- [ ] **Step 4: `docs/spec-v0.2-server.md`.** 상태 줄을 "구현"으로 바꾸고, 보정 V1~V5와 진행 중의 판단 가운데 스펙을 바꾼 것을 해당 장에 옮겨 적는다. 14장의 수용 기준을 근거(테스트 이름, Task 6의 지연과 로그인 시간, Step 2의 결과)와 함께 체크한다. 확인하지 못한 것(다른 클라이언트, 매니지드 서비스)은 체크하지 않고 이유를 적는다. 이 기기에 `psycopg`나 `node-postgres`, JDBC가 있으면 로그인만 해 보고 결과를 적는다. 없으면 없다고 적는다.
- [ ] **Step 5:** 소유자의 문서에서 낡는 줄을 찾아(고치지 않는다) 보고서에 파일과 줄 번호로 적는다.
- [ ] **Step 6:** 전체 명령이 통과한다. 커밋 `Docs: deploying stet on a server, and the server spec as built`.
