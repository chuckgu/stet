# enforce 최소형 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `mode: enforce`에서 에이전트 계정의 쓰기를 판정에 따라 보내거나, 보내지 않고 에이전트가 읽고 고칠 수 있는 에러를 돌려준다. 섀도의 판정은 같은 규칙으로 맞춘다.

**Architecture:** 프록시는 에이전트 계정의 세션만 따라가며, 트랜잭션이 깨끗한지와 서버가 확인한 문장·포털 표를 들고 있다. 게이트는 enforce에서 `Begin` 안에서 판정을 끝내고(결정론적 규칙이 먼저, 남을 때만 모델) 보내지 말아야 하면 에러를 돌려준다. 프록시는 그 메시지를 서버에서 반드시 실패하는 같은 종류의 메시지로 바꿔 보내고, 서버가 낸 그 에러 하나의 내용만 바꿔 쓴다. 보류한 쓰기는 `stet.held_writes`에 남는다.

**Tech Stack:** Go 1.27, `github.com/jackc/pgx/v5` v5.11.0 (`pgconn`, `pgproto3`, `pgxpool`), `github.com/pganalyze/pg_query_go/v6` v6.2.5 (PostgreSQL 17 문법), 테스트 서버 PostgreSQL 14 (`scripts/testdb.sh`).

**Spec:** [docs/spec-v0.2-enforce.md](spec-v0.2-enforce.md). 앞 문서는 [spec-v0.1.md](spec-v0.1.md) 7·10장, [spec-v0.2-impact.md](spec-v0.2-impact.md), [spec-v0.2-accounting.md](spec-v0.2-accounting.md)다. 스펙과 이 계획이 다르면 아래 "스펙 보정"에 적힌 곳만 계획을 따르고, 나머지는 스펙을 따른다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 규칙 1은 Task 0에서 스펙 4장의 문구로 바뀐다: "**모델을 기다리는 동안 잠금이나 미확정 변경을 쥐지 않는다.** Stet은 모델을 부를 때 자기 트랜잭션을 열어 두지 않고, 이미 무언가를 바꾼 클라이언트 트랜잭션을 세워 두지 않는다. 판단이 끝난 뒤에만 실행한다."
- 규칙 2·3·4·7은 그대로다: 모델 버전 고정, `stet.decisions`는 append-only, 재현할 때 재추론하지 않음, 모델 인터페이스(`decide.Decider`)는 바꾸지 않는다.
- 규칙 5: 결정론적 규칙(형태, 트랜잭션, 접속 계정, 영향 한도, 누적 한도, 측정 실패)을 모델보다 먼저 적용한다. enforce에서 여기서 판정이 나면 모델을 부르지 않는다.
- 규칙 6: 섀도 모드는 전달되는 바이트를 고치거나 늦추거나 순서를 바꾸지 않는다. 섀도 모드와 감시하지 않는 세션은 이 계획이 더하는 경로(메시지 바꿔 보내기, 에러 바꿔 쓰기, 닫힌 지점, 세션 끊기)를 하나도 타지 않는다.
- 프록시가 지금 지키는 것을 그대로 지킨다: 세션 잠금(`session.mu`)을 쥔 채 게이트를 부르지 않는다. `End`와 `Observe`는 문장마다 정확히 한 번 불린다. 결과 행 메시지(`D`, `d`)마다 하는 일은 정수 하나를 올리는 것뿐이다.
- 스펙의 값. SQLSTATE: `ST001`(hold, escalate), `ST002`(unsupported, 닫힌 지점), `ST003`은 내지 않는다. 새 reason: `no_login_role`, `impact:cumulative_unknown`, `concurrent_change`, `impact:exceeded_after`, `rejudged`. `in_transaction`은 "더러운 트랜잭션 안"이라는 새 뜻이다. 설정 키: `agents.roles`, `limits.max_inflight`(기본 32), `model.on_failure`(enforce 기본 `hold`, 섀도 기본 `pass`). 상태 읽기 상한 500ms(기존 `stateTimeout`), `held_writes` 삽입 상한 500ms, 검사 상한 64MiB(기존 `maxInspect`), 바꿔 보내는 이름은 63바이트 이하의 무작위 값, `held_writes.status`는 `pending` 또는 `undelivered`, 판단 기록의 새 컬럼은 `refused`.
- 깨끗한 트랜잭션: 서버가 유휴라고 답한 마지막 ReadyForQuery 뒤로 클라이언트가 보낸 문장이 `BEGIN`, `START TRANSACTION`, `SET`, `SHOW`뿐인 상태다. 서버가 아직 답하지 않은 문장도 센다.
- 에러 메시지는 영어다. 다른 행의 값을 넣지 않는다(중복 후보는 키만).
- 테스트: `scripts/testdb.sh start <dir>` 뒤 `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다. 모든 Task는 이 명령이 모든 패키지에서 `ok`인 채로 끝난다.
- 테스트 계정: Stet 자신의 계정은 `stet_scram`, enforce 테스트의 에이전트 계정은 `stet_md5`(비밀번호 `md5-pw`)다. `stet_trust`는 비밀번호 없는 원시 프로토콜 테스트에 쓴다. **`stet_trust`를 어떤 역할에도 부여하지 않는다**(`GRANT stet_trust TO ...` 금지. 기존 테스트가 그 부재에 기대고 있다). 테이블 권한을 `stet_trust`에게 주는 것은 된다.
- 테스트 서버는 PostgreSQL 14이고 분류기 문법은 17이다. 서버 동작에 기대는 단언은 14에서 확인된 것만 쓴다.
- `internal/proxy/latency_test.go`의 2ms 예산(p95)을 지킨다. 그 파일의 기존 테스트는 고치지 않고 새 테스트만 더한다.
- 기존 테스트의 단언은 Task가 이름을 적어 허락한 곳만 고친다. 테스트용 게이트(`recordingGate`, `fakeGate` 등)를 바뀐 `Gate` 인터페이스에 맞추는 것과 헬퍼의 호출 인자를 맞추는 것은 단언 변경이 아니다.
- `internal/lab`은 고치지 않는다(스펙의 비목표). `decide.Input`과 `decide.Verdict`의 기존 필드와 뜻은 lab이 쓰므로 그대로 둔다.
- 코드의 주석과 식별자는 영어, 문서와 YAML·SQL 스키마 주석은 한국어다.
- `main`에 직접 커밋하지 않는다. 브랜치 `v0.2-enforce`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고, 커밋한 모델의 `Co-Authored-By` 줄을 붙인다.

## Review Focus

스펙이 암시하지만 놓치기 쉬운 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **드라이버가 실제로 보내는 모양.** 이름 붙은 준비된 문장(Parse·Describe·Sync 뒤에 Bind·Describe·Execute·Sync)으로 보낸 쓰기를 두 번 막은 뒤에도 그 준비된 문장으로 통과하는 쓰기가 실행된다. `COPY ... FROM STDIN`을 막으면 클라이언트는 CopyInResponse를 기다리며 멈추지 않고 에러를 받는다. Parse·Describe 뒤에 Sync가 아니라 Flush를 보내고(asyncpg의 `prepare`가 이렇게 보내는 것으로 알려져 있다) 곧바로 단순 Query를 보내는 세션에서도 응답이 제 문장에 맞는다. (Task 7: `TestRefusedExecuteLeavesThePreparedStatementUsable`, `TestRefusedCopyFromStdinDoesNotHang`, Task 9: `TestQueryBeforeSyncIsFollowed`)
2. **바이너리 바인딩 값과 NULL이 든 보류 건.** pgx는 정수를 바이너리로 보낸다. `held_writes.params`에는 텍스트로 풀린 값이 들어가고 NULL은 NULL로 남으며, `param_types`는 클라이언트가 Parse에서 준 그대로다(안 줬으면 0). (Task 11: `TestHeldWriteKeepsBinaryAndNullParamsAsText`)
3. **막힌 쓰기를 에이전트가 곧바로 다시 보낸다.** 막힌 쓰기는 누적 한도의 예약에서 빠지므로, 같은 쓰기를 여러 번 다시 보내도 그 계정의 다른 쓰기가 `impact:cumulative`에 걸리지 않는다. 다시 보낼 때마다 보류 줄이 하나씩 생긴다. (Task 11: `TestRetriedHeldWriteDoesNotUseUpTheCumulativeLimit`)
4. **서버의 에러를 덮어쓰면 안 되는 경우.** `commit`으로 판정됐지만 서버가 거부한 쓰기(UNIQUE 위반)는 서버의 에러가 그대로 가고 `refused`는 NULL이다. 막은 Execute 앞의 Bind가 먼저 실패하면 클라이언트는 자기 Bind 에러를 받고 보류 줄은 `undelivered`가 된다. (Task 7: `TestBindFailureBeforeARefusedExecuteIsLeftAlone`, Task 11: `TestEnforceLeavesTheServersOwnErrorAlone`)
5. **서버가 읽는 대로 읽지 않으면 감시를 벗어나는 이름.** 시작 패킷에 `user`가 두 번 오면 서버는 마지막 것을 쓴다. 63바이트가 넘는 계정 이름은 테스트 서버(14)가 63바이트로 잘라 쓴다. 준비된 문장과 포털의 이름도 앞 63바이트로 비교된다. 앞 Sync의 Parse가 확인되기 전에 그 이름을 실행하는 파이프라인도 같은 종류다. (Task 5: `TestStartupUserIsReadAsTheServerReadsIt`, Task 6: `TestNamesAreComparedByTheirFirst63Bytes`, Task 9: `TestUnsureNameIsRefused`)

## 스펙 보정

코드와 PostgreSQL 14.18에 맞춰 보니 스펙대로는 되지 않거나 스펙이 정하지 않은 곳이다. 이 계획이 가장 작은 보정을 넣었고, Task 13이 스펙에 옮겨 적는다.

| # | 스펙 | 확인한 것 | 보정 (Task) |
|---|---|---|---|
| B1 | 6장 "어느 에러가 어느 메시지의 것인지는 지금도 Execute 순번으로 맞추고 있다" | Execute 순번은 막은 Execute 바로 앞의 Bind·Parse·Describe가 낸 에러를 그 Execute의 에러와 구별하지 못한다 | 단위 안의 메시지 순번과 서버의 완료 응답 수로 가린다 (6, 7) |
| B2 | 6장은 포털 이름의 63바이트만 말한다 | 준비된 문장 이름도 앞 63바이트로 비교된다. `s…s`(63자)+`A`로 Parse한 쓰기를 `s…s`+`B`로 Bind하면 실행된다 | 두 표의 키를 앞 63바이트로 둔다 (6) |
| B3 | 8장 "시작 패킷의 `user`" | `user`가 두 번 오면 서버는 마지막 것으로 인증한다. 63바이트가 넘는 이름은 서버가 잘라 쓴다(14에서 확인). 자르지 않는 서버에서는 그런 이름으로 로그인할 수 없으므로, 자르는 쪽이 어느 쪽에서나 맞다 | 마지막 값을 쓰고 63바이트로 자른다 (5) |
| B4 | 6장 "같은 Sync 안에서 뒤따르는 Bind와 Execute는 새 뜻으로 본다" | Sync가 다르면 성립하지 않는다. 앞 Sync의 Parse가 실패해도 뒤 Sync의 Bind는 건너뛰어지지 않고 옛 문장에 묶인다 | 아직 답 없는 앞 단위의 변경이 걸린 이름은 뜻이 둘 이상이다. 그 가운데 감시 대상이 있으면 enforce에서 막는다 (6, 9) |
| B5 | 7장의 닫힌 지점 | SQL `EXECUTE s1`은 프로토콜 Parse로 준비한 쓰기를 실행한다. 트랜잭션 안에서 SQL `FETCH`·`MOVE`는 프로토콜 Bind로 만든 `UPDATE ... RETURNING` 포털을 실행한다. 분류기에는 둘 다 테이블이 보이지 않는다 | 세션의 표에 감시 대상으로 적힌 이름을 `EXECUTE`·`FETCH`·`MOVE`하면 enforce에서 막는다 (9) |
| B6 | 6장의 표 맞추기는 프록시가 응답을 메시지에 정확히 맞춘다고 전제한다 | 지금의 프록시는 단위와 서버의 ReadyForQuery가 어긋나는 순서가 있다. ① Sync 전에 온 Query·FunctionCall: 앞의 확장 메시지가 모두 성공했으면 서버는 그 Query에만 ReadyForQuery를 보내고, 실패했으면 다음 Sync까지 Query도 삼킨다. ② 확장 프로토콜의 `COPY FROM STDIN`: 서버가 복사 중의 Sync를 무시한다. ③ 단순 프로토콜 복사 중의 Sync도 무시한다. 어긋나면 ParseComplete가 다른 Parse의 것으로 읽혀 표가 서버와 달라진다 | ①은 서버가 하는 대로 따라간다(두 모드 모두). ②와 ③은 enforce의 에이전트 세션에서 세션을 끊는다. 확장 프로토콜로 `COPY FROM STDIN`을 보내는 클라이언트(tokio-postgres의 `copy_in`이 그렇다고 알려져 있다)는 enforce에서 끊긴다: 13장에 한계로 적는다 (9) |
| B7 | 4장 "CLAUDE.md는 구현의 첫 작업에서 고친다" | 구현하는 에이전트는 CLAUDE.md를 읽고 일한다. 지금 문구로는 Task 11의 코드가 규칙 위반이다 | Task 0에서 고친다 |
| B8 | 9장 `undelivered`의 조건 | 에러를 바꿔 쓴 뒤 클라이언트에 실제로 써졌는지 프록시는 모른다 | 그 단위의 ReadyForQuery까지 간 경우만 전달된 것으로 본다. 그 전에 연결이 끝났으면 `undelivered`다 (7) |
| B9 | 12장 "초 단위로 묶어 읽는다", "간격을 두고 다시 시도" | 간격과 칸의 경계가 정해져 있지 않다 | 다시 시도 간격은 5초. 초 칸은 그 초 전체가 창 밖으로 나가야 빠진다(최대 1초 더 센다) (10) |
| B10 | 7장 닫힌 지점은 "대상 테이블과 상관없이" | 게이트 대상 테이블이 하나도 없으면 게이트가 없다 | 그 설정에서는 enforce에서도 프록시가 통과만 한다 (12) |

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/config/config.go` | `agents.roles`, `limits.max_inflight`, enforce의 기본값과 검증 (수정) |
| `internal/store/schema.sql`, `store.go` | `stet.decisions.refused`, 초 단위 사용량 질의 (수정) |
| `internal/store/held.go` | `stet.held_writes` 쓰기 (신규) |
| `internal/sqlparse/classify.go`, `measure.go` | 깨끗한 문장, `DO`, 값 하나짜리 `IN`, 이름으로 실행하는 문장, 한정된 열 이름 (수정) |
| `internal/decide/decide.go`, `impact.go`, `cumulative.go` | 새 규칙과 순서 (수정) |
| `internal/decide/act.go` | 판정을 행동과 에러 문구로 옮기는 표 (신규) |
| `internal/proxy/proxy.go` | 감시할 세션 고르기, 메시지 하나 바꿔 쓰기, 세션 끊기 (수정) |
| `internal/proxy/startup.go` | 시작 패킷을 서버처럼 읽기 (신규) |
| `internal/proxy/names.go` | 서버가 확인한 문장·포털 표 (신규) |
| `internal/proxy/refuse.go` | 바꿔 보내는 메시지와 에러 바꿔 쓰기 (신규) |
| `internal/proxy/session.go` | 트랜잭션 깨끗함, 메시지 순번, 닫힌 지점 (수정) |
| `internal/gate/gate.go`, `impact.go` | 한 벌의 판정 규칙, 동시 상한, 열 이름 확인 (수정) |
| `internal/gate/usage.go` | 초 단위 사용량, 묶어 읽기, 다시 시도 간격 (다시 씀) |
| `internal/gate/enforce.go` | `Begin`에서 판정하고 적용하기, 행 버전 다시 확인, 보류 저장 (신규) |
| `cmd/stet/main.go`, `cmd/stet/check.go` | 실행부 분리, 시작할 때의 확인 (수정, 신규) |

---

### Task 0: 스펙, 계획, 규칙 1

**Files:** Modify `CLAUDE.md`. Add `docs/spec-v0.2-enforce.md`, `docs/plan-v0.2-enforce.md`.

- [ ] **Step 1:** `git switch v0.2-enforce` (이미 있다). `git status`에 스펙과 계획만 추적되지 않은 파일로 보여야 한다.
- [ ] **Step 2:** CLAUDE.md의 규칙 1을 Global Constraints 첫 줄에 인용한 문구로 바꾼다. 다른 줄은 건드리지 않는다.
- [ ] **Step 3:** 커밋한다.

```bash
git add CLAUDE.md docs/spec-v0.2-enforce.md docs/plan-v0.2-enforce.md
git commit -m "Spec and plan: minimal enforce; reword rule 1 as the spec proposes"
```

- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`가 모든 패키지에서 `ok`인지 확인한다.

---

### Task 1: 설정

**Files:** Modify `internal/config/config.go`. Test `internal/config/config_test.go`.

**Interfaces — Produces:**

```go
// in Config:
AgentRoles  []string // agents.roles: the login roles whose sessions Stet watches. Empty (shadow only): every session
MaxInflight int      // limits.max_inflight: writes being judged at once; default 32
// Model.OnFailure: when the file leaves it out, "hold" in enforce and "pass" in shadow.
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestEnforceDefaults`: `mode: enforce`, `agents: {roles: [agent_a, agent_b]}`, jev 모델, 테이블 하나 → `OnFailure == "hold"`, `AgentRoles == [agent_a agent_b]`, `MaxInflight == 32`. 같은 설정에 `on_failure: pass`를 적으면 `pass`. `mode: enforce` + `model: {adapter: none, on_failure: pass}`는 유효하다.
  - `TestAgentRolesAndLimits`: 섀도에서 `agents`가 없으면 `AgentRoles == nil`, 있으면 그대로. `limits: {max_inflight: 4}` → 4. 기존 `TestDefaults`(섀도 `on_failure` 기본 `pass`)는 고치지 않고 통과한다.
  - `TestInvalidConfigs`에 행을 더한다.

| 이름 | 설정 | 에러에 들어갈 문자열 |
|---|---|---|
| enforce needs agent roles | `mode: enforce`, `agents` 없음 | `agents.roles` |
| enforce with no roles listed | `mode: enforce`, `agents: {roles: []}` | `agents.roles` |
| empty role name | `agents: {roles: [""]}` | `agents.roles` |
| role listed twice | `agents: {roles: [a, a]}` | `agents.roles` |
| role name over 63 bytes | 64바이트 이름 | `agents.roles` |
| Stet's own account listed | `upstream: postgres://app@…`, `agents: {roles: [app]}` | `agents.roles` |
| record-only model in enforce, on_failure left out | `mode: enforce`, 역할 있음, `model: {adapter: none}` | `model.on_failure` |
| record-only model in enforce, on_failure hold | 위에 `on_failure: hold` | `model.on_failure` |
| max_inflight zero | `limits: {max_inflight: 0}` | `limits.max_inflight` |
| agents unknown key | `agents: {role: [a]}` | `role` |
| limits unknown key | `limits: {inflight: 4}` | `inflight` |

- [ ] **Step 2:** `go test ./internal/config/ -run 'Enforce|AgentRoles|InvalidConfigs'` → FAIL (컴파일: `AgentRoles` 없음).
- [ ] **Step 3: 구현.** `rawConfig`에 `Agents *struct{ Roles []string }`와 `Limits *struct{ MaxInflight *int }`를 더한다(`max_inflight: 0`을 없는 것과 구분). `on_failure`를 적었는지는 `rawModel.OnFailure == ""`로 안다. Stet 자신의 계정은 `upstream` URL에 사용자가 있을 때만 여기서 비교한다. URL에 없으면 Task 12의 시작 확인이 본다. 역할 이름은 대소문자까지 그대로 비교한다.
- [ ] **Step 4:** `go test ./internal/config/` → PASS (`TestLoadExampleConfig`, `TestLabConfigIsValid` 포함).
- [ ] **Step 5:** 커밋 `Config: agent roles, the inflight limit, and enforce defaults`.

---

### Task 2: 저장소

**Files:** Modify `internal/store/schema.sql`, `store.go`. Create `internal/store/held.go`, `internal/store/held_test.go`. Test `internal/store/store_test.go`.

**Interfaces — Produces:**

```go
// in Decision:
Refused *string // the SQLSTATE Stet answered with instead of sending the statement; nil when it was sent

// HeldWrite is one row of stet.held_writes. Nil fields are stored as NULL.
type HeldWrite struct {
	ID, DecisionID                              string
	CreatedAt                                   time.Time // when the statement arrived
	LoginRole, Database, Statement, TargetTable string
	Params                                      []*string // bind values as text
	ParamTypes                                  []uint32  // the types the client gave in Parse
	TargetPK, RowVersion                        *string
	Verdict                                     string // hold | escalate
	Reasons                                     []string
}
func (s *Store) InsertHeldWrite(ctx context.Context, h *HeldWrite) error
// MarkUndelivered sets a pending held write to undelivered. An id that is
// unknown or already undelivered is not an error.
func (s *Store) MarkUndelivered(ctx context.Context, id string) error

// UsageSecond is the rows one role's executed writes changed in a table within one second.
type UsageSecond struct {
	Second time.Time // start of the second
	Rows   int64
}
// UsageBySecond sums executed writes by role to table per second, for
// created_at in [since, before), oldest first. Seconds with no rows are left out.
func (s *Store) UsageBySecond(ctx context.Context, role, table string, since, before time.Time) ([]UsageSecond, error)
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestInsertRecordsRefused`: `sample`에 `Mode: "enforce"`, `Refused: ptr("ST001")`, `Executed: false` → 읽으면 `mode = enforce`, `refused = ST001`. `sample` 그대로 넣으면 `refused`는 NULL.
  - `TestInsertHeldWrite`: 모든 필드를 채워 넣고 읽는다. `Params: {ptr("CTO"), nil}` → `params = ["CTO", null]`. `ParamTypes: {25, 0}` → `{25,0}`. `status = 'pending'`. `Params`, `ParamTypes`, `TargetPK`, `RowVersion`이 nil이면 NULL.
  - `TestMarkUndelivered`: `pending` → `undelivered`. 다시 불러도, 없는 id로 불러도 에러가 없다.
  - `TestUsageBySecond`: 같은 역할·테이블에 실행된 기록을 넣는다: 같은 초에 5행과 3행, 1초 뒤 2행, NULL, 0, 실행 안 된 9행, 다른 역할 9행, `since` 앞 9행, `before`와 같은 시각 9행 → `[{첫 초, 8}, {다음 초, 2}]`.
  - 기존 `TestMigrateIsIdempotentAndConcurrent`가 새 스키마로 통과한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/` → FAIL.
- [ ] **Step 3: 구현.** `schema.sql`에 `ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS refused text;`와 스펙 9장의 `CREATE TABLE IF NOT EXISTS stet.held_writes`·인덱스를 그대로(주석은 한국어) 더한다. `held_writes`에는 append-only 트리거를 두지 않는다. `MarkUndelivered`는 `status = 'pending'`인 줄만 바꾼다. `UsageBySecond`는 `date_trunc('second', created_at)`로 묶는다. 기존 `Usage`와 `TestUsage`는 Task 10이 지울 때까지 둔다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/store/` → PASS.
- [ ] **Step 5:** 커밋 `Store: held writes, the refused column, and usage grouped per second`.

---

### Task 3: 분류기

**Files:** Modify `internal/sqlparse/classify.go`, `measure.go`. Test `internal/sqlparse/classify_test.go`.

**Interfaces — Produces:**

```go
// PostgresMajor is the PostgreSQL version whose grammar Classify reads.
const PostgresMajor = 17

// in Result (set for every Kind):
Clean    bool     // every statement is BEGIN, START TRANSACTION, SET or SHOW; true for SQL with no statement
Do       bool     // some statement is a DO block
Executes []string // prepared statements run by SQL EXECUTE, wherever it sits (EXPLAIN EXECUTE, CREATE TABLE AS EXECUTE), in order
Portals  []string // portals read by FETCH or MOVE, in order

// in Measure:
// Qualified lists the names the WHERE clause qualifies by a table, alias, or
// schema.table (c.x, public.contacts.x), sorted, without duplicates; nil when
// none. Such a name may be a function called as an attribute, so the count
// runs only when each is a column of Relation.
Qualified []string
```

- [ ] **Step 1: 실패하는 테스트** (`gated` 맵 재사용).
  - `TestClassifyClean`:

| sql | Clean |
|---|---|
| `""`, `-- ping`, `begin`, `BEGIN ISOLATION LEVEL SERIALIZABLE`, `start transaction read only`, `set search_path = public`, `set local a.b = '1'`, `set transaction isolation level serializable`, `show a.b`, `begin; set local statement_timeout = 0` | true |
| `reset a.b`, `reset all`, `select 1`, `commit`, `rollback`, `savepoint s`, `discard all`, `begin; select 1`, `update contacts set title = 'x' where id = 1`, `do $$ begin end $$` | false |

  - `TestClassifyDo`: `do $$ begin update contacts set title = 'x'; end $$` → `Do`, Kind `Pass`. `select 1; do $$ begin end $$` → `Do`. `select 'do'` → 아니다.
  - `TestClassifySingleValueIn`:

| sql | Kind | Write.PK |
|---|---|---|
| `update contacts set title = 'x' where id in (5)` | Gated | Literal `5` |
| `delete from contacts where id in ($1)` | Gated | Param 1 |
| `update contacts set title = 'x' where contacts.id in (5) and status = 'a'` | Gated | Literal `5` |
| `update contacts set title = 'x' where id in ('5'::int)` | Gated | Literal `5` |
| `… where id in (5, 6)`, `… where id not in (5)`, `… where id in (null)`, `… where id = any($1)`, `… where id between 5 and 5`, `… where id = 5 or false` | Measured (`not_single_row`) | 없음 |

  - `TestClassifyRunsByName`: `execute s1(1)` → `Executes [s1]`. `explain analyze execute s1`, `create table x as execute s1` → `[s1]`. `execute "S 1"` → `[S 1]`. `fetch all from p1` → `Portals [p1]`(`Cursors`는 지금과 같다). `move all in p1` → `Portals [p1]`, `Cursors`는 지금처럼 비어 있다. `select 1` → 둘 다 nil.
  - `TestClassifyQualifiedNames`: `update contacts set status = 'x' where contacts.to_json is not null and public.contacts.company_id = 3 and status = 'a'` → Measured, `Qualified [company_id to_json]`. `update contacts c set status = 'x' where c.company_id = 3` → `[company_id]`. `… where company_id = 3` → nil. `… where c.* is not null` → nil.
- [ ] **Step 2:** `go test ./internal/sqlparse/` → FAIL.
- [ ] **Step 3: 구현.** `pkEquality`가 `A_Expr_Kind_AEXPR_IN`이고 연산자 이름이 `=`이며 오른쪽 List의 항목이 하나인 식을 `pk = 값`으로 읽는다. 값은 지금처럼 `value()`가 Literal이나 Param일 때만 받는다. 깨끗한 문장은 `TransactionStmt`의 `TRANS_STMT_BEGIN`·`TRANS_STMT_START`, `VariableSetStmt`의 `VAR_SET_VALUE`·`VAR_SET_DEFAULT`·`VAR_SET_CURRENT`·`VAR_SET_MULTI`, `VariableShowStmt`다. `RESET`은 스펙의 목록에 없으므로 깨끗하지 않다. `Qualified`는 조건식의 `ColumnRef` 가운데 필드가 둘 이상이고 마지막 필드가 이름인 것의 마지막 필드다. 기존 테스트의 단언은 고치지 않는다.
- [ ] **Step 4:** `go test ./internal/sqlparse/` → PASS. `go test -bench . -run xxx ./internal/sqlparse/`의 문장당 시간을 이 변경 전과 나란히 보고한다.
- [ ] **Step 5:** 커밋 `Classifier: clean statements, DO, single-value IN, runs by name, qualified names`.

---

### Task 4: 판정 규칙과 행동의 표

**Files:** Modify `internal/decide/decide.go`, `impact.go`, `cumulative.go`. Create `internal/decide/act.go`, `internal/decide/act_test.go`. Test `internal/decide/decide_test.go`.

**Interfaces — Produces:**

```go
// added to Input:
InTransaction bool // the write arrived in a transaction that had already run something else
NoLoginRole   bool // the session's login role is not known

// Unknown reports that the usage could not be read (Error is set).
func (c *Cumulative) Unknown() bool

// Deterministic applies the rules that need no model answer (spec 3, step 6).
// ok is false when the write's questions must be answered first. A write
// with no questions is decided here.
func Deterministic(in Input) (verdict string, reasons []string, ok bool)

const (
	CodeHeld        = "ST001"
	CodeUnsupported = "ST002"
)

// Facts are what an error message may mention. Nothing else of a write's state reaches the client.
type Facts struct {
	Table, PK, Op string
	Unsupported   string   // the table's unsupported setting: reject | pass
	Rules         []string // the table's policy rules
	Impact        *Impact
	Limit         *config.Impact
	Cumulative    *Cumulative
}

// Action is what enforce mode does with a write once its verdict is known (spec 5).
type Action struct {
	Forward bool   // send the statement; the other fields are empty
	Code    string // CodeHeld or CodeUnsupported
	Held    bool   // keep the write in stet.held_writes (verdict hold or escalate)
	Message string // the error's first line
	// unexported: the hint's template
}
func Act(verdict string, reasons []string, f Facts) Action
// Hint is the HINT line. heldWrite is the stet.held_writes id; "" when the
// write is not held or its row could not be stored.
func (a Action) Hint(heldWrite string) string
// Detail is the DETAIL line: "decision=<id> held_write=<id> reasons=<a,b>", without held_write when it is "".
func Detail(decision, heldWrite string, reasons []string) string
// Blocked is the refusal of a message the proxy does not forward and no
// decision records (spec 7). Its Code is CodeUnsupported.
func Blocked(reason string) Action
```

`Verdict`의 순서(처음 걸리는 규칙이 정한다): ① `Unsupported`(형태) → `unsupported`. ② `InTransaction` → `unsupported`, `in_transaction`. ③ `NoLoginRole` → `unsupported`, `no_login_role`. ④ 측정한 쓰기: 측정 실패 → `unsupported`(`ShapeReason`), 행 수 → `hold` `impact:rows`, 비율 → `hold` `impact:share`, 누적 초과 → `hold` `impact:cumulative`, **누적 값 모름 → `hold` `impact:cumulative_unknown`**, 추정뿐 → `escalate` `impact:uncertain`, 그 외 `commit`. ⑤ 단일 행 쓰기: 누적 초과, **누적 값 모름**, 그다음은 지금 순서 그대로(상태·모델 실패, 모델의 답).

- [ ] **Step 1: 실패하는 테스트.**
  - `TestVerdict`에 행: `Input{InTransaction: true}` → `unsupported [in_transaction]`. `Input{NoLoginRole: true}` → `unsupported [no_login_role]`. `Input{Unsupported: "multi_row", InTransaction: true, NoLoginRole: true}` → `[multi_row]`. `Input{InTransaction: true, NoLoginRole: true}` → `[in_transaction]`. 측정 값이 있는 입력에 `InTransaction` → `[in_transaction]`.
  - `TestVerdictCumulative`: **기존 행 `unknown usage is not applied`를 고친다**: `Cumulative{Error: "x"}` → `hold [impact:cumulative_unknown]`. 새 행: 측정한 쓰기에 `Cumulative{Error}` → `hold [impact:cumulative_unknown]`. 행 수 초과와 누적 모름이 함께면 `[impact:rows]`. 추정과 누적 모름이 함께면 `[impact:cumulative_unknown]`. 모델이 hold를 답했고 누적 모름이면 `[impact:cumulative_unknown]`.
  - `TestDeterministic`: 형태·트랜잭션·계정·측정·누적·`StateErr`가 정하는 입력은 `ok == true`이고 `Verdict`와 같은 값. 질문이 있고 위의 어느 것도 걸리지 않으면 `ok == false`. 질문이 없으면 `commit, ok == true`.
  - `TestAct`: 아래 표의 줄마다 `Message`와 `Hint("H1")`를 문자 그대로 확인한다. `Hint("")`는 `held_write H1` 자리에 `this write`가 온다. `commit`(이유가 `rejudged`나 `model_error`여도) → `Forward`. `unsupported`이고 `Facts.Unsupported == "pass"`면 `Forward`, 단 `in_transaction`과 `no_login_role`은 `pass`여도 막는다. `hold`와 `escalate`는 `Held`.
  - `TestActNamesOnlyTheKeyOfADuplicate`: `duplicate:4812`의 `Message`와 `Hint`에 `contacts.id=4812` 말고 행의 값이 없다.
  - `TestDetail`: `decision=D1 held_write=H1 reasons=impact:rows`. 보류 id가 없으면 `decision=D1 reasons=in_transaction`. 이유가 여럿이면 쉼표로 잇는다(`policy:1,rejudged`).
  - `TestBlocked`: 아래 닫힌 지점 표의 줄마다 `Code == "ST002"`, `Message`, `Hint("")`.

ST001의 `Message`는 `write held by stet: `, ST002는 `write refused by stet: `로 시작한다. 문구는 첫 이유로 고른다(`policy`만 걸린 이유를 모두 적는다). `{verb}`는 `inserted`·`updated`·`deleted`, `{held}`는 `held_write <id>` 또는 `this write`다.

| 첫 이유 | Message (머리말 뒤) | Hint |
|---|---|---|
| `impact:rows` | `{rows} rows would be {verb}, over the limit of {max_rows}`. 상한에서 멈춘 세기(`Capped`)면 `more than {max_rows} rows would be {verb}, over the limit of {max_rows}` | `narrow the WHERE clause, or ask a human to approve {held}`. insert면 `insert fewer rows in one statement, or ask a human to approve {held}` |
| `impact:share` | `{share×100, %.3g}% of {table} would be {verb}, over the limit of {max_share×100, %.3g}%` | `impact:rows`와 같다 |
| `impact:cumulative` | `this role has touched {used} rows of {table} in the last {window}, and {planned} more would pass the limit of {limit}` (`{window}`는 `time.Duration`의 표기, 예 `10m0s`) | `wait until earlier writes leave the window, or ask a human to approve {held}` |
| `impact:cumulative_unknown` | `the rows this role has touched in {table} could not be read, so its cumulative limit cannot be checked` | `retry shortly, or ask a human to approve {held}` |
| `impact:uncertain` | `the rows it would touch could only be estimated (about {rows})` | `use a WHERE clause of plain comparisons on the table's own columns, or ask a human to approve {held}` |
| `state_error` | `the state needed to judge it could not be read in time` | `retry shortly, or ask a human to approve {held}` |
| `model_error` | `the judgment model did not answer` | `retry shortly, or ask a human to approve {held}` |
| `no_model` | `it needs a judgment and no model is configured` | `ask a human to approve {held}` |
| `low_confidence:…` | `the judgment was not confident enough to let it through` | `ask a human to approve {held}` |
| `duplicate:<id>` | `possible duplicate of {table}.{pk}={id}` | `update {table}.{pk}={id} instead of inserting a new row, or ask a human to approve {held}` |
| `conflict` | `it would undo a value another write set recently` | `read the row again and keep the recent value, or ask a human to approve {held}` |
| `policy:<n>` | `it breaks policy rule {n} ("{rule}")`. 여럿이면 `, rule {m} ("{rule}")`를 잇는다 | `change the write to follow the rule, or ask a human to approve {held}` |
| `concurrent_change` | `the row changed while the write was being judged` | `read the row again and retry` |
| `in_transaction` | `it came after another statement in the same transaction and cannot be judged there` | `ROLLBACK, then send this write as its own statement or as the first statement after BEGIN` |
| `no_login_role` | `the session had not finished logging in, so its login role is not known` | `wait until the connection is ready before sending statements` |
| `multi_statement` | `several statements were sent in one query` | `send each statement as its own query` |
| `not_single_row`, `join`, `subquery`, `with`, `multi_row`, `insert_select` | `it may touch more than one row of {table} and its rows could not be counted` | `write one row at a time, selected by primary key` |
| `on_conflict` | `INSERT ... ON CONFLICT cannot be judged` | `check whether the row exists, then send a plain INSERT or UPDATE` |
| `copy` | `COPY into {table} cannot be judged` | `use INSERT statements` |
| `truncate` | `TRUNCATE of {table} cannot be judged` | `delete rows by primary key` |
| `merge` | `MERGE into {table} cannot be judged` | `send separate INSERT and UPDATE statements` |
| `nested_write` | `a write to {table} inside another statement cannot be judged` | `send it as a plain INSERT, UPDATE or DELETE` |
| `indirection`, `multi_assign` | `this form of SET cannot be judged` | `set each column to a whole value, one column per assignment` |
| 그 밖 | `this statement shape cannot be judged ({reason})` | `send a plain single-row INSERT, UPDATE or DELETE` |

닫힌 지점(`Blocked`). `Message`는 표의 문자열 전체다.

| reason | Message | Hint |
|---|---|---|
| `unparsed` | `statement refused by stet: it could not be parsed, so it cannot be checked` | `send standard PostgreSQL syntax, up to version 17` |
| `too_large` | `message refused by stet: it is larger than 64 MiB and cannot be inspected` | `send the data in smaller statements` |
| `function_call` | `function call refused by stet: the FunctionCall message cannot be inspected` | `call the function with SELECT` |
| `do_block` | `DO block refused by stet: what it writes cannot be seen` | `send its statements one by one` |
| `prepared_write` | `statement refused by stet: it runs a prepared write by name, which cannot be judged` | `run the write with Bind and Execute, or send it as SQL` |
| `unsettled_name` | `statement refused by stet: the name it runs was changed by a message the server has not answered yet` | `wait for the answer to Parse or Bind before running the name` |
| `protocol_order` | `connection closed by stet: a message arrived during COPY FROM STDIN that it cannot follow` | `send only copy data until the copy ends` |
| `copy_extended` | `connection closed by stet: COPY FROM STDIN through the extended protocol is not supported here` | `send COPY ... FROM STDIN as a simple query` |
| 그 밖 | `refused by stet ({reason})` | 없음 |

- [ ] **Step 2:** `go test ./internal/decide/` → FAIL.
- [ ] **Step 3: 구현.** `Deterministic`과 `Verdict`는 같은 앞부분을 쓴다(한 함수가 다른 함수를 부른다). `Input.Unsupported`에 `"in_transaction"`을 직접 넣던 기존 호출(lab, 기존 테스트)은 그대로 동작한다. 숫자가 없어 문구를 채울 수 없으면(예: `Impact`가 nil인 `impact:rows`) 숫자 없는 같은 뜻의 문장 대신 "그 밖" 줄을 쓴다.
- [ ] **Step 4:** `go test ./internal/decide/... ./internal/lab/...` → PASS.
- [ ] **Step 5:** 커밋 `Decide: one rule order for both modes, and the table from verdict to action`.

---

### Task 5: 프록시 — 감시할 세션, 깨끗한 트랜잭션, 완료된 쓰기

**Files:** Create `internal/proxy/startup.go`, `internal/proxy/startup_internal_test.go`. Modify `internal/proxy/proxy.go`, `session.go`, `internal/gate/gate.go`(`Inspect` 한 줄), `cmd/stet/main.go`(`proxy.New` 호출). Test `internal/proxy/session_internal_test.go`, `gate_test.go`, `proxy_test.go`, `latency_test.go`, `internal/gate/gate_test.go`·`usage_test.go`·`impact_test.go`·`reads_test.go`(헬퍼와 방법만).

**Interfaces — Consumes:** `sqlparse.Result.Clean` (Task 3). `config.Config.AgentRoles` (Task 1).

**Produces:**

```go
// Policy says which sessions the gate is shown.
type Policy struct {
	// Agents lists the login roles whose sessions are watched, compared byte
	// for byte with the role the server will authenticate. Empty: every session.
	Agents []string
}
func New(upstream string, log *slog.Logger, gate Gate, policy Policy) *Server

// in SessionInfo:
// TxDirty means a statement other than BEGIN, START TRANSACTION, SET or
// SHOW has been sent since the server last reported the session idle,
// whether or not the server has answered it yet (spec 4).
TxDirty bool

// in Statement:
// Clean marks a message whose statements are all BEGIN, START TRANSACTION, SET or SHOW.
Clean bool
```

패키지 안: `startupInfo(pkt []byte) (info SessionInfo, ok bool)`. 테스트 헬퍼: `startProxyWith(t, upstreamAddr, gate, policy)`와 `addedLatencyWith(t, gate, policy, user, label)`(기존 `startProxy`와 `addedLatency`는 빈 `Policy`와 `stet_scram`으로 이것을 부른다), `recordingGate.inspects`(Inspect 호출 수), `rawSession`에 프로토콜 번호 인자, 게이트 테스트의 `(*env).earlyQuery(t, user, sql) string`.

- [ ] **Step 1: 실패하는 테스트.**

  내부 테스트:

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestStartupUserIsReadAsTheServerReadsIt` | 손으로 만든 시작 패킷을 `startupInfo`에 준다 | 프로토콜 3.0·3.1·3.2·3.9 모두 `user`·`database`·`application_name`을 읽는다. `user`가 두 번이면 마지막 값. 70바이트 이름은 앞 63바이트. `database`가 없으면 `user`(자른 값). 끝의 0이 없거나, 값 없는 키로 끝나거나, `user`가 없으면 `ok == false` |
| `TestPolicyWatches` | 읽은 결과와 `Policy`로 감시 여부를 정하는 함수 | `Agents`가 비면 모두 감시. 있으면 목록의 계정만. 읽지 못한 패킷(`ok == false`)은 언제나 감시하고 `User`는 `""` |
| `TestTxDirtyFollowsWhatWasSent` | `fromClient`·`fromServer`를 직접 불러, `Begin`에 온 `TxDirty`를 본다. 테스트용 게이트는 `begin`·`set`·`show`를 `Clean`으로, `insert`를 감시 대상으로 표시한다 | 아래 표 |
| `TestAbandonKnowsACompletedWriteRan` | **기존 `TestAbandonLosesUnansweredWrites`의 뒷부분 단언을 고친다**: CommandComplete(`INSERT 0 1`)는 왔고 ReadyForQuery 전에 끊긴 쓰기 | `Executed == true`, `Lost == true`, `Rows == 1`. 문장이 여럿인 Query(`Several`)는 지금처럼 `Executed == false`. 앞부분(답이 없던 쓰기)은 그대로다 |

  `TestTxDirtyFollowsWhatWasSent`의 줄(`Z(x)`는 서버의 ReadyForQuery, 괄호 없는 것은 클라이언트가 보낸 순서):

| 보낸 것 | 쓰기의 `TxDirty` |
|---|---|
| `insert` | false |
| `begin`, Z(T), `insert`, Z(T), `insert` | false, true |
| `begin`, Z(T), `set`, Z(T), `show`, Z(T), `insert` | false |
| `begin`, Z(T), `select`, Z(T), `insert` | true |
| `begin`, `insert` (답을 기다리지 않음) | false |
| `select`, `insert` (답을 기다리지 않음), Z(I), Z(I), `insert` | true, false |
| `select`, `begin`, Z(I), `insert` | false |
| `select`, `begin`, `select`, Z(I), `insert` | true |
| 한 Sync 아래 Execute(`begin`), Execute(`insert`), Execute(`insert`) | false, true |
| 한 Sync 아래 Execute(`select`), Execute(`insert`) | true |
| 표에 없는 포털의 Execute, 이어서 Execute(`insert`) | true |
| FunctionCall, `insert` (답을 기다리지 않음) | true |
| `begin`, Z(T), 읽지 못한 문장(`Clean`이 아닌 것), Z(T), `insert` | true |
| `commit`, Z(I), `insert` | false |

  통합 테스트(`gate_test.go`, `recordingGate`의 `Inspect`가 정규식으로 `Clean`을 채운다):

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestSessionsOfOtherRolesAreNotSeen` | `Policy{Agents: ["stet_md5"]}`. `stet_scram`과 `stet_md5`가 감시 대상 테이블에 쓴다 | `stet_scram`의 쓰기는 실행되고 `Inspect`·`Begin`이 한 번도 불리지 않는다. `stet_md5`의 쓰기는 `Begin`에 오고 `User == "stet_md5"` |
| `TestProtocol31SessionHasItsLoginRole` | `stet_trust`가 프로토콜 3.1 시작 패킷으로 붙어 쓴다 | `Begin`의 `User == "stet_trust"` |
| `TestGateSeesCleanTransactions` | pgx로: 자동 커밋 쓰기. `begin` 뒤 쓰기 둘. `begin`, `select 1`, 쓰기. 한 Sync 아래 `begin`과 쓰기(pgconn 파이프라인의 `SendQueryParams` 둘 뒤 `SendPipelineSync` 한 번) | `TxDirty`가 차례로 false. false, true. true. false. `TxOpen`은 지금 뜻 그대로다 |
| `TestUnwatchedSessionLatencyBudget` (`latency_test.go`) | `addedLatencyWith`를 `Policy{Agents: ["stet_md5"]}`와 `recordingGate`로, `stet_scram` 연결로 잰다 | p95 추가 지연 2ms 미만이고 `Inspect`가 한 번도 불리지 않는다 |

  게이트 테스트의 방법 변경(단언은 그대로다. Task 10이 단언을 바꾼다): 프로토콜 3.1로 "접속 계정 없는 세션"을 만들던 세 테스트(`TestImpactIsNotMeasuredWithoutALoginRole`, `TestCumulativeIsNotAppliedWithoutALoginRole`, `TestReadsWithoutALoginRoleUseAnEmptyRole`)를 `earlyQuery`로 바꾼다. `earlyQuery`는 `stet_trust`의 시작 패킷과 Query를 한 번의 `Write`로 보내고 두 번째 ReadyForQuery까지 읽어 그 Query의 SQLSTATE를 돌려준다. 프록시는 Query를 서버의 첫 ReadyForQuery보다 먼저 읽으므로(서버는 연결마다 프로세스를 새로 띄운다) 그 문장은 접속 계정 없이 게이트에 가고, 서버는 인증한 뒤 그것을 실행한다. 이 근거를 헬퍼의 주석에 적는다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'Startup|Policy|TxDirty|Abandon|OtherRoles|Protocol31|CleanTransactions|Unwatched'` → FAIL.
- [ ] **Step 3: 구현.**
  - `startupInfo`는 `pgproto3`의 디코더를 쓰지 않고 서버처럼 읽는다: 주 버전이 3이면 부 버전은 가리지 않는다. `키\0값\0` 쌍을 끝의 `\0`까지 읽고, 같은 키는 뒤의 것이 이긴다. `user`와 `database`는 앞 63바이트로 자른다.
  - `handle`: 게이트가 있고 `Policy`가 그 세션을 감시하라고 할 때만 세션 탭을 붙인다. 아니면 지금의 게이트 없는 경로(`relay(client, server, nil, nil)`)다.
  - 세션은 "답을 받은 단위까지의 더러움" 하나와 답을 기다리는 단위마다의 표시를 든다. 문장이 도착할 때의 `TxDirty`는 둘 중 하나라도 참이면 참이다. 감시 대상 쓰기는 자기 `TxDirty`를 읽은 뒤에 자기 단위를 더럽힌다. ReadyForQuery가 유휴면 세션의 것을 지우고, 아니면 그 단위의 표시를 세션으로 옮긴다. 모두 `s.mu` 아래다.
  - 더럽히는 것: `Clean`이 아닌 Query와 Execute, 표에 없는 포털의 Execute, FunctionCall, 본문을 읽지 못한 Query. Parse·Bind·Describe·Close·Flush·Sync는 문장이 아니다. Bind는 `Clean`인 문장의 포털도 적어 둔다.
  - `unanswered`: 그 문장의 완료가 이미 왔으면 `Executed`와 `Rows`를 채운다(`Lost`도 그대로 참이다). Query 단위는 문장이 하나일 때만이다.
  - `gate.Inspect`가 `Clean: r.Clean`을 넘긴다. 분류에 실패한 문장은 `Clean`이 거짓이다. 게이트 테스트의 `setup`과 `restart`, `cmd/stet/main.go`는 `proxy.Policy{Agents: cfg.AgentRoles}`를 넘긴다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → PASS.
- [ ] **Step 5:** 커밋 `Proxy: watch agent roles only, track clean transactions, and know a completed write ran`.

---

### Task 6: 프록시 — 문장·포털 표는 서버가 확인한 것만 믿는다

**Files:** Create `internal/proxy/names.go`, `internal/proxy/names_internal_test.go`. Modify `internal/proxy/session.go`. Test `internal/proxy/gate_test.go`.

**Interfaces — Produces (패키지 안):**

```go
// in unit:
msgs   int // P, B, D, C and E messages so far; a message's pos is its 1-based count
acks   int // how many of them the server completed, known at the unit's ReadyForQuery
errPos int // pos of the message the unit's error answers; 0 when it has none

// names follows the prepared statements, or the portals, of one session as
// the server holds them. Keys are the first 63 bytes of a name.
type names[T any] struct{ /* confirmed entries; changes not answered yet, oldest first */ }

// put notes a Parse or Bind (set) or a Close (!set) of name, sent as message pos of u.
func (n *names[T]) put(u *unit, pos int, name string, set bool, v T)
// get is what name means to a message arriving now in u. A change made
// earlier in u itself counts at once: if it fails, the server skips the rest
// of u. A change from an earlier unit not yet answered may hold or not, so
// the name can mean several things: v is the newest and others the rest.
func (n *names[T]) get(u *unit, name string) (v T, ok bool, others []T)
// answered settles u's changes at its ReadyForQuery: those at pos <= acks hold, the rest are dropped.
func (n *names[T]) answered(u *unit, acks int)
```

서버가 하는 일(PostgreSQL 14.18에서 확인): 있는 이름으로 다시 Parse하면 42P05로 거부하고 옛 문장을 둔다. 있는 포털 이름으로 다시 Bind하면 42P03이다. 앞 메시지가 실패하면 Sync까지 건너뛴다. 응답은 Parse에 `1`, Bind에 `2`, Close에 `3`, Describe에 `T` 또는 `n` 하나(문장을 Describe하면 그 앞에 `t`가 온다), Execute에 `C`·`I`·`s` 가운데 하나다. 이름 없는 문장의 Parse가 실패하면 서버는 이름 없는 문장을 버린다. 이때 프록시는 옛 뜻으로 돌아가는데, 서버에 없는 문장을 한 번 더 판단할 뿐이다.

- [ ] **Step 1: 실패하는 테스트.** 내부 테스트는 `fromClient`와 `serve`로 세션을 직접 몬다.

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestReparseTheServerRefusedKeepsTheStatement` | `s1`을 `insert`로 Parse, 서버 `1` `Z`. `s1`을 `select 1`로 Parse, 서버 에러 `Z`. `s1`을 Bind·Execute | `Begin`이 `insert`의 SQL로 불린다 |
| `TestSkippedParseAndCloseAreUndone` | `""`와 `s1`이 `insert`로 확인된 뒤, 한 단위에 없는 문장의 Bind, `""`를 `select 1`로 Parse, `s1`의 Close, Sync. 서버 에러 `Z` | `""`와 `s1` 모두 Bind·Execute하면 `Begin`이 불린다 |
| `TestParseCountsAtOnceWithinItsSync` | 한 단위에 `""`를 `insert`로 Parse, Bind, Execute(서버의 답 전) | `Begin`이 불린다. 그 Parse가 실패한 것으로 답이 오면 표는 그 앞의 뜻으로 돌아간다 |
| `TestConfirmedCloseAndParseReplace` | `s1`(`insert`)의 Close, 서버 `3` `Z`. `s1`을 `select 1`로 Parse, 서버 `1` `Z`. Bind·Execute | `Begin`이 불리지 않는다 |
| `TestRebindTheServerRefusedKeepsThePortal` | 포털 `p`를 `insert` 문장에 Bind, 서버 `2` `Z`(트랜잭션 안). `p`를 읽는 문장에 Bind, 서버 에러 `Z`. Execute `p` | `Begin`이 불린다 |
| `TestDescribeRepliesAreCounted` | 한 단위에 Parse, Describe(문장), Bind, Describe(포털), Execute. 서버 `1` `t` `T` `2` `T`, 에러, `Z` | `acks == 4`, `errPos == 5`. Parse와 Bind는 표에 남는다 |
| `TestNamesAreComparedByTheirFirst63Bytes` | 63자 + `AAAA` 이름으로 `insert`를 Parse(확인). 63자 + `BBBB`로 Bind·Execute | `Begin`이 불린다. 포털 이름도 같다 |
| `TestUnansweredChangeLeavesSeveralMeanings` | `s1`이 `insert`로 확인된 뒤, 답 없는 단위에서 `s1`을 `select 1`로 Parse하고 Sync. 다음 단위에서 `get` | `v`는 `select 1`, `others`에 `insert`. 같은 단위 안에서 물으면 `others`가 비어 있다 |
| `TestPortalsEndWithTheirTransaction` | 확인된 포털이 있는 채로 ReadyForQuery가 유휴로 온다 | 그 포털은 표에 없다. 아직 답 없는 Bind는 남는다 |

  통합 테스트 `TestGateJudgesTheStatementTheServerHolds`(`rawSession`의 `stet_trust`, 감시 대상 테이블 권한을 준다): ① `s1`을 감시 대상 `insert`로 Parse·Sync. `s1`을 `select 1`로 Parse·Sync(42P05). `s1`을 Bind·Execute·Sync → `Begin`이 `insert`로 한 번 불리고 행이 들어간다. ② 없는 문장의 Bind, `s1`의 Close, Sync(26000) 뒤 `s1`을 Bind·Execute·Sync → `Begin`이 또 불린다. 응답을 읽는 헬퍼 `replies(t, fe) string`을 더한다: ReadyForQuery까지 받은 메시지의 종류를 순서대로, 에러는 `E:<SQLSTATE>`로 돌려준다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'Reparse|Skipped|ParseCounts|Confirmed|Rebind|Describe|First63|Unanswered|PortalsEnd|ServerHolds'` → FAIL.
- [ ] **Step 3: 구현.**
  - `session.stmts`와 `session.portals`를 `names`로 바꾼다. 두 고루틴이 만지므로 `s.mu` 아래다(메시지마다 한 번이고, 행마다가 아니다).
  - Bind는 모든 포털을 적는다(관심 없는 문장이면 빈 뜻으로). 그래야 확인된 Bind가 옛 뜻을 덮는다. 바인딩 값은 지금처럼 감시 대상 문장일 때만 복사한다.
  - 서버 쪽은 `1`·`2`·`3`·`T`·`n`을 본문 없이 종류만 보는 훅(`tap.each`)에서 센다. 결과 행(`D`, `d`)의 갈래는 지금처럼 정수 하나를 올리는 것뿐이다. `C`·`I`·`s`는 지금 보는 자리에서 함께 센다. 확장 단위의 첫 에러에서 `errPos`를 정하고, ReadyForQuery에서 `acks`를 단위에 옮긴 뒤 `answered`를 부른다. 기존 `executes`·`completions`·`errAt`와 그것으로 정하던 `Outcome`은 건드리지 않는다.
  - ReadyForQuery가 유휴면 확인된 포털을 비운다. 서버는 트랜잭션이 끝날 때 포털을 모두 없앤다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/ ./internal/gate/` → PASS. 기존 테스트의 단언은 고치지 않는다.
- [ ] **Step 5:** 커밋 `Proxy: statement and portal tables follow what the server confirmed`.

---

### Task 7: 프록시 — 막기

**Files:** Create `internal/proxy/refuse.go`, `internal/proxy/refuse_internal_test.go`. Modify `internal/proxy/proxy.go`(`tap`, `pump`), `session.go`, `internal/gate/gate.go`(`Begin`의 반환값만). Test `internal/proxy/gate_test.go`.

**Interfaces — Consumes:** `unit.msgs`·`acks`·`errPos` (Task 6).

**Produces:**

```go
// Refusal is the error a client gets in place of a message the proxy did not forward.
type Refusal struct {
	Code    string // SQLSTATE
	Message string
	Detail  string
	Hint    string
}

// in Gate:
// Begin runs just before a watched statement goes upstream, and the client
// waits for it. When refuse is not nil the statement is not sent: the proxy
// sends a message of the same kind that the server rejects, and gives the
// client refuse in place of the server's error. End is still called.
Begin(s SessionInfo, st Statement, params []Param) (handle any, refuse *Refusal)

// in Outcome:
// Refused means the statement was not sent: the gate's Begin refused it.
Refused bool
// Undelivered, set only with Refused, means the client was not given the
// refusal: an earlier message under the same Sync failed first, or the
// connection ended before the unit's ReadyForQuery.
Undelivered bool
```

패키지 안:

```go
// in tap: see may return one whole message (type, length, body) to send in
// place of the one it was shown; nil forwards it unchanged. For a message
// past maxInspect a replacement makes pump read and discard the body. A
// non-nil err ends the pump.
see func(t byte, body []byte, flush func() error) (replace []byte, err error)

// refuse records that the message at pos of u (0 for a Query or FunctionCall
// unit) is answered with r, and returns the message to send in its place:
// one of kind t that fails on the server. name is the Parse's statement name.
func (s *session) refuse(u *unit, pos int, t byte, name string, r Refusal) []byte
```

바꿔 보내는 메시지(PostgreSQL 14.18에서 모두 그 자리에서 실패하고, 같은 Sync 아래의 앞 `INSERT`가 롤백되며, 트랜잭션 안에서는 상태가 실패로 바뀌는 것을 확인했다):

| 원래 | 바꿔 보내는 것 | 서버의 에러 |
|---|---|---|
| Query | `Query{String: ") stet: statement refused"}` | 42601 |
| Parse | 같은 이름, 본문 `) stet: statement refused`. 본문을 읽지 못했으면 이름은 새 무작위 이름 | 42601 |
| Execute | 없는 포털 이름 | 34000 |
| Bind | 없는 문장 이름, 포털은 `""` | 26000 |
| FunctionCall | 함수 OID 0 | 42883 |

"없는 이름"은 `stet_` 뒤에 `crypto/rand`의 16바이트를 16진수로 붙인 37바이트이고 매번 새로 만든다.

- [ ] **Step 1: 실패하는 테스트.** `recordingGate`에 `refuse func(st proxy.Statement) *proxy.Refusal`을 더한다(nil이면 지금처럼 보낸다). 쓰는 `Refusal`은 `{Code: "ST001", Message: "held", Detail: "d", Hint: "h"}`다.

| 테스트 | 보내는 것 | 단언 |
|---|---|---|
| `TestRefusedQueryGetsTheGatesError` | 단순 프로토콜의 감시 대상 쓰기 | 클라이언트의 에러가 `ST001`, `held`, `d`, `h`. 테이블은 그대로. 다음 문장이 된다. `End`의 결과는 `Refused`, `Undelivered` 아님, `Executed` 아님 |
| `TestRefusedExecuteGetsTheGatesError` | 확장 프로토콜(`conn.Exec`와 인자) | 같다. `Begin`에는 원래 문장과 바인딩 값이 온다 |
| `TestRefusalFailsTheOpenTransaction` | `begin`, 막히는 쓰기, `select 1`, `rollback`, `select 1` | `ST001`, 25P02, 성공, 성공 |
| `TestRefusalRollsBackTheRestOfItsSync` | 한 Sync 아래에 감시하지 않는 테이블의 insert와 막히는 쓰기 | 에러는 `ST001` 하나. 앞의 insert도 남지 않는다 |
| `TestRefusedPipelineAnswersLikeAServerError` | 원시 연결로 P·B·E(통과하는 insert), P·B·E(막히는 쓰기), P·B·E(`select 1`), Sync | `replies`가 `1 2 C 1 2 E:ST001 Z`. 같은 자리의 문장이 서버에서 실패하게(PK 중복) 직접 연결로 보낸 것과 메시지의 종류와 수가 같다 |
| `TestEarlierFailureHidesTheRefusal` | 한 Sync 아래에 `select 1/0`과 막히는 쓰기 | 클라이언트는 22012를 그대로 받는다. `End`는 `Refused`이고 `Undelivered` |
| `TestBindFailureBeforeARefusedExecuteIsLeftAlone` | 원시 연결로 `… where id = $1`의 Parse, 값 `abc`의 Bind, Execute(막힘), Sync | 클라이언트는 22P02를 받는다. `ST001`이 아니다. `Undelivered` |
| `TestSecondRefusalUnderOneSyncIsUndelivered` | 한 Sync 아래에 막히는 쓰기 둘 | 에러는 하나. 첫째는 전달됨, 둘째는 `Undelivered`. `End`는 둘 다 한 번씩 |
| `TestRefusedExecuteLeavesThePreparedStatementUsable` | `conn.Prepare`로 준비한 쓰기를 두 번 막고, 세 번째는 통과시킨다 | 에러 둘 뒤 세 번째가 실행된다 |
| `TestRefusedCopyFromStdinDoesNotHang` | `conn.CopyFrom`으로 감시 대상 테이블에 넣는다(게이트가 `copy` 문장을 막는다) | 5초 안에 `ST001`로 끝난다. 다음 문장이 된다 |
| `TestRefusalOfALostSessionIsUndelivered` (내부) | 막힌 Query 뒤에 서버의 답 없이 `abandon` | `Refused`, `Undelivered`. 에러를 바꿔 쓴 뒤 ReadyForQuery 전에 `abandon`해도 `Undelivered` |
| `TestRefusedMessagesAreReplacedInKind` (내부) | `refuse`가 돌려준 바이트를 디코드한다 | 위 표의 다섯 줄. 없는 이름은 두 번 부르면 다르고 63바이트 이하다 |

  기존 `TestWatchedPassthrough`, `TestObservedPassthrough`, 지연 테스트가 고치지 않고 통과해야 한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'Refus|EarlierFailure'` → FAIL.
- [ ] **Step 3: 구현.**
  - `pump`: `see`가 대신 보낼 메시지를 돌려주면 원래 것 대신 그것을 쓴다.
  - 클라이언트 쪽: `Begin`이 `Refusal`을 돌려주면 `refuse`로 기록하고 바꿔 보낸다. 게이트는 지금처럼 잠금 없이 부르고, 앞에 쌓인 메시지를 먼저 `flush`한다. 막은 메시지는 커서 표와 `on_behalf_of`를 바꾸지 않는다. 막은 뒤에도 그 단위의 뒤 메시지는 그대로 보낸다(서버가 건너뛴다).
  - 서버 쪽: 단위의 에러가 막은 메시지의 것이면(Query·FunctionCall 단위면 그 단위의 에러, 확장 단위면 `errPos`가 막은 메시지의 순번) 그 ErrorResponse를 `Refusal`로 바꿔 쓴다: `Severity`와 `SeverityUnlocalized`는 `ERROR`, 나머지는 `Code`·`Message`·`Detail`·`Hint`뿐이다. 앞 순번의 에러면 손대지 않는다. 한 단위에서 바꿔 쓰는 에러는 하나다.
  - `finish`는 막은 쓰기를 `Refused`로 알리고, 에러가 가려졌으면 `Undelivered`를 더한다. `abandon`은 막은 쓰기를 언제나 `Undelivered`로 알린다.
  - `gate.Gate.Begin`은 `(p, nil)`을 돌려준다. Task 11이 채운다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/ ./internal/gate/` → PASS.
- [ ] **Step 5:** 커밋 `Proxy: refuse a write by sending a failing message and rewriting its one error`.

---

### Task 8: 프록시 — 닫힌 지점

**Files:** Create `internal/proxy/export_test.go`. Modify `internal/proxy/proxy.go`, `session.go`, `internal/gate/gate.go`(`Inspect`, `Blocked`), `cmd/stet/main.go`(`Policy`). Test `internal/proxy/gate_test.go`, `latency_test.go`, `internal/gate/gate_test.go`.

**Interfaces — Consumes:** `refuse`, `tap.see`의 반환값 (Task 7). `decide.Blocked` (Task 4). `sqlparse.Result.Do` (Task 3).

**Produces:**

```go
// in Policy:
// Enforce makes watched sessions fail closed: a message that cannot be
// inspected is refused instead of forwarded.
Enforce bool

// Reasons a message is refused without a decision.
const (
	BlockUnparsed     = "unparsed"
	BlockTooLarge     = "too_large"
	BlockFunctionCall = "function_call"
	BlockDo           = "do_block"
)

// in Statement:
// Block says why the message cannot be inspected (BlockUnparsed, BlockDo); "" when it can.
Block string

// in Gate:
// Blocked returns the error for a message refused for reason on a watched
// session, and logs it: no decision records such a message. It must not block.
Blocked(s SessionInfo, reason string) Refusal
```

테스트 전용(`export_test.go`): `SetMaxInspect(n int64) (restore func())`. `maxInspect`를 `var`로 바꾼다.

- [ ] **Step 1: 실패하는 테스트.** `recordingGate`에 `block func(sql string) string`과 `Blocked`(호출을 기록하고 `Refusal{Code: "ST002", Message: "blocked " + reason}`을 돌려준다)를 더한다. 표의 테스트는 `Policy{Enforce: true}`와 빈 `Policy` 두 가지로 돈다.

| 테스트 | 보내는 것 | enforce | 섀도 |
|---|---|---|---|
| `TestUnreadableStatementsAreRefused` | `block`이 `unparsed`로 표시하는 문장을 Query로, 그리고 Parse로 | `ST002` `blocked unparsed`. 세션을 계속 쓸 수 있다 | 서버의 답 그대로 |
| `TestDoBlocksAreRefused` | 임시 테이블에 쓰는 `do $$ … $$` | `ST002`, 테이블은 그대로 | 실행된다 |
| `TestFunctionCallsAreRefused` | 원시 연결의 FunctionCall(`pg_backend_pid`의 OID) | `E:ST002 Z` | FunctionCallResponse |
| `TestOversizedMessagesAreRefused` | `SetMaxInspect(1 << 20)` 뒤 2MiB짜리 Query, 그리고 2MiB 값의 Bind | `ST002` `blocked too_large`. 뒤의 문장이 된다 | 실행된다 |
| `TestClosedPointsFailTheOpenTransaction` | `begin`, DO 블록, `select 1`, `rollback` | `ST002`, 25P02, 성공 | 해당 없음 |
| `TestUnwatchedSessionsSkipTheClosedPoints` | `Policy{Agents: ["stet_md5"], Enforce: true}`에서 `stet_scram`이 DO 블록과 FunctionCall을 보낸다 | 둘 다 실행되고 `Blocked`가 불리지 않는다 | — |
| `TestEnforcedSessionLatencyBudget` (`latency_test.go`) | `addedLatencyWith`를 `Policy{Enforce: true}`와 `recordingGate`로 | p95 추가 지연 2ms 미만 | — |

  게이트 테스트: `TestInspectMarksWhatCannotBeInspected`: `Inspect("selec 1").Block == "unparsed"`, `Inspect("do $$ begin end $$").Block == "do_block"`, `Inspect("select 1").Block == ""`. `TestBlockedAnswersAndLogs`: `Blocked(info, "do_block")`의 `Code`·`Message`·`Hint`가 `decide.Blocked`의 것이고 `Detail`은 비어 있으며, 경고 로그에 세션 id, 접속 계정, 이유가 있다(`gate.SetLogger`와 `lockedBuffer`).
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ ./internal/gate/ -run 'Unreadable|DoBlocks|FunctionCalls|Oversized|ClosedPoints|SkipTheClosed|EnforcedSession|Inspect|Blocked'` → FAIL.
- [ ] **Step 3: 구현.** 세션은 `Policy.Enforce`를 들고, 켜져 있을 때만 다음을 한다. Query·Parse의 `Block`이 비어 있지 않으면 그 이유로, 본문을 읽지 못한 Query·Parse·Bind는 `too_large`로, FunctionCall은 `function_call`로 `gate.Blocked`를 부르고 `refuse`로 바꿔 보낸다. 큰 메시지의 본문은 `pump`가 읽어서 버린다. 막은 Parse는 표에 적지 않는다. `gate.Inspect`는 분류에 실패하면 `Block: "unparsed"`, `Result.Do`면 `Block: "do_block"`을 모드와 상관없이 채운다. `gate.Blocked`는 `decide.Blocked`를 `proxy.Refusal`로 옮기고 경고 로그를 남긴다. 게이트 테스트의 `setup`·`restart`와 `cmd/stet/main.go`는 `Enforce: cfg.Mode == config.ModeEnforce`를 넘긴다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → PASS.
- [ ] **Step 5:** 커밋 `Proxy: refuse what cannot be inspected on enforced sessions`.

---

### Task 9: 프록시 — 표를 비켜 가는 길을 닫는다

스펙 보정 B4·B5·B6이다. Step 3의 첫 항목(Sync 없는 단위 따라가기)만 두 모드 모두에 적용하고, 나머지는 `Policy.Enforce`인 감시 세션에서만 한다.

**Files:** Modify `internal/proxy/proxy.go`(`relay`), `session.go`, `internal/gate/gate.go`(`Inspect`). Test `internal/proxy/gate_test.go`, `session_internal_test.go`.

**Interfaces — Consumes:** `unit.msgs`, `names.get`의 `others`, `names.answered` (Task 6). `refuse`, `tap.see`의 `err` (Task 7). `Gate.Blocked` (Task 8). `sqlparse.Result.Executes`·`Portals` (Task 3).

**Produces:**

```go
const (
	BlockPreparedWrite = "prepared_write" // SQL EXECUTE, FETCH or MOVE of a name the session prepared or bound as a watched statement
	BlockUnsettledName = "unsettled_name" // the name may mean a watched statement, and the server has not said which meaning holds
	BlockProtocolOrder = "protocol_order" // the session is ended: answers can no longer be matched to messages
	BlockCopyExtended  = "copy_extended"  // the session is ended: COPY FROM STDIN through the extended protocol
)

// in Statement:
Executes []string // prepared statements the SQL runs with EXECUTE
Portals  []string // portals the SQL reads with FETCH or MOVE
```

서버가 하는 일(PostgreSQL 14.18에서 확인):

- 프로토콜 Parse로 준비한 `s1`을 SQL `EXECUTE s1`이 실행한다. 트랜잭션 안에서 프로토콜 Bind로 만든 `UPDATE … RETURNING` 포털을 SQL `FETCH`와 `MOVE`가 실행한다.
- 확장 메시지 뒤에 Sync 없이 Query가 오면: 앞의 확장 메시지가 모두 성공했을 때는 그 Query를 실행하고 ReadyForQuery를 하나 보낸다(확장 메시지 몫의 ReadyForQuery는 없다). 앞에서 에러가 났을 때는 다음 Sync까지 Query도 답 없이 건너뛰고, 그 Sync에 ReadyForQuery를 하나 보낸다. FunctionCall도 같다. Flush만 보낸 뒤의 Query는 첫 경우다.
- 확장 프로토콜의 `COPY FROM STDIN`은 Execute 뒤의 Sync를 무시하고 CopyDone 뒤의 Sync에만 답한다. 단순 프로토콜의 복사 중에 온 Sync도 무시한다.

- [ ] **Step 1: 실패하는 테스트.** 통합 테스트는 원시 연결(`rawSession`, `stet_trust`)이고, 따로 적지 않으면 `Policy{Enforce: true}`다.

| 테스트 | 보내는 것 | 단언 |
|---|---|---|
| `TestQueryBeforeSyncIsFollowed` (빈 `Policy`와 enforce 둘 다) | ① `select 1`의 Parse·Describe·Flush를 보내고 답(`1 t T`)을 읽은 뒤, 감시 대상 insert를 Query로, 이어서 또 하나를 Query로. ② 문법이 틀린 Parse와 Flush, 에러를 읽은 뒤 감시 대상 insert를 Query로, 이어서 Sync. 그 뒤 insert 하나 더 | ① 두 쓰기의 결과가 각각 `Executed`, `Rows 1`이고 세션이 끊기지 않는다. ② Query의 답은 없고 Sync에 `Z` 하나. 그 insert의 결과는 `Executed`가 아니고 `Error`에 `not executed`. 뒤의 insert는 `Executed`다(응답이 한 칸씩 밀리지 않는다) |
| `TestUnsyncedUnitIsAnsweredWithoutReadyForQuery` (내부) | `s1`을 insert로 Parse, Describe, Flush, 이어서 Query `begin`. 서버 `1` `t` `n` `C` `Z(T)`. 그 뒤 `s1`을 Bind·Execute·Sync | Parse가 표에 확인돼 있고, 남은 단위가 없고, `Begin`이 `TxDirty == false`로 불린다 |
| `TestSQLExecuteOfAPreparedWriteIsRefused` | `s1`을 감시 대상 insert로 Parse·Sync. Query `execute s1`, 이어서 `explain analyze execute s1` | 둘 다 `E:ST002`(`blocked prepared_write`)이고 행이 들어가지 않는다. 빈 `Policy`에서는 실행된다. 표에 없는 이름의 `execute`는 서버의 답 그대로다 |
| `TestFetchFromAWritePortalIsRefused` | `begin`. 감시 대상 `update … returning id`를 Parse하고 포털 `p1`에 Bind·Sync. Query `fetch all from p1`. 새 트랜잭션에서 같은 준비 뒤 `move all in p1` | 둘 다 `ST002`이고 테이블은 그대로다 |
| `TestUnsureNameIsRefused` | `s1`이 감시 대상 insert로 확인된 뒤, 한 번의 Flush로 `s1`을 `select 1`로 Parse, Sync, `s1`을 Bind, Execute, Sync | 둘째 단위가 `E:ST002`(`blocked unsettled_name`). `Begin`이 불리지 않고 행이 들어가지 않는다. 어느 뜻도 감시 대상이 아닌 이름으로 같은 순서를 보내면 실행된다 |
| `TestCopyFromStdinThroughTheExtendedProtocolEndsTheSession` | 임시 테이블로의 `copy … from stdin`을 Parse·Bind·Execute·Sync | 클라이언트는 `FATAL` `ST002`(`blocked copy_extended`)를 받고 연결이 끝난다. 서버의 세션도 끝난다(`pg_stat_activity`). 같은 `Policy`에서 `conn.CopyFrom`(단순 프로토콜)은 된다. 빈 `Policy`에서는 끊기지 않는다 |
| `TestSyncDuringCopyEndsTheSession` | Query `copy … from stdin`, CopyInResponse를 받은 뒤 Sync. 그리고 그 Query와 Sync를 한 번에 보낸 경우 | 둘 다 `FATAL` `ST002`(`blocked protocol_order`) |

- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'BeforeSync|Unsynced|SQLExecute|FetchFromAWrite|UnsureName|ExtendedProtocolEnds|DuringCopy'` → FAIL.
- [ ] **Step 3: 구현.**
  - Sync 없는 단위(두 모드 모두): Query나 FunctionCall이 올 때 마지막 단위가 아직 열려 있으면 그 단위를 "Sync 없이 닫힘"으로 표시하고, Query는 지금처럼 자기 단위를 만든다. 서버 쪽은 응답을 처리하기 전에 본다: 맨 앞 단위가 Sync 없이 닫혔고 에러가 없고 그 메시지가 모두 완료됐으면(`msgs`만큼) ReadyForQuery 없이 그 단위를 끝낸다(표에는 전부 확인된 것으로, 결과는 트랜잭션 안에서 실행된 것으로). Sync 없이 닫힌 단위에 에러가 났으면, 다음 ReadyForQuery에서 그 단위와 함께 뒤의 단위를 Sync로 닫힌 첫 단위까지 끝낸다. 건너뛰어진 단위의 문장은 지금의 "앞 문장이 실패해 실행되지 않음"으로 알린다. `End`와 `Observe`는 여전히 잠금을 푼 뒤에 부른다.
  - `gate.Inspect`가 `Executes`와 `Portals`를 `Result`에서 옮긴다.
  - Query와 Execute에서: 문장의 `Executes`에 있는 이름을 문장 표에서, `Portals`에 있는 이름을 포털 표에서 찾는다. 가능한 뜻 가운데 감시 대상이 하나라도 있으면 `prepared_write`로 막는다.
  - Bind와 Execute에서: 이름의 뜻이 둘 이상이고(`others`) 그 가운데 감시 대상이 있으면 그 포털의 Execute를 `unsettled_name`으로 막는다. 게이트는 부르지 않는다. 섀도에서는 가장 새 뜻으로 본다(지금과 같다).
  - 세션을 끊는 경우: ① 서버가 확장 단위에 CopyInResponse(`G`)나 CopyBothResponse(`W`)로 답한다(`copy_extended`). ② 서버가 Query 단위에 `G`로 답했는데 그 단위가 이미 마지막 단위가 아니거나, 그 단위의 ReadyForQuery 전에 탭이 보는 클라이언트 메시지가 온다(`protocol_order`. CopyData·CopyDone·CopyFail은 탭이 보지 않는다).
  - 끊는 방법: 세션에 이유를 적고 `see`가 에러를 돌려줘 그 `pump`를 끝낸다(그 메시지는 전달하지 않는다). `relay`는 끝낼 이유가 적혀 있으면 서버 연결을 닫고 클라이언트의 읽기 기한을 지금으로 당겨 다른 `pump`를 끝낸 뒤, `gate.Blocked`의 `Refusal`을 `Severity` `FATAL`인 ErrorResponse로 클라이언트에 쓰고 닫는다. 이유가 없으면 `relay`는 지금 그대로다. 서버가 보내던 메시지가 중간에 끊겼다면 클라이언트는 깨진 스트림을 볼 수 있고, 그것은 받아들인다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → PASS. 기존 테스트의 단언은 고치지 않는다.
- [ ] **Step 5:** 커밋 `Proxy: follow a Query sent before Sync, and close the ways around the statement tables`.

---

### Task 10: 게이트 — 섀도와 enforce가 함께 쓰는 판정 규칙

**Files:** Rewrite `internal/gate/usage.go`. Create `internal/gate/usage_internal_test.go`. Modify `internal/gate/gate.go`, `impact.go`, `export_test.go`, `internal/store/store.go`(`Usage` 삭제). Test `internal/gate/gate_test.go`, `impact_test.go`, `usage_test.go`, `internal/store/store_test.go`(`TestUsage` 삭제).

**Interfaces — Consumes:** `Config.MaxInflight` (Task 1). `Store.UsageBySecond` (Task 2). `Measure.Qualified` (Task 3). `Input.InTransaction`·`NoLoginRole`, `Cumulative.Unknown` (Task 4). `SessionInfo.TxDirty` (Task 5).

**Produces (패키지 안):** `usage.reserve`와 `usage.settle`의 시그니처는 지금 그대로다. 테스트 전용(`export_test.go`):

```go
// SetUsageRetry changes how long a pair waits after a failed load before
// loading again, until restore is called. Not for parallel tests.
func SetUsageRetry(d time.Duration) (restore func())
// HoldInflight takes every slot of g's inflight limit until release is called.
func HoldInflight(g *Gate) (release func())
```

`FailUsageLoads`는 새 적재 함수에 맞춘다.

- [ ] **Step 1: 실패하는 테스트.**

  새 테스트:

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestShadowJudgesTheFirstWriteOfATransaction` | `conn.Begin`, 단일 행 update, 커밋. 그리고 `impactYAML`에서 `conn.Begin`, 2행 update, 커밋 | 첫째는 자동 커밋과 같은 판정(`commit`)이고 질문과 답이 기록된다. 둘째는 측정된다(`method count`, `rows 2`). 둘 다 실행된다 |
| `TestWritesWithoutALoginRoleAreUnsupported` | `earlyQuery`(`stet_trust`)로 단일 행 update | `unsupported [no_login_role]`, 실행됨(섀도), `login_role` NULL, 모델 호출 없음 |
| `TestCumulativeWaitsBeforeLoadingAgain` | `cumYAML`, `SetUsageRetry(time.Hour)`, `FailUsageLoads(g, 1)`. 쓰기 셋 | 셋 다 `hold [impact:cumulative_unknown]`이고 적재는 한 번만 시도된다. 셋 다 실행된다(섀도) |
| `TestInflightLimitTurnsIntoAStateError` | `limits: {max_inflight: 1}`을 더한 설정, `HoldInflight`. 단일 행 update | `commit [state_error]`(섀도의 `on_failure: pass`), 실행됨, `latency_ms`가 450 이상. 풀어 준 뒤의 쓰기는 `commit`이고 이유가 없다 |
| `TestImpactDoesNotCountThroughATableQualifiedFunction` | `looseYAML`. 그 테이블의 행을 받아 `pg_try_advisory_lock(키)`를 부르는 SQL 함수 `f`를 만들고, `update T set status = 'x' where T.f and company_id = 9` | `escalate [impact:uncertain]`, `method estimate`, `cap` 없음. 그 잠금을 쥔 백엔드는 클라이언트의 것뿐이다(`advisoryHolders`). `wantCleanPool` |
| `TestImpactStillCountsQualifiedColumns` | `update T set status = 'x' where T.company_id = 9` | `method count`, `rows 2` |
| `TestProtocol31SessionsRecordTheirLoginRole` | `rawSimpleQuery`에 프로토콜 3.1(`3<<16 + 1`)과 `stet_trust`를 주어 단일 행 update | `login_role = stet_trust`, `commit` |

  내부 테스트(`usage_internal_test.go`, DB 없음. 적재 함수와 시각을 테스트가 준다):

| 테스트 | 단언 |
|---|---|
| `TestUsageCountsPerSecond` | 예약은 `used`에 들지 않고 다음 쓰기부터 든다. 확정은 그 초의 수를 고친다. 서버가 거부한 쓰기는 빠지고 끊긴 쓰기는 남는다(지금의 `settle` 규칙) |
| `TestUsageKeepsASecondUntilItLeftTheWindow` | 창 2초. 0.9초 시각의 쓰기는 2.5초 시각에 아직 세고(그 초가 창에 걸친다), 3.0초부터 세지 않는다 |
| `TestUsageLoadsOnlyWhatCameBeforeItStarted` | 첫 `reserve`가 `[창의 시작, 게이트 시작 시각)`으로 적재 함수를 한 번 부르고, 받은 초 칸을 더한다 |
| `TestUsageRetriesAfterAnInterval` | 실패한 적재 뒤 간격 안의 `reserve`는 적재 함수를 부르지 않고 에러를 돌려준다. 간격 뒤에는 다시 부른다. 그사이의 예약은 남아 있다 |
| `TestUsageDropsIdlePairs` | 쌍의 마지막 쓰기가 창 밖으로 나간 뒤 다른 쌍의 `reserve`가 정리를 돌리면 그 쌍이 지워진다 |

  **고치는 기존 테스트**(스펙 16장이 허락한 판정 변경: `in_transaction`, 계정 없는 세션, 누적 값 모름):

| 테스트 | 고치는 것 |
|---|---|
| `TestShadowWriteInTransactionIsNotJudged` | 트랜잭션 안의 update 앞에 `select 1`을 둔다. 단언은 그대로다 |
| `TestImpactIsNotMeasuredInsideATransaction` | 위와 같다 |
| `TestCumulativeCountsWritesItDoesNotJudge` | 위와 같다 |
| `TestImpactIsRecordedInAnImplicitTransaction` | 쓰기가 그 Sync의 첫 Execute이므로 `commit`이고 측정값은 그대로다. `select 1`을 쓰기 앞에 둔 경우를 더한다: `unsupported [in_transaction]`, `impact` NULL |
| `TestImpactRecordsAWriteSkippedOnAStaleTransactionStatus` | `unsupported [in_transaction]`, `impact` NULL. 이름을 `TestWriteSentBehindUnansweredStatementsIsNotJudged`로 바꾼다 |
| `TestImpactIsNotMeasuredBeforeAuthentication`, `TestImpactIsNotMeasuredWithoutALoginRole` | 판정만 `unsupported [no_login_role]`. `impact.error`의 단언은 그대로다 |
| `TestCumulativeIsNotAppliedWithoutALoginRole` | 여섯 쓰기의 판정이 `unsupported [no_login_role]`. `cumulative`가 `{"error": "no login role"}`인 것과 뒤 쓰기의 `used 0`은 그대로다 |
| `TestCumulativeLoadsAgainAfterAFailedLoad` | `SetUsageRetry(0)`. 첫 쓰기의 판정이 `hold [impact:cumulative_unknown]`. 나머지 단언은 그대로다 |
| `TestCumulativeWindowSlides` | 기다리는 시간을 2200ms에서 3200ms로(초 칸은 최대 1초 더 센다). 단언은 그대로다 |

- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/` → FAIL.
- [ ] **Step 3: 구현.**
  - `judge`: `in.InTransaction = p.session.TxDirty`, `in.NoLoginRole = p.session.User == ""`. `Outcome.InTransaction`으로 판정하던 갈래를 없앤다. 형태가 먼저인 순서는 `decide.Verdict`가 정한다.
  - `Begin`: 측정은 `TxOpen`이 아니라 `TxDirty`가 아닐 때 한다. 더러운 트랜잭션의 측정 대상 쓰기는 지금처럼 `impact`가 NULL이다. 계정 없는 쓰기는 지금처럼 측정 실패(`impact.error`)와 `cumulative.error`를 남긴다.
  - 동시 상한: `Begin`의 첫 일로 `MaxInflight` 크기의 자리를 `stateTimeout` 컨텍스트 아래에서 잡는다. 못 잡으면 `stateErr`(문구 `too many writes are being judged`)이고 상태 읽기와 측정을 하지 않는다. 섀도에서는 `Begin`이 끝날 때 놓는다. 섀도의 모델 호출은 지금의 `sem`(8)을 그대로 쓴다.
  - 사용량: 쌍마다 초 → 행 수의 칸과 합계를 둔다. 아직 확정되지 않은 예약만 id로 따로 둔다. 초 칸은 그 초 전체가 창 밖으로 나갈 때 뺀다. 첫 적재는 `UsageBySecond(창의 시작, 게이트 시작 시각)` 한 번이다. 실패하면 `usageRetry`(기본 5초, `var`) 동안 다시 읽지 않고 에러를 돌려준다. 1분에 한 번을 넘지 않게, `reserve`가 창이 지난 빈 쌍을 지운다. `store.Usage`와 `TestUsage`를 지운다.
  - 열 이름 확인: `queryImpact`가 세기 전에, 같은 읽기 전용 트랜잭션에서 `Measure.Relation`의 열 이름(`pg_attribute`, 지워지지 않은 것, 시스템 열 포함)을 읽어 `Measure.Qualified`가 모두 그 안에 있는지 본다. 아니면 세지 않고 추정값을 쓴다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/ ./internal/store/` → PASS. 위 표에 없는 기존 테스트의 단언은 고치지 않는다.
- [ ] **Step 5:** 커밋 `Gate: one set of verdict rules, usage per second, an inflight limit, and a column check before counting`.

---

### Task 11: 게이트 — enforce

**Files:** Create `internal/gate/enforce.go`, `internal/gate/enforce_test.go`. Modify `internal/gate/gate.go`(`Begin`, `End`, `judge`, 패키지 주석), `export_test.go`. Test `internal/gate/gate_test.go`(`fakeDecider`에 훅).

**Interfaces — Consumes:** `Decision.Refused`, `HeldWrite`, `InsertHeldWrite`, `MarkUndelivered` (Task 2). `Deterministic`, `Act`, `Action.Hint`, `Detail`, `Facts` (Task 4). `proxy.Refusal`, `Outcome.Refused`·`Undelivered` (Task 7). Task 10의 `Begin`과 훅.

**Produces (테스트 전용):**

```go
// FailHeldWrites makes g's next n inserts into stet.held_writes fail.
func FailHeldWrites(g *Gate, n int)
```

테스트 헬퍼(`enforce_test.go`): `enforceYAML`(`"mode: enforce\nagents: {roles: [stet_md5]}\n" + tableYAML`), `setupEnforce(t, d, yamlFormat ...string) *env`(`setup` 뒤에 superuser로 `grant stet_md5 to stet_scram`, 테이블 권한을 `stet_md5`에게 주고, 끝날 때 그 테이블의 `held_writes` 줄을 지운다), `(*env).agent(t, name)`(`stet_md5` 연결), `(*env).held(t, n)`, `(*env).enforced(t, n)`(판단 기록의 `verdict`, `reasons`, `executed`, `exec_error`, `refused`, `rows_affected`, `mode`), `wantRefused(t, err, code, messagePart)`. `fakeDecider`에 `onDecide func(call int)`(Decide 안에서 불린다).

enforce에서 `Begin`이 하는 일(스펙 3장의 순서):

1. 형태, 트랜잭션, 접속 계정만 채운 `Input`으로 `decide.Verdict`가 `unsupported`를 내면, 자리도 잡지 않고 상태도 읽지 않고 예약도 하지 않고 6번으로 간다.
2. 자리를 잡고, 바인딩 값을 풀고, 상태를 읽거나 영향을 재고, 누적 한도에 예약한다(Task 10의 코드).
3. 2번의 결과를 채운 `Input`으로 `decide.Deterministic`이 판정을 내면 모델을 부르지 않는다.
4. 남으면 모델을 부른다(`model.timeout_ms`). 이때 Stet은 자기 트랜잭션을 열어 두지 않는다. enforce의 모델 호출은 2번의 자리를 쥔 채로 하고 `sem`은 쓰지 않는다.
5. 판정이 `commit`이고 단일 행 `UPDATE`·`DELETE`면 대상 행의 `xmin`을 다시 읽는다(새 `stateTimeout`). 같거나 행이 사라졌으면 보낸다. 다르면 상태를 다시 읽고 한 번 다시 판단하고 이유 끝에 `rejudged`를 더한다. 다시 판단한 것이 `commit`이면 한 번 더 읽어, 또 달라졌으면 `hold [concurrent_change rejudged]`다. 다시 읽기가 실패하면 `state_error`로 `on_failure`를 따른다.
6. `decide.Act`. 보류면 `held_writes`에 넣는다(상한 500ms). 실패해도 막고, `DETAIL`에서 `held_write`가 빠지고 에러 로그를 남긴다.
7. 막으면 `proxy.Refusal{Code, Message, Detail: decide.Detail(...), Hint: action.Hint(heldID)}`을 돌려준다.

`End`: 막은 쓰기는 `executed = false`, `rows_affected` NULL, `refused`에 코드, `exec_error`에 `SQLSTATE <코드>: <Message>`를 적고, 예약은 서버가 거부한 것처럼 뺀다(`Lost`여도). `Undelivered`이고 보류 id가 있으면 `MarkUndelivered`. 보낸 쓰기는 지금처럼 기록하고 `refused`는 NULL이다. 두 모드 모두: 측정한 쓰기가 `commit`이었는데 실제 행 수가 `max_rows`를 넘었으면 이유에 `impact:exceeded_after`를 더하고 경고 로그를 남긴다. 모델의 답과 지연은 `Begin`에서 얻은 것을 기록한다. `latency_ms`는 `Begin`에 든 시간 전체다.

- [ ] **Step 1: 실패하는 테스트** (`setupEnforce`, 에이전트는 `stet_md5`).

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestEnforceSendsACommittedWrite` | 단일 행 update(`calm`) | 실행된다. 기록: `mode enforce`, `commit`, `executed`, `refused` NULL, 질문과 답이 있다 |
| `TestEnforceCommittedWritesAnswerLikeADirectConnection` | `TestWritesBehaveTheSameWithImpactOn`의 방식으로, `commit`으로 판정되는 문장들(단일 행 insert·update·delete, 0행 update, PK 중복 insert, 한도 아래 다중 행 update)을 단순·확장 프로토콜로 | 명령 태그, SQLSTATE, 테이블 내용이 쌍둥이 테이블에 직접 보낸 것과 같다 |
| `TestEnforceHoldsWhatTheModelFlags` | `policy_1`을 어겼다고 답하는 모델, `update … set status = $1 where id = $2` | `ST001`. `Message`는 `write held by stet: it breaks policy rule 1 ("Do not set status to churned.")`. `DETAIL`에 판단 기록 id와 보류 id, `HINT`에 그 보류 id. 행은 그대로. 보류 줄: 문장, `login_role stet_md5`, `database stet_test`, `target_pk`, `row_version`(읽은 `xmin`), `verdict hold`, `reasons [policy:1]`, `status pending`. 기록: `refused ST001`, `executed false`, `rows_affected` NULL, `exec_error`가 `SQLSTATE ST001: write held by stet:`로 시작 |
| `TestHeldWriteKeepsBinaryAndNullParamsAsText` | 위 모델로 `update … set status = $1, title = $2 where id = $3`에 `"churned"`, `nil`, `int32(2)`(pgx는 정수를 바이너리로 보낸다) | 보류 줄의 `params`는 `["churned", null, "2"]`, `param_types`의 길이는 3 |
| `TestEnforceSkipsTheModelWhenTheRulesDecide` | `impactYAML`: 한도를 넘는 다중 행 update. `cumYAML`: 한도를 채운 뒤의 단일 행 update | 둘 다 `ST001`(`impact:rows`의 문구는 `more than 3 rows would be updated, over the limit of 3`, `impact:cumulative`)이고 모델 호출이 0이다. 행은 그대로이고 보류 줄이 있다 |
| `TestEnforceEscalatesAnEstimate` | `looseYAML`: 세지 못하는 조건식의 update | `ST001`, 보류 줄의 `verdict escalate`, `reasons [impact:uncertain]` |
| `TestEnforceRefusesUnsupportedShapes` | upsert. 같은 문장을 `unsupported: pass`인 설정에서 | `ST002`이고 보류 줄이 없다. `pass`에서는 실행되고 기록은 `unsupported`, `refused` NULL |
| `TestEnforceRefusesWhenMeasuringFails` | `impactYAML`, 에이전트 계정을 `stet_trust`로 둔 설정(Stet이 그 역할이 될 수 없다)에서 다중 행 update | `ST002`, `reasons [not_single_row]`, 행은 그대로 |
| `TestEnforceJudgesTheFirstWriteAfterBegin` | `begin`을 단순 프로토콜로 보내고 답을 받은 뒤 인자 있는 update(psycopg 기본 설정의 모양), 커밋. 그리고 한 Sync 아래에 `begin`과 update(pgconn 파이프라인) | 둘 다 자동 커밋과 같은 판정을 받고 실행된다. 모델이 hold를 답하면 둘 다 `ST001`이다 |
| `TestEnforceJudgesASingleValueIn` | hold를 답하는 모델, `update … where id in ($1)` | `ST001`, 모델 호출 1 |
| `TestEnforceRefusesAWriteInADirtyTransaction` | `begin`, `select 1`, update. 이어서 `select 1`, `rollback`, update | `ST002`(`in_transaction`), 25P02, 성공, 실행됨. 첫 update에 모델 호출이 없고 `state`가 NULL이다 |
| `TestEnforceRefusesWithoutALoginRole` | `e.gate.Begin(proxy.SessionInfo{ID: "x", Database: "stet_test"}, e.gate.Inspect(sql), nil)`을 직접 부른다. 그리고 `earlyQuery`를 `stet_trust`가 에이전트인 설정에서 | `Refusal`의 `Code`가 `ST002`이고 `Detail`에 `no_login_role`. `earlyQuery`는 `ST002`를 돌려주고 행은 그대로다 |
| `TestEnforceFailuresFollowOnFailure` | 모델 에러. 깨진 후보 SQL. 각각 기본 설정과 `on_failure: pass` | 기본: `ST001`(`model_error`, `state_error`)이고 보류 줄이 있다. `pass`: 실행되고 기록은 `commit [model_error]`, `commit [state_error]` |
| `TestEnforceHoldsWhenTheCumulativeUsageIsUnknown` | `cumYAML`, `FailUsageLoads(g, 1)` | `ST001`, `reasons [impact:cumulative_unknown]`, 모델 호출 0 |
| `TestEnforceHoldsWhenTooManyWritesAreInFlight` | `max_inflight: 1`, `HoldInflight` | 약 500ms 뒤 `ST001`(`state_error`) |
| `TestEnforceRefusesEvenWhenTheHeldWriteIsNotStored` | `FailHeldWrites(g, 1)`, hold를 답하는 모델 | `ST001`. `DETAIL`에 `held_write`가 없고 `HINT`에 `this write`. 보류 줄이 없다. 행은 그대로. 에러 로그 |
| `TestEnforceRejudgesWhenTheRowChanged` | `onDecide`가 첫 호출에서 관리자 연결로 그 행을 고친다 | 실행된다. 기록 `commit [rejudged]`, 모델 호출 2. 기록의 `state`는 두 번째 읽은 것이다 |
| `TestEnforceHoldsWhenTheRowKeepsChanging` | `onDecide`가 호출마다 그 행을 고친다 | `ST001`, `reasons [concurrent_change rejudged]`, 모델 호출 2, 행에는 관리자의 값만 있다 |
| `TestEnforceSendsAWriteWhoseRowIsGone` | `onDecide`가 그 행을 지운다 | 에러 없이 `UPDATE 0`, 기록 `commit`, `rows_affected 0` |
| `TestEnforceMarksAHiddenHoldUndelivered` | pgconn 파이프라인으로 한 Sync 아래에 `SendPrepare("x", "select * from no_such_table", nil)`과 hold될 update | 클라이언트는 42P01을 받는다. 보류 줄의 `status`가 `undelivered`가 된다 |
| `TestEnforceLeavesTheServersOwnErrorAlone` | `commit`으로 판정되는 PK 중복 insert | 23505가 그대로 온다. 기록: `refused` NULL, `executed false`, `exec_error`에 23505 |
| `TestRetriedHeldWriteDoesNotUseUpTheCumulativeLimit` | `cumYAML`(한도 5). `policy_1`만 어겼다고 답하는 모델로 같은 update를 6번 보낸다. 그 뒤 모델을 `calm`의 답으로 바꾸고 다른 행의 update | 6번 모두 `ST001`이고 보류 줄이 6개다. 마지막은 실행되고 `cumulative.used`가 0이다 |
| `TestMeasuredWriteOverTheLimitAfterTheFact` | `impactYAML`. `Begin`을 직접 불러 2행으로 측정된 update를 통과시킨 뒤 `End`에 `Outcome{Executed: true, Rows: 5}`를 준다. 섀도 설정에서도 한 번 | 기록 `commit [impact:exceeded_after]`, 경고 로그 |
| `TestEnforceLeavesOtherRolesAlone` | `stet_scram`이 프록시로 붙어 hold될 update와 DO 블록을 보낸다 | 둘 다 실행되고 그 테이블의 판단 기록이 없다 |
| `TestEnforceClosedPointsAnswerThroughTheGate` | 에이전트가 `selec 1`, DO 블록 | `ST002`이고 `Message`가 `decide.Blocked`의 것이다. 판단 기록이 없다 |
| `TestShadowAndEnforceAgreeOnVerdicts` | 같은 문장 여덟 개(통과하는 단일 행 쓰기, 모델이 hold하는 쓰기, 한도를 넘는 다중 행, 한도 아래 다중 행, upsert, `begin` 바로 뒤의 쓰기, 다른 문장 뒤의 쓰기, 값 하나짜리 `IN`)를 섀도 환경과 enforce 환경에 같은 모델 답으로 보낸다. 문장끼리 서로 다른 행을 건드린다 | `(verdict, reasons)`의 목록이 같다. 섀도에서는 여덟 개가 모두 실행되고 `refused`가 모두 NULL이다 |

- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/ -run 'Enforce|HeldWrite|Retried|AfterTheFact|Agree'` → FAIL.
- [ ] **Step 3: 구현.** `Begin`은 섀도에서 지금 그대로이고(`refuse`는 nil), enforce에서 위 순서를 탄다. 판정과 모델의 답은 `pending`에 두고 `judge`가 그것을 기록한다(enforce의 `judge`는 모델을 다시 부르지 않는다). `Facts`는 테이블 설정과 `pending`에서 채운다. 패키지 주석의 "shadow mode only"를 고친다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → PASS. 섀도의 기존 테스트는 고치지 않는다.
- [ ] **Step 5:** 커밋 `Gate: judge before sending in enforce mode, hold what is refused, and re-check the row version`.

---

### Task 12: `stet proxy` — enforce로 시작하기, 시작할 때의 확인, 종료 순서

**Files:** Create `cmd/stet/check.go`, `cmd/stet/main_test.go`. Modify `cmd/stet/main.go`, `stet.example.yaml`. Test `internal/config/config_test.go`(예시 설정).

**Interfaces — Consumes:** `Config.AgentRoles` (Task 1). `sqlparse.PostgresMajor` (Task 3). `proxy.Policy` (Task 5, 8).

**Produces (패키지 `main`):**

```go
// run serves cfg on ln until ctx is done, then stops in order: the proxy
// first, waiting for its sessions to report their last statements; then the
// gate, which records what it still holds; then Stet's own pool.
func run(ctx context.Context, cfg *config.Config, ln net.Listener, log *slog.Logger) error

// check looks at the database before serving (spec 7, 8). Warnings go to
// the log; an error stops the start.
func check(ctx context.Context, pool *pgxpool.Pool, cfg *config.Config) (warnings []string, err error)

// newerThanParser reports whether a server_version_num is of a major
// version above sqlparse.PostgresMajor.
func newerThanParser(serverVersionNum int) bool
```

- [ ] **Step 1: 실패하는 테스트** (`main_test.go`. 통합 테스트는 `STET_TEST_PG`가 없으면 건너뛴다. 설정의 모델은 `{adapter: none, on_failure: pass}`).

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestProxyCommandNamesTheMissingAgentRoles` | `mode: enforce`이고 `agents`가 없는 설정 파일로 `runProxy` | 에러에 `agents.roles`가 있다 |
| `TestRunServesInEnforceMode` | `agents.roles: [stet_md5]`, `impact.max_rows: 3`인 테이블(superuser로 `grant stet_md5 to stet_scram`, 테이블 권한은 `stet_md5`에게). `run`을 고루틴으로 띄우고 `stet_md5`로 붙는다 | 단일 행 update는 실행되고, 모든 행의 update는 `ST001`이다. `ctx`를 취소하면 `run`이 nil로 돌아온다 |
| `TestRunRecordsWhatWasInFlightAtShutdown` | 섀도, `reads`를 켠 테이블. 읽기 하나를 마친 뒤 `insert … select … from pg_sleep(2)`를 보내고, 서버가 그 문장을 실행 중인 것을 `pg_stat_activity`로 확인한 다음 `ctx`를 취소한다 | `run`이 돌아온 뒤 그 쓰기의 판단 기록이 있고(`executed false`), 읽기의 집계가 `stet.reads`에 있다 |
| `TestCheckWarnsAboutAgentRoles` | 역할마다 `check`: 없는 역할. `postgres`(superuser). `stet` 스키마의 `USAGE`를 잠시 받은 `stet_clear`. 게이트 대상 테이블을 가진 `stet_md5`(그 계정으로 테이블을 만든다). 역할 수준 `search_path`를 잠시 다르게 둔 `stet_clear`(superuser로 `ALTER ROLE`). 문제없는 `stet_md5` | 경고에 차례로 `does not exist`, `superuser`, `schema stet`, `owns`, `search_path`가 그 역할 이름과 함께 있다. 마지막은 경고가 없다. 테스트가 바꾼 것은 `t.Cleanup`으로 되돌린다 |
| `TestCheckRefusesStetsOwnAccount` | `stet_scram`으로 연 풀과 `AgentRoles`가 `[stet_scram]`인 `Config`(직접 만든다. `upstream` URL에 사용자가 없어 설정 검증이 잡지 못한 경우다) | 에러에 `agents.roles`가 있다 |
| `TestCheckWarnsAboutPassSettings` | enforce에서 `on_failure: pass`, 테이블의 `unsupported: pass` | 경고가 각각 하나. 섀도에서는 없다 |
| `TestNewerThanParser` | 140018, 170004, 180000 | false, false, true |

  `TestLoadExampleConfig`에 단언을 더한다: 예시는 섀도이고 `AgentRoles`가 비어 있으며 `MaxInflight == 32`다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./cmd/stet/` → FAIL.
- [ ] **Step 3: 구현.**
  - `runProxy`는 플래그, 설정, 로거, 신호 컨텍스트, `net.Listen`만 하고 `run`을 부른다. enforce를 거부하던 검사를 없앤다.
  - `run`: 테이블이 있으면 풀을 열고, 마이그레이션하고, `check`의 경고를 `log.Warn`으로 남기고(에러면 시작하지 않는다), 게이트를 만든다. `proxy.New`에 `Policy{Agents: cfg.AgentRoles, Enforce: cfg.Mode == config.ModeEnforce}`를 준다. 종료 순서는 지금 그대로다(프록시의 `Close`가 끝난 뒤 게이트의 `Close`). 테이블이 하나도 없으면 enforce에서도 통과만 하고, 그 사실을 경고로 남긴다.
  - `check`: 서버 버전(`server_version_num`)이 분류기보다 새면 경고한다(두 모드 모두). enforce에서는: `on_failure: pass`와 `unsupported: pass`인 테이블마다 경고. 에이전트 역할마다 있는지, superuser가 아닌지, `stet` 스키마에 `USAGE`나 `CREATE`가 없는지, 게이트 대상 테이블의 소유 역할의 구성원이 아닌지, 역할 수준 `search_path`(`pg_db_role_setting`, 모든 DB용과 이 DB용)가 있으면 Stet 연결의 것과 같은지 본다. 풀의 접속 계정이 `agents.roles`에 있으면 에러다.
  - `stet.example.yaml`: `mode` 아래에 주석 처리한 `agents: {roles: [...]}`와 `limits: {max_inflight: 32}`를 한국어 주석과 함께 넣는다. `on_failure`의 주석에 enforce의 기본값이 `hold`라고 적고, `adapter: none`은 enforce에서 `on_failure: pass`를 직접 적어야 한다고 적는다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → PASS. `go build -o bin/stet ./cmd/stet`.
- [ ] **Step 5:** 커밋 `Stet: start in enforce mode, check agent roles at startup, and test the shutdown order`.

---

### Task 13: 마무리

**Files:** Modify `docs/spec-v0.2-enforce.md`, `README.md`.

- [ ] **Step 1: 전체를 확인한다.**

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...` → 모든 패키지 `ok`.
Run: `go build -o bin/stet ./cmd/stet && go vet ./... && gofmt -l .` → 출력 없음.
Run: `STET_TEST_PG=127.0.0.1:55432 go test -race -count=10 ./internal/proxy/ -run 'Refus|Unsure|TxDirty|CleanTransactions|Sync|Copy|ServerHolds'` 와 `… ./internal/gate/ -run 'Enforce|Cumulative|Inflight|LoginRole'` → 실패와 경쟁이 없다. 흔들리는 테스트가 있으면 숨기지 않고 보고한다. `earlyQuery`를 쓰는 테스트는 따로 `-count=50`으로 돌려 본다.

- [ ] **Step 2: 바이너리를 enforce로 띄워 본다.** 테스트 DB에 `stet_scram`으로 테이블 하나를 만들어 일곱 행을 넣고 `stet_md5`에게 권한을 준다. 설정은 `mode: enforce`, `agents: {roles: [stet_md5]}`, `upstream: postgres://stet_scram:scram-pw@127.0.0.1:55432/stet_test`, `model: {adapter: none, on_failure: pass}`, 그 테이블에 `impact: {max_rows: 3}`. `bin/stet proxy -config <파일>`을 띄우고 `psql`로 `stet_md5`로 붙어(`\set VERBOSITY verbose`) 다음을 보내 출력을 보고서에 붙인다: 단일 행 update(`UPDATE 1`). 모든 행의 update(`ST001`과 `DETAIL`·`HINT`). `begin; select 1;` 뒤의 update(`ST002`)와 `rollback`. `do $$ begin null; end $$`(`ST002`). 그 뒤 `select 1`(세션이 살아 있다). `stet_scram`으로 붙어 모든 행의 update(실행된다). `stet.held_writes`와 `stet.decisions`에서 그 테이블의 줄을 확인한다. 끝나면 프록시를 멈추고 테이블과 그 테이블의 `held_writes` 줄을 지운다.
- [ ] **Step 3: 스펙을 맞춘다.** 상태 줄을 `상태: 구현됨`으로 바꾼다. 16장에서 테스트로 확인된 항목을 `[x]`로 바꾸고 테스트 이름을 적는다(아래 표). 확인하지 못한 항목은 그대로 두고 이유를 적는다. "스펙 보정"의 B1~B10을 해당 장(6·7·8·9·12·13장)에 코드가 실제로 하는 대로 옮겨 적는다. 구현하면서 더 달라진 곳과 13장에 더할 한계가 있으면 코드가 하는 것만 적는다. README의 상태 표에서 enforce 줄을 지금 상태로 고친다.
- [ ] **Step 4:** 커밋 `Spec: mark minimal enforce as implemented`.

---

## 스펙의 항목과 Task

**16장 수용 기준**

| 기준 | Task | 테스트 |
|---|---|---|
| enforce로 시작한다. `agents.roles`가 없으면 시작하지 않는다 | 1, 12 | `TestInvalidConfigs`(enforce needs agent roles), `TestProxyCommandNamesTheMissingAgentRoles`, `TestRunServesInEnforceMode` |
| `commit`인 쓰기는 실행되고 응답이 직접 연결과 같다 | 11 | `TestEnforceCommittedWritesAnswerLikeADirectConnection` |
| `hold`·`escalate`·`unsupported`는 실행되지 않고 해당 SQLSTATE를 받는다 | 11 | `TestEnforceHoldsWhatTheModelFlags`, `TestEnforceEscalatesAnEstimate`, `TestEnforceRefusesUnsupportedShapes` |
| 막힌 뒤에도 세션을 쓴다. 파이프라인에서도 응답의 수와 순서가 맞다 | 7 | `TestRefusedQueryGetsTheGatesError`, `TestRefusedExecuteGetsTheGatesError`, `TestRefusedPipelineAnswersLikeAServerError` |
| 한 Sync 아래에서 뒤의 쓰기가 막히면 앞 문장도 롤백된다 | 7 | `TestRefusalRollsBackTheRestOfItsSync` |
| 보류 건이 문장·값·계정과 함께 남고 `DETAIL`에 id가 있다 | 2, 11 | `TestInsertHeldWrite`, `TestEnforceHoldsWhatTheModelFlags`, `TestHeldWriteKeepsBinaryAndNullParamsAsText` |
| `BEGIN` 바로 뒤의 쓰기는 자동 커밋과 같은 판정을 받는다 | 5, 10, 11 | `TestTxDirtyFollowsWhatWasSent`, `TestShadowJudgesTheFirstWriteOfATransaction`, `TestEnforceJudgesTheFirstWriteAfterBegin` |
| 다른 문장 뒤의 쓰기는 `ST002`로 막히고 트랜잭션은 실패 상태가 된다 | 7, 11 | `TestRefusalFailsTheOpenTransaction`, `TestEnforceRefusesAWriteInADirtyTransaction` |
| 한 Sync 아래의 `BEGIN`과 쓰기가 판단된다 | 5, 11 | `TestGateSeesCleanTransactions`, `TestEnforceJudgesTheFirstWriteAfterBegin` |
| 다시 Parse해 서버가 거부한 이름, 건너뛴 Parse와 Close | 6 | `TestGateJudgesTheStatementTheServerHolds`, `TestSkippedParseAndCloseAreUndone` |
| 가려진 에러는 그대로 가고 보류 줄은 `undelivered`가 된다 | 7, 11 | `TestEarlierFailureHidesTheRefusal`, `TestEnforceMarksAHiddenHoldUndelivered` |
| 트랜잭션 안에서 막힌 뒤 `ROLLBACK`하면 정상으로 돌아온다 | 7, 11 | `TestRefusalFailsTheOpenTransaction`, `TestEnforceRefusesAWriteInADirtyTransaction` |
| `agents.roles`에 없는 계정은 그대로 실행되고 기록이 없다 | 5, 11 | `TestSessionsOfOtherRolesAreNotSeen`, `TestEnforceLeavesOtherRolesAlone` |
| 7장의 표의 줄마다 테스트가 있다 | 8, 10, 11 | 아래 표 |
| 행이 바뀌면 다시 판단하고, 또 바뀌면 `hold`(`concurrent_change`) | 11 | `TestEnforceRejudgesWhenTheRowChanged`, `TestEnforceHoldsWhenTheRowKeepsChanging`, `TestEnforceSendsAWriteWhoseRowIsGone` |
| 결정론적 규칙으로 판정이 나면 모델을 부르지 않는다 | 4, 11 | `TestDeterministic`, `TestEnforceSkipsTheModelWhenTheRulesDecide` |
| `IN (5)`와 `IN ($1)`이 단일 행 쓰기로 값 판단을 받는다 | 3, 11 | `TestClassifySingleValueIn`, `TestEnforceJudgesASingleValueIn` |
| 섀도의 판정이 enforce와 같고 섀도는 막지 않는다 | 11 | `TestShadowAndEnforceAgreeOnVerdicts` |
| 섀도의 기존 테스트는 허락된 판정 변경 말고는 고치지 않고 통과한다 | 4, 5, 10 | 고치는 단언은 Task 4(`TestVerdictCumulative`의 한 행), Task 5(`TestAbandonLosesUnansweredWrites`의 뒷부분), Task 10의 "고치는 기존 테스트" 표가 전부다 |
| 2ms 예산. 에이전트가 아닌 세션은 게이트를 부르지 않는다 | 5, 8 | `TestPassthroughLatencyBudget`, `TestObservedReadLatencyBudget`, `TestUnwatchedSessionLatencyBudget`, `TestEnforcedSessionLatencyBudget` |
| 12장의 표의 줄마다 테스트가 있다 | — | 아래 표 |
| `go test -race ./...`가 통과한다 | 13 | Step 1 |

**7장 "실패하면 막는다"의 줄**

| 지점 | Task | 테스트 |
|---|---|---|
| 분류기가 읽지 못한 문장 | 8, 11 | `TestUnreadableStatementsAreRefused`, `TestEnforceClosedPointsAnswerThroughTheGate` |
| 64MiB가 넘는 Query·Parse·Bind | 8 | `TestOversizedMessagesAreRefused` |
| FunctionCall 메시지 | 8 | `TestFunctionCallsAreRefused` |
| `DO` 블록 | 3, 8 | `TestClassifyDo`, `TestDoBlocksAreRefused` |
| 판단할 수 없는 형태의 쓰기 | 11 | `TestEnforceRefusesUnsupportedShapes` |
| 접속 계정을 모르는 세션의 쓰기 | 10, 11 | `TestWritesWithoutALoginRoleAreUnsupported`, `TestEnforceRefusesWithoutALoginRole` |
| 상태를 읽지 못함·시간 초과 | 11 | `TestEnforceFailuresFollowOnFailure` |
| 모델 실패·시간 초과 | 11 | `TestEnforceFailuresFollowOnFailure` |
| 영향 측정 실패 | 11 | `TestEnforceRefusesWhenMeasuringFails` |
| 누적 값을 읽지 못함 | 4, 10, 11 | `TestVerdictCumulative`, `TestCumulativeWaitsBeforeLoadingAgain`, `TestEnforceHoldsWhenTheCumulativeUsageIsUnknown` |
| 동시에 판단 중인 쓰기가 상한을 넘음 | 10, 11 | `TestInflightLimitTurnsIntoAStateError`, `TestEnforceHoldsWhenTooManyWritesAreInFlight` |
| `held_writes` 저장 실패 | 11 | `TestEnforceRefusesEvenWhenTheHeldWriteIsNotStored` |

**12장 "이 단계에서 닫는다"의 줄**

| 항목 | Task | 테스트 |
|---|---|---|
| 파싱 실패, 큰 메시지, FunctionCall, `DO`, 모델·상태 실패 | 8, 11 | 위 표의 해당 줄 |
| `IN` 하나짜리로 값 판단을 피함 | 3, 11 | `TestClassifySingleValueIn`, `TestEnforceJudgesASingleValueIn` |
| `테이블.함수` 표기의 조건식을 Stet이 실행함 | 3, 10 | `TestClassifyQualifiedNames`, `TestImpactDoesNotCountThroughATableQualifiedFunction` |
| 동시에 도는 측정에 상한이 없음 | 1, 10 | `TestAgentRolesAndLimits`, `TestInflightLimitTurnsIntoAStateError` |
| 누적 값의 첫 적재가 느리면 한도가 꺼짐 | 2, 10 | `TestUsageBySecond`, `TestUsageLoadsOnlyWhatCameBeforeItStarted`, `TestUsageRetriesAfterAnInterval`, `TestCumulativeWaitsBeforeLoadingAgain` |
| 누적 값 정리가 공용 잠금 아래에서 느림 | 10 | `TestUsageCountsPerSecond`, `TestUsageKeepsASecondUntilItLeftTheWindow`, `TestUsageDropsIdlePairs` |
| 프로토콜 3.1 시작 패킷의 계정을 못 읽음 | 5, 10 | `TestStartupUserIsReadAsTheServerReadsIt`, `TestProtocol31SessionHasItsLoginRole`, `TestProtocol31SessionsRecordTheirLoginRole` |
| 인증 전 문장이 한도를 피함 | 10, 11 | `TestWritesWithoutALoginRoleAreUnsupported`, `TestEnforceRefusesWithoutALoginRole` |
| CommandComplete 뒤에 끊긴 쓰기가 "실행 안 됨"으로 기록됨 | 5 | `TestAbandonKnowsACompletedWriteRan` |
| 프록시의 문장 표가 서버와 어긋남 | 6, 9 | Task 6의 테스트 전부, `TestUnsureNameIsRefused`, `TestSQLExecuteOfAPreparedWriteIsRefused`, `TestFetchFromAWritePortalIsRefused`, `TestQueryBeforeSyncIsFollowed`, `TestCopyFromStdinThroughTheExtendedProtocolEndsTheSession`, `TestSyncDuringCopyEndsTheSession` |
| 종료 순서의 자동 테스트가 없음 | 12 | `TestRunRecordsWhatWasInFlightAtShutdown` |
