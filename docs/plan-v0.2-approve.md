# 보류 건의 승인과 거부 (2b) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 검토자가 `stet.review`에서 보류 건을 보고 `stet.approve(id)`·`stet.reject(id)`를 부르면, 승인된 쓰기를 실행 중인 Stet이 에이전트 계정의 권한으로, 사람이 본 것까지만 실행한다.

**Architecture:** 보류 줄은 `unconfirmed`로 들어가고 게이트가 거부의 전달을 확인한 뒤에만 `pending`이 된다. 두 함수는 줄의 상태만 바꾼다. 새 패키지 `internal/apply`의 실행부가 1초마다 `approved`인 줄을 집어, 줄마다 새 연결과 한 트랜잭션으로 확인하고 실행한 뒤 결과를 그 줄에 적는다. 프록시는 enforce 세션의 `TimeZone`·`DateStyle`·`IntervalStyle`을 따라가 게이트에 넘기고, 게이트는 그것을 줄에 저장한다.

**Tech Stack:** Go 1.27, `github.com/jackc/pgx/v5` v5.11.0 (`pgconn`, `pgproto3`, `pgxpool`), `github.com/pganalyze/pg_query_go/v6`, PL/pgSQL, 테스트 서버 PostgreSQL 14.18.

**Spec:** [docs/spec-v0.2-approve.md](spec-v0.2-approve.md). 앞 문서는 [spec-v0.2-enforce.md](spec-v0.2-enforce.md) 5·9·11·13·17장이다. 스펙과 이 계획이 다르면 아래 "스펙 보정"에 적힌 곳만 계획을 따르고, 나머지는 스펙을 따른다. "스펙 보정"은 실행을 맡은 쪽이 받아들였다(B1~B9). 소유자에게는 끝날 때 보고한다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 규칙 6: 섀도 모드에서는 실행부를 켜지 않고, 섀도 세션은 지금처럼 ParameterStatus를 보지 않고, 전달되는 바이트는 두 모드 모두 그대로다. 규칙 3: `stet.decisions`에 줄을 더하지도 고치지도 않는다. 규칙 1: 실행부의 트랜잭션은 모델을 부르지 않는다. 규칙 5: 결정론적 확인(행 버전, 행 수 상한, 트리거)이 사람의 승인과 별개로 적용된다. 규칙 7: `decide.Decider`와 판정 규칙은 바꾸지 않는다.
- 스펙의 값. 상태: `unconfirmed`, `pending`, `undelivered`, `approved`, `rejected`, `applied`, `failed` (뷰에서만 `expired`). SQLSTATE: 트리거 `ST004`, 함수의 거절 `ST005`. 에이전트는 지금처럼 `ST001`만 받는다. 실행부의 `error`: `approval_lapsed`, `other_database`, `not_gated`, `row_changed`, `search_path_differs`, `role_changed`, `rows_exceeded: <n> rows, approved up to <m>`, `SQLSTATE <코드>: <서버의 메시지>`, 그리고 보정 B5의 `no_row_limit`. 집는 간격 1초, 승인 뒤 10분(설정이 아니다), `held.expire_after` 기본 `24h`(1m 이상 30d 이하), `held.apply_timeout` 기본 `30s`(1s 이상 10m 이하).
- 테스트: `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. PostgreSQL 14.18이 이미 떠 있다(superuser `postgres`/`postgres-pw`, 데이터베이스 `stet_test`, 계정 `stet_scram`/`scram-pw`, `stet_md5`/`md5-pw`, 비밀번호 없는 `stet_trust`). 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다. store, gate, proxy, apply, cmd를 건드리는 Task는 반드시 이 변수로 돌리고, 모든 Task는 이 명령이 모든 패키지에서 `ok`인 채로 끝난다.
- 테스트 계정: Stet 자신은 `stet_scram`, 에이전트는 `stet_md5`다. **`stet_trust`를 어떤 역할에도 부여하지 않고, 어떤 역할도 `stet_trust`에 부여하지 않는다.** PUBLIC, `public` 스키마, 공유 데이터베이스 `stet_test` 자체에는 GRANT·REVOKE하지 않는다. 그런 권한이 달라야 하는 테스트는 자기 데이터베이스나 역할을 만들고 지운다(`cmd/stet`의 `cleanDatabase`, `createRole`). `stet` 스키마 안에서 두 새 함수의 `EXECUTE`를 PUBLIC에서 거두는 것은 이 단계의 일이므로 된다. 검토자 역할은 테스트가 만들고(`CREATE ROLE ... LOGIN PASSWORD`, scram으로 로그인된다) `DROP OWNED BY`와 `DROP ROLE`로 지운다.
- TDD: 동작이 바뀌는 곳은 실패하는 테스트를 먼저 쓰고, 돌려서 실패를 본 뒤에 구현한다.
- `internal/proxy`를 건드리는 Task는 `internal/proxy/latency_test.go`(p95 2ms)도 돌린다. 그 파일의 기존 테스트는 고치지 않는다.
- 기존 테스트의 단언은 Task가 이름을 적어 허락한 곳만 고친다. 테스트용 게이트와 헬퍼를 새 필드에 맞추는 것은 단언 변경이 아니다.
- README.md, docs/concept-v0.2.md, docs/decisions.md(소유자의 문서)와 앞 단계의 계획 문서는 고치지 않는다. `internal/lab`은 고치지 않는다.
- 문서와 YAML·SQL 스키마 주석은 한국어, 코드의 주석·식별자·에러 메시지·로그는 영어다.
- 브랜치 `v0.2-approve`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고 마지막 줄은 `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`이다.

## Review Focus

스펙이 암시하지만 수용 기준에 줄이 없는 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **바이너리와 텍스트가 섞인 바인딩.** 정수는 바이너리로, 날짜·시각은 타입 없는 텍스트로 한 Bind에 섞어 보낸다(JDBC가 그렇게 보내는 것으로 알려져 있다). 텍스트 값은 온 바이트 그대로 저장되고, 승인하면 에이전트 세션의 `DateStyle`·`TimeZone`으로 읽힌 값이 들어간다. (Task 5 `TestHeldWriteKeepsTextValuesAsSentAmongBinaryOnes`, Task 7 `TestApprovedWriteRunsAsTheAgentSentIt`)
2. **이 단계 전에 보류된 줄.** enforce 단계가 넣은 `pending` 줄은 `expires_at`도 `planned_rows`도 없다. 스키마를 올린 뒤 뷰에 `expired`로 보이고, 승인은 `ST005`로 거절되고, 거부는 된다. (Task 3 `TestRowsHeldBeforeThisStageCannotBeApproved`)
3. **두 검토자가 같은 줄을 동시에.** 한 사람이 승인하고 다른 사람이 거부하면 하나만 되고, 진 쪽은 지금 상태를 말하는 `ST005`를 받는다. 같은 줄을 두 번 승인해도 같다. (Task 3 `TestConcurrentApproveAndRejectLetOneWin`)
4. **Stet이 그 계정이 될 수 없다.** 보류된 뒤 구성원 자격이 거둬졌거나 처음부터 없던 계정의 줄을 승인하면 `failed`와 `SQLSTATE 42501: ...`이 남고, 실행부는 다음 줄로 간다. (Task 6 `TestApplyFailsWhenStetCannotBecomeTheRole`)
5. **인용이 필요한 이름.** 대문자가 섞인 계정 이름과 스키마까지 적은 테이블 키(`crm.contacts`)의 줄도 실행된다. (Task 6 `TestApplyQuotesRoleAndTableNames`)

## 스펙 보정

코드와 PostgreSQL 14.18에 맞춰 보니 스펙대로는 되지 않거나 스펙이 정하지 않은 곳이다. 가장 작은 보정을 제안하고, Task 9가 스펙에 옮겨 적는다.

| # | 스펙 | 확인한 것 | 보정 (Task) |
|---|---|---|---|
| B1 | 2·4장 "텍스트로 온 값은 온 그대로" | `gate.paramTexts`(gate.go)는 값 하나라도 바이너리면 텍스트 값까지 Stet의 풀 연결에서 `$n::text`로 다시 읽는다. 타입 없는 텍스트 `'01/02/2026'`이 Stet의 `DateStyle`로 읽혀 `2026-01-02`로 저장된다. JDBC가 이 모양으로 보내는 것으로 알려져 있다 | 텍스트 형식(`Format == 0`)으로 온 값은 풀어 읽은 결과를 쓰지 않고 온 바이트를 그대로 둔다. 판단 기록의 `params`도 같아진다(enforce 스펙 9장의 문장과 맞는다) (5) |
| B2 | 4장 "응답을 기다리지 않고 이어 보낸 쓰기는 이미 `in_transaction`으로 막히므로 남는 경우는 한 Sync 아래의 것뿐이다" | `session.txDirty`는 더러운 단위만 센다. `SET`은 깨끗한 문장이어서, `SET TimeZone`을 Query로 보내고 답을 기다리지 않고 보낸 쓰기는 막히지 않고 판단받는다. Sync 없이 Query가 뒤따른 확장 프로토콜 단위(unsynced)는 ReadyForQuery 없이 끝난다. `RESET`은 깨끗하지 않아서 그 뒤의 쓰기는 보류되기 전에 `in_transaction`으로 막힌다 | 깃발은 "아직 ReadyForQuery가 답하지 않은 단위 어디든, 또는 쓰기 자신의 메시지에 `SET`이나 `RESET`이 있다"로 본다. unsynced 단위가 든 것은 다음 ReadyForQuery까지 세션이 들고 있는다 (4) |
| B3 | 4장 `in_transaction`: "명시적 트랜잭션의 첫 쓰기였다" | 프록시의 `Outcome.InTransaction`은 명시적 트랜잭션뿐 아니라 한 Sync 아래 Execute가 둘 이상인 단위와 unsynced 단위에서도 참이다. 검토자에게 뜻은 같다(함께 보낸 다른 문장은 실행되지 않았다) | 프록시가 보고한 값을 그대로 저장하고, 컬럼 주석과 5장의 설명을 "명시적 트랜잭션이거나 한 Sync 아래 다른 문장과 함께 온 쓰기"로 고친다 (5, 9) |
| B4 | 4장 트리거의 고정 목록에 `id`, `decision_id`, `created_at`, `database`, `param_types`, `target_pk`가 없다 | `param_types`는 값이 읽히는 타입을, `target_pk`와 `database`는 실행부의 확인 대상을 정한다 | 바뀌어도 되는 컬럼(`status`, `in_transaction`, `resolved_by`, `resolved_at`, `note`, `max_rows`, `applied_at`, `rows_affected`, `error`)을 뺀 전부를 고정한다. 나중에 더해지는 컬럼도 기본이 고정이다. 그 아홉 가운데 `status`와 `in_transaction`만 스펙의 규칙으로 묶고, 나머지는 스펙대로 트리거가 막지 않는다 (2) |
| B5 | 6장의 상한은 `max_rows`, 없으면 `planned_rows` | 둘 다 NULL인 줄이 있을 수 있다(이 단계 전의 줄, `rows_exact`가 NULL인 줄, 함수를 거치지 않고 고친 줄). 실행부가 고를 `error`가 스펙에 없다 | `stet.approve`는 `max_rows`가 없고 `rows_exact IS NOT TRUE`이거나 `planned_rows IS NULL`이면 거절한다. 실행부는 상한이 없는 줄을 실행하지 않고 `failed`(`no_row_limit`)로 적는다. 새 `error` 값 하나다 (3, 6) |
| B6 | 7장의 두 표에 4단계(`SET LOCAL ROLE`, 세 설정)의 실패가 없다. 10장은 구성원이 아니면 "`failed`가 된다"고 한다 | 구성원이 아니면 `SET LOCAL ROLE`이 `42501`로, 역할이 지워졌으면 `22023`으로 실패한다 | 4단계가 서버 에러로 실패하면 문장의 실패처럼 저장점까지 되돌리고 `failed`(`SQLSTATE <코드>: <메시지>`)로 적는다. 연결이 끊긴 것은 Stet 자신의 실패로 본다 (6) |
| B7 | 7장 2단계 "없으면 그만둔다" | `REPEATABLE READ`에서는 다른 Stet이 그 줄을 끝내고 커밋한 직후 `FOR UPDATE SKIP LOCKED`가 빈 결과가 아니라 `40001`(`could not serialize access due to concurrent update`)로 실패한다(테스트 서버에서 확인). 또 격리 수준은 줄을 읽기 전에 정해야 한다 | 집는 질의의 `40001`은 "다른 Stet이 처리했다"로 보고 조용히 그만둔다. 목록 질의가 줄마다 `row_version`이나 `row_absent`가 있는지를 함께 돌려준다(고정 컬럼이라 트랜잭션 밖에서 읽어도 같다) (3, 6) |
| B8 | 12장은 enforce 스펙의 9·13·17장만 고친다고 한다 | enforce 스펙 8장의 "Stet 계정이 각 에이전트 계정의 구성원인지는 시작할 때 확인하지 않는다"도 10장의 새 경고로 낡는다 | Task 9가 그 문장도 고친다 (9) |
| B9 | 4·6장 `resolved_at`(`now()`), 6장의 만료 비교 | `now()`는 트랜잭션이 시작한 시각이다. 검토자가 오래 열어 둔 트랜잭션에서 승인하면 `resolved_at`이 실제보다 이르게 적히고, 만료된 줄이 승인될 수 있다 | 두 함수는 `resolved_at`과 `expires_at` 비교에 `clock_timestamp()`를 쓴다. 실행부의 10분 비교도 `clock_timestamp()`다. 뷰의 `expired` 표시는 스펙대로 `now()`다 (3, 6) |

**스펙이 구현에 맡겨 이 계획이 정한 것.** 실행부는 새 패키지 `internal/apply`에 둔다(게이트의 판정 코드와 섞이지 않고, 필요한 것은 풀·저장소·설정뿐이다). 끌 때 기다리는 상한의 "몇 초"는 5초다. `approved` 목록은 풀로 읽고(한 번에 100줄까지), 줄의 처리만 새 연결에서 한다. `ST005`·`ST004`의 영어 문구와 확인 순서는 Task 2·3이 정한다. `search_path` 비교는 시작할 때의 확인과 같은 함수(`SchemaNames`)로 한다.

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/config/config.go` | `held.expire_after`, `held.apply_timeout` (수정) |
| `internal/store/schema.sql` | 4장의 컬럼·인덱스·트리거, 5장의 뷰, 6장의 두 함수와 권한 거두기 (수정) |
| `internal/store/held.go` | 보류 줄을 넣고 `pending`·`undelivered`로 바꾸기 (수정) |
| `internal/store/approved.go` | `approved`인 줄의 목록, 집기, 결과 적기 (신규) |
| `internal/sqlparse/classify.go` | `Result.Sets` (수정) |
| `internal/proxy/settings.go`, `session.go` | 세 설정 따라가기, `SET` 깃발, `SessionInfo`로 넘기기 (수정) |
| `internal/gate/gate.go`, `enforce.go` | 보류 줄의 새 내용, 저장할 수 없는 경우, `End`의 상태 단계, B1 (수정) |
| `internal/apply/apply.go` | 한 줄의 처리(7장) (신규) |
| `internal/apply/loop.go` | 1초마다 집는 루프와 멈추기 (신규) |
| `internal/apply/searchpath.go` | `SchemaNames`(`cmd/stet/check.go`에서 옮김), 역할 수준 `search_path` 읽기 (신규) |
| `cmd/stet/main.go`, `check.go` | 실행부 켜고 끄기, 풀의 `DateStyle`·`IntervalStyle`, 10장의 두 경고 (수정) |

---

### Task 0: 계획 커밋

- [ ] **Step 1:** `git switch v0.2-approve`(이미 있다). `git status`에 이 계획만 추적되지 않은 파일로 보여야 한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`가 모든 패키지에서 `ok`인지 확인한다.
- [ ] **Step 3:** 커밋 `Plan: approving and rejecting held writes (step 2b)`.

---

### Task 1: 설정 `held.*`

**Files:** Modify `internal/config/config.go`. Test `internal/config/config_test.go`.

**Interfaces — Produces:**

```go
// in Config:
Held Held

// Held bounds the approval of held writes (spec-v0.2-approve 9). Validated in both modes; used in enforce only.
type Held struct {
	ExpireAfter  time.Duration // held.expire_after: how long a held write can be approved; default 24h
	ApplyTimeout time.Duration // held.apply_timeout: the bound on running one approved write; default 30s
}
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestHeldDefaults`: `held`가 없는 섀도 설정과 enforce 설정 모두 `Held == {24h, 30s}`.
  - `TestHeldSettings`: `held: {expire_after: 2d, apply_timeout: 5s}` → `{48h, 5s}`. 경계 값 `1m`, `30d`, `1s`, `10m`은 유효하다. 하나만 적으면 다른 하나는 기본값이다.
  - `TestInvalidConfigs`에 행을 더한다(섀도 설정으로).

| 이름 | 설정 | 에러에 들어갈 문자열 |
|---|---|---|
| expire_after too short | `held: {expire_after: 59s}` | `held.expire_after` |
| expire_after too long | `held: {expire_after: 31d}` | `held.expire_after` |
| expire_after not a duration | `held: {expire_after: soon}` | `held.expire_after` |
| apply_timeout too short | `held: {apply_timeout: 999ms}` | `held.apply_timeout` |
| apply_timeout too long | `held: {apply_timeout: 10m1s}` | `held.apply_timeout` |
| held unknown key | `held: {expire: 1h}` | `expire` |

- [ ] **Step 2:** `go test ./internal/config/ -run 'Held|InvalidConfigs'` → FAIL (컴파일: `Held` 없음).
- [ ] **Step 3: 구현.** `rawConfig`에 `Held *struct{ ExpireAfter, ApplyTimeout string }`(yaml `expire_after`, `apply_timeout`)를 더하고 기존 `parseDuration`으로 읽는다. 빈 문자열은 기본값이다.
- [ ] **Step 4:** `go test ./internal/config/` → PASS (`TestLoadExampleConfig` 포함).
- [ ] **Step 5:** 커밋 `Config: held.expire_after and held.apply_timeout`.

---

### Task 2: 저장소 1 — 컬럼, 고정 트리거, 보류 줄 쓰기

**Files:** Modify `internal/store/schema.sql`, `internal/store/held.go`. Test `internal/store/held_test.go`, `internal/store/store_test.go`. 단언 한 곳: `internal/gate/enforce_test.go`의 `TestEnforceHoldsWhatTheModelFlags`.

**Interfaces — Produces:**

```go
// added to HeldWrite:
ExpiresAt   time.Time         // until when it can be approved
Settings    map[string]string // the session's TimeZone, DateStyle and IntervalStyle when it was held; nil is NULL
RowAbsent   bool              // its state was read and the target row was not there
PlannedRows *int64            // the rows measured when it was held; nil when it was not measured
RowsExact   bool              // PlannedRows is exact

// InsertHeldWrite stores a held write with status unconfirmed.
func (s *Store) InsertHeldWrite(ctx context.Context, h *HeldWrite) error
// MarkDelivered sets an unconfirmed held write to pending and records
// in_transaction. An id that is unknown or no longer unconfirmed is not an error.
func (s *Store) MarkDelivered(ctx context.Context, id string, inTransaction bool) error
// MarkUndelivered sets an unconfirmed held write to undelivered, with the same tolerance.
func (s *Store) MarkUndelivered(ctx context.Context, id string) error
```

트리거: 함수 `stet.held_writes_guard()`, 트리거 `held_writes_guard BEFORE UPDATE ON stet.held_writes FOR EACH ROW`. 어기면 `ERRCODE 'ST004'`다.

- [ ] **Step 1: 실패하는 테스트** (`held_test.go`. `sampleHeld`에 `ExpiresAt: now+24h`, `Settings` 셋, `PlannedRows: 1`, `RowsExact: true`를 더한다).
  - `TestInsertHeldWrite`를 고친다: `status = 'unconfirmed'`, `expires_at`, `settings = {"TimeZone": "UTC", "DateStyle": "ISO, MDY", "IntervalStyle": "postgres"}`, `row_absent = false`, `planned_rows = 1`, `rows_exact = true`, `in_transaction IS NULL`. `PlannedRows`와 `Settings`가 nil이면 NULL이다.
  - `TestMarkDelivered`: `unconfirmed` → `pending`이고 `in_transaction`이 준 값이다. 다시 불러도, 없는 id로 불러도 에러가 없고 값이 그대로다.
  - `TestMarkUndelivered`를 고친다: `unconfirmed` → `undelivered`. `pending`인 줄에 부르면 에러 없이 `pending`으로 남는다.
  - `TestHeldWritesContentIsFixed`: Stet의 풀에서 `statement`, `params`, `param_types`, `login_role`, `database`, `target_table`, `target_pk`, `row_version`, `row_absent`, `verdict`, `reasons`, `expires_at`, `settings`, `planned_rows`, `rows_exact`, `decision_id`, `created_at`를 하나씩 바꾸는 `UPDATE`가 모두 `ST004`다. `pending`이 된 뒤 `in_transaction`을 바꿔도, `unconfirmed`인 채로 `in_transaction`만 적어도 `ST004`다. `note`만 바꾸는 `UPDATE`는 된다.
  - `TestHeldWritesStatusFollowsTheArrows`: 되는 것 `unconfirmed→pending`, `unconfirmed→undelivered`, `pending→approved`, `pending→rejected`, `approved→applied`, `approved→failed`. `ST004`인 것 `pending→undelivered`, `pending→applied`, `unconfirmed→approved`, `approved→pending`, `approved→rejected`, 그리고 끝 상태 넷(`applied`, `failed`, `rejected`, `undelivered`)에서 `pending`과 `approved`로. `DELETE`는 된다.
  - `TestMigrateIsIdempotentAndConcurrent`(기존)가 새 스키마로 통과한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/ -run 'Held|Mark|Migrate'` → FAIL.
- [ ] **Step 3: 구현.** `schema.sql`에 스펙 4장의 `ALTER TABLE`들과 `held_writes_approved` 인덱스를 그대로(주석은 한국어, `in_transaction`의 주석은 B3의 뜻으로) 더하고, "트리거를 두지 않는다"는 옛 주석을 고친다. 트리거 함수는 `to_jsonb(NEW)`와 `to_jsonb(OLD)`에서 B4의 아홉 컬럼을 뺀 것이 같은지 비교해 내용을 고정한다. `in_transaction`은 `unconfirmed→pending`인 그 `UPDATE`에서만 바뀔 수 있다. `status`가 같으면 통과다. 메시지: `stet.held_writes: what was held cannot change`, `stet.held_writes: in_transaction is set once, when the row becomes pending`, `stet.held_writes: status cannot go from <old> to <new>`. 트리거는 `stet.decisions`처럼 `DROP TRIGGER IF EXISTS` 뒤에 만든다. `InsertHeldWrite`는 `status`를 적지 않는다(컬럼 기본값). 두 `Mark*`는 `WHERE status = 'unconfirmed'`다.
- [ ] **Step 4:** `internal/gate/enforce_test.go`의 `TestEnforceHoldsWhatTheModelFlags`에서 `h.Status != "pending"`을 `"unconfirmed"`로 바꾼다(Task 5가 `pending`을 기다리도록 되돌린다. 그 사이의 줄은 승인할 수 없는 쪽으로 틀린다). `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`.
- [ ] **Step 5:** 커밋 `Store: held writes start unconfirmed, keep what was held, and move only along the spec's arrows`.

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P2) `stet.held_writes`의 DDL은 스키마가 이미 맞으면 잠금을 잡지 않게 쓴다. `Migrate`는 Stet이 시작할 때마다 돌고, `ALTER TABLE`과 `DROP TRIGGER`는 아무것도 바꾸지 않아도 `AccessExclusiveLock`을 잡는다. 실행부의 트랜잭션이 줄을 잠그고 있는 동안 다른 Stet이 시작하면 `lock_timeout`(200ms)으로 시작에 실패한다. 그래서 컬럼 추가와 기본값 변경은 빠진 것이 있을 때만 도는 `DO` 블록 안에 두고(`information_schema.columns`나 `pg_attribute`, `pg_attrdef`로 확인), 트리거는 없을 때만 만든다(`pg_trigger`로 확인. 트리거 함수는 `CREATE OR REPLACE FUNCTION`이 본문을 새로 한다). 인덱스는 `CREATE INDEX IF NOT EXISTS` 그대로다. 테스트 하나를 더한다: 다른 연결이 `stet.held_writes`의 한 줄을 `FOR UPDATE`로 잠근 채로 있어도 `Migrate`가 `lock_timeout` 200ms인 풀에서 성공한다(`TestMigrateDoesNotWaitForAHeldRowLock`).
- (P7) 트리거의 두 확인은 따로다. 상태 화살표 확인은 `status`가 같으면 통과다. `in_transaction` 확인은 그와 별개이고, 값이 바뀌는 것은 `unconfirmed`에서 `pending`으로 가는 그 갱신에서만 허락한다.

---

### Task 3: 저장소 2 — 뷰, 승인·거부 함수, 승인된 줄 집기

**Files:** Modify `internal/store/schema.sql`. Create `internal/store/approved.go`, `internal/store/approved_test.go`, `internal/store/review_test.go`.

**Interfaces — Consumes:** Task 2의 `HeldWrite`, `InsertHeldWrite`, `MarkDelivered`.

**Interfaces — Produces:**

```sql
stet.review                                                                      -- 스펙 5장의 뷰 그대로
stet.approve(id uuid, note text DEFAULT NULL, max_rows bigint DEFAULT NULL) RETURNS text  -- 'approved'
stet.reject(id uuid, note text DEFAULT NULL) RETURNS text                                 -- 'rejected'
```

```go
// Approved names an approved held write waiting to be applied.
type Approved struct {
	ID       string
	Snapshot bool // it has a row version or row_absent: its transaction is REPEATABLE READ
}
// ApprovedWrites lists approved held writes, oldest approval first (resolved_at, id), at most limit.
func (s *Store) ApprovedWrites(ctx context.Context, limit int) ([]Approved, error)

// Querier is a connection or transaction of Stet's own account
// (pgx.Tx, *pgx.Conn and *pgxpool.Pool satisfy it).
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Claimed is an approved held write, locked by the transaction that applies it.
type Claimed struct {
	ID, LoginRole, Database, Statement, TargetTable string
	Params               []*string
	ParamTypes           []uint32
	TargetPK, RowVersion *string
	RowAbsent            bool
	Settings             map[string]string
	Limit                *int64 // the rows the approval allows: max_rows, else planned_rows; nil when neither is set
	Lapsed               bool   // approved more than 10 minutes before clock_timestamp(), or resolved_at is NULL (B9)
}
// ClaimApproved locks and reads held write id when it is approved and no other
// transaction holds it (FOR UPDATE SKIP LOCKED). nil, nil when there is none to take.
func ClaimApproved(ctx context.Context, q Querier, id string) (*Claimed, error)
// MarkApplied sets it to applied, with applied_at = now() and rows_affected.
// MarkFailed sets it to failed, with applied_at = now() and error.
// Either returns an error unless exactly one approved row changed.
func MarkApplied(ctx context.Context, q Querier, id string, rows int64) error
func MarkFailed(ctx context.Context, q Querier, id, reason string) error
```

`ST005`의 문구(확인은 이 순서다. `reject`는 앞의 세 줄만 본다):

| 경우 | 메시지 |
|---|---|
| 줄이 없다 | `held write <id> does not exist` |
| `unconfirmed` | `held write <id> is unconfirmed: it is not confirmed that the agent was told of it` |
| 그 밖에 `pending`이 아니다 | `held write <id> is <status>, not pending` |
| `expires_at`이 없거나 `<= clock_timestamp()` (B9) | `held write <id> has expired` |
| `session_user = login_role` | `<session_user> sent this write and cannot approve it` |
| 이유에 `concurrent_change` | `held write <id> was held because its row kept changing while it was judged (concurrent_change): its stored state is stale and it would not run; the agent must send it again` |
| `max_rows < 0` | `max_rows must not be negative` |
| `max_rows`가 없고 정확한 `planned_rows`도 없다(B5) | `held write <id> was not measured exactly (planned_rows: <n 또는 unknown>): pass max_rows, the most rows it may change` |

- [ ] **Step 1: 실패하는 테스트.** 헬퍼 `reviewer(t)`: superuser로 로그인 역할을 만들어 스펙 6장의 `GRANT` 세 줄을 주고, 그 역할로 서버에 직접 붙은 연결과 이름을 돌려준다. 끝날 때 `DROP OWNED BY`, `DROP ROLE`.
  - `TestReviewShowsAHeldWriteWithItsDecision`: 판단 기록(`sample`)과 그 `decision_id`의 `pending` 줄 → 뷰의 한 줄에 `agent_id`, `on_behalf_of`, `state`, `answers`, `impact`, `cumulative`, `proposed`와 줄의 `statement`, `params`, `reasons`, `planned_rows`, `in_transaction`이 함께 있다. 판단 기록이 없는 줄도 나오고 그 칸들은 NULL이다. `expires_at`이 지난 `pending`은 `status = 'expired'`이고, 지나지 않은 것은 `pending`이다.
  - `TestApproveMarksTheRow`: 검토자가 `select stet.approve($1, 'ok', 5)` → `'approved'`. 줄은 `approved`, `resolved_by` = 검토자의 계정, `resolved_at` ≈ 지금, `note = 'ok'`, `max_rows = 5`. 인자 하나로 부르면 `note`와 `max_rows`는 NULL이다.
  - `TestRejectMarksTheRow`: `'rejected'`, `resolved_by`, `note`. 만료된 줄도, 그 줄의 `login_role` 자신도 거부할 수 있다.
  - `TestApproveAndRejectRefusals`: 위 표의 줄마다 SQLSTATE `ST005`이고 메시지에 표의 문구가 있다. 상태는 `unconfirmed`, `undelivered`, `approved`, `rejected`, `applied`, `failed` 모두 본다. 거절된 줄은 바뀌지 않는다. `rows_exact = false`, `planned_rows = 101`인 줄은 메시지에 `101`이 있고, `max_rows`를 주면 승인된다. `max_rows = 0`은 유효하다.
  - `TestOnlyGrantedRolesReview`: 권한을 받지 않은 로그인 역할은 `select * from stet.review`와 `select stet.approve(...)` 모두 `42501`이다. `stet` 스키마의 `USAGE`만 받은 역할도 두 함수가 `42501`이다(PUBLIC에 없다). 검토자는 `stet.held_writes`와 `stet.decisions`를 직접 읽지 못한다(`42501`). `Migrate`를 다시 돌린 뒤에도 검토자는 승인할 수 있고, `USAGE`만 받은 역할은 여전히 `42501`이다.
  - `TestConcurrentApproveAndRejectLetOneWin` (Review Focus 3): 한 연결이 트랜잭션 안에서 승인하고 커밋하지 않은 동안 다른 연결의 `stet.reject`는 기다린다. 커밋하면 `ST005`(`is approved, not pending`)를 받는다. 같은 줄을 다시 승인해도 같다.
  - `TestRowsHeldBeforeThisStageCannotBeApproved` (Review Focus 2): 새 컬럼이 모두 NULL인 `pending` 줄을 SQL로 넣는다 → 뷰에 `expired`, 승인은 `ST005`(`has expired`), 거부는 된다.
  - `TestApprovedWrites`: 승인 순서대로 나온다. `row_version`이 있는 줄과 `row_absent`인 줄은 `Snapshot`이 참, `INSERT`의 줄은 거짓이다. `pending`·`applied`인 줄은 나오지 않는다. `limit`를 지킨다.
  - `TestClaimApproved`: 트랜잭션 안에서 집으면 필드가 저장한 그대로다(`Params`의 NULL, `ParamTypes`, `Settings`). `Limit`는 `max_rows`를 준 승인이면 그 값, 아니면 `planned_rows`다. 그 트랜잭션이 열린 동안 다른 트랜잭션의 `ClaimApproved`는 `nil, nil`이다. `pending`인 줄도 `nil, nil`이다. `resolved_at`이 11분 전인 줄(SQL로 `approved`인 채 넣는다)은 `Lapsed`, 9분 전은 아니다.
  - `TestMarkAppliedAndFailed`: `applied`와 `rows_affected`, `applied_at`. `failed`와 `error`, `applied_at`. `approved`가 아닌 줄에는 에러다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/ -run 'Review|Approve|Reject|Claim|Mark|Roles|Rows'` → FAIL.
- [ ] **Step 3: 구현.** 뷰는 스펙 5장의 SQL 그대로(`CREATE OR REPLACE VIEW`). 두 함수는 `LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp`이고 이름을 `stet.`까지 적는다. 줄을 `FOR UPDATE`로 잠근 뒤 표의 순서로 확인하고, `status`, `resolved_by`(`session_user`), `resolved_at`(`clock_timestamp()`, B9), `note`, `max_rows`만 바꾼다. 함수 정의 바로 뒤에 `REVOKE EXECUTE ON FUNCTION stet.approve(uuid, text, bigint), stet.reject(uuid, text) FROM PUBLIC;`을 둔다(`Migrate`가 한 트랜잭션이므로 PUBLIC이 부를 수 있는 틈이 없고, `CREATE OR REPLACE`는 이미 준 권한을 지우지 않는다). `Lapsed`는 SQL에서 `resolved_at IS NULL OR resolved_at < clock_timestamp() - interval '10 minutes'`로 낸다(DB의 시계만 쓴다).
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`.
- [ ] **Step 5:** 커밋 `Store: the review view, approve and reject, and claiming approved held writes`.

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P1) `TestApprovedWrites`는 순서와 `limit`을 자기가 넣은 id들 안에서만 확인한다. 공유 데이터베이스 `stet_test`에는 다른 테스트의 줄이 있을 수 있다.
- (P3) 검토자 역할을 만드는 테스트 헬퍼는 `GRANT`와, 나중의 `DROP OWNED BY`를 `Migrate`가 잡는 것과 같은 advisory lock(`pg_advisory_xact_lock`, 같은 키)을 먼저 잡은 트랜잭션에서 한다. 다른 패키지의 `Migrate`가 같은 때에 두 함수와 뷰의 카탈로그 줄을 고쳐 쓰면 `tuple concurrently updated`로 실패하기 때문이다.
- (P5, B9에 더함) `MarkApplied`와 `MarkFailed`는 `applied_at`에 `clock_timestamp()`를 쓴다. 스펙 4장의 뜻은 실행을 끝낸 시각이다.

---

### Task 4: 프록시 — 세션 설정과 `SET` 깃발

**Files:** Modify `internal/sqlparse/classify.go`, `internal/proxy/session.go`, `internal/proxy/settings.go`, `internal/gate/gate.go`(`Inspect` 한 줄). Test `internal/sqlparse/classify_test.go`, `internal/proxy/settings_internal_test.go`, `internal/proxy/gate_test.go`, `internal/gate/gate_test.go`.

**Interfaces — Produces:**

```go
// sqlparse.Result:
Sets bool // some statement is SET or RESET (any VariableSetStmt)

// proxy.Statement:
Sets bool // the message holds a SET or RESET: what the server reports of the session's settings may change at its ReadyForQuery

// proxy.SessionInfo:
// TimeZone, DateStyle and IntervalStyle are what the server last reported for
// the session; "" for one it has not reported. Followed on enforced sessions only.
TimeZone, DateStyle, IntervalStyle string
// SetPending means a SET or RESET was sent that no ReadyForQuery has answered
// yet, before the statement or in its own message: the three settings may
// not be the ones the statement runs under.
SetPending bool
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestClassifySets`(sqlparse): 참 `set timezone = 'UTC'`, `set local datestyle to 'SQL, DMY'`, `set time zone 'UTC'`, `reset timezone`, `reset all`, `begin; set local a.b = '1'`. 거짓 `show timezone`, `begin`, `select set_config('timezone', 'UTC', false)`, `update contacts set title = 'x' where id = 1`.
  - `TestInspectMarksSets`(gate): `Inspect("set timezone = 'UTC'").Sets`는 참, `Inspect("select 1").Sets`는 거짓.
  - `TestSessionSettingsAreFollowed`(proxy 내부, enforce 세션): 시작할 때 서버가 `TimeZone=Asia/Seoul`, `DateStyle=ISO, MDY`, `IntervalStyle=postgres`를 알리면 그 뒤 쓰기의 `Begin`이 그 셋을 받고 `SetPending`은 거짓이다. `set timezone`의 답으로 `TimeZone=UTC`가 오면 다음 쓰기는 `UTC`를 받는다. 서버가 알린 적 없는 것은 `""`다.
  - `TestSetPendingUntilReadyForQuery`(proxy 내부): 쓰기의 `Begin`이 받는 `SetPending`.

| 순서 | SetPending |
|---|---|
| 한 Sync 아래 Parse·Bind·Execute(`SET`) 뒤 Parse·Bind·Execute(쓰기) | true |
| Query(`SET`)에 답이 오기 전의 Query(쓰기) (B2) | true |
| Query(`SET`)의 ReadyForQuery가 온 뒤의 Query(쓰기) | false |
| Parse·Bind·Execute(`SET`)가 모두 완료된 뒤 Sync 없이 온 Query(쓰기) (unsynced, B2) | true. 그 Query의 ReadyForQuery 뒤의 쓰기는 false |
| `SET`이 없는 세션의 쓰기 | false |

  - `TestShadowSessionsFollowNoSettings`(proxy 내부): enforce가 아닌 세션의 서버 탭은 ParameterStatus를 보지 않고(`want('S')`가 거짓), 그 세션의 `Begin`은 세 설정을 `""`로 받는다(규칙 6: 섀도의 경로는 그대로다).
  - `TestGateSeesSessionSettings`(`gate_test.go`, 실제 서버): enforce 정책의 세션에서 `set timezone = 'UTC'`를 따로 보낸 뒤의 쓰기는 `TimeZone == "UTC"`, `IntervalStyle == "postgres"`, `DateStyle != ""`, `SetPending == false`다. 파이프라인으로 한 Sync 아래 `SET`과 쓰기를 보내면 `SetPending == true`다. `recordingGate.Inspect`가 `set`·`reset`으로 시작하는 SQL에 `Sets`와 `Clean`을 세우게 한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/sqlparse/ ./internal/proxy/ ./internal/gate/ -run 'Sets|Settings|SetPending'` → FAIL (컴파일).
- [ ] **Step 3: 구현.** `Result.Sets`는 최상위 문장 가운데 `VariableSetStmt`가 있으면 참이다. `Gate.Inspect`는 `st.Sets = r.Sets`만 더한다. 프록시: `session.parameter`가 세 이름을 `s.mu` 아래의 세션 필드에 적고, `identity()`가 `SessionInfo`에 옮긴다. `unit.sets`는 `dirties`가 불리는 자리에서 `st.Sets`로 세우고, Execute가 실행할 수 있는 다른 뜻(`anyOther`) 가운데 `Sets`가 있어도 세운다. `session.setsCarried`는 `answeredUnsynced`가 `sets`인 단위를 내보낼 때 세우고 `readyForQuery`에서 지운다. `identity()`의 `SetPending`은 `setsCarried`이거나 기다리는 단위 가운데 `sets`가 있으면 참이고, `begin`이 쓰기 자신의 `st.Sets`를 더한다. 세션 잠금을 쥔 채 게이트를 부르지 않는 것과 행 메시지마다 하는 일은 그대로다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run LatencyBudget -count=1 -v`의 다섯 테스트가 PASS이고, 찍힌 p95를 보고에 적는다.
- [ ] **Step 5:** 커밋 `Proxy: follow TimeZone, DateStyle and IntervalStyle on enforced sessions, and tell the gate of a SET not yet answered`.

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P6) 테스트용 `recordingGate`는 `set`이나 `reset`으로 시작하는 SQL에 `Sets`만 세운다. `Clean`은 지금의 `cleanRE`가 정하는 대로 둔다. 실제 분류기와 B2에서 `RESET`은 깨끗한 문장이 아니다.

---

### Task 5: 게이트 — 보류 줄의 내용과 전달 확인

**Files:** Modify `internal/gate/enforce.go`(`hold`, `unstorable`), `internal/gate/gate.go`(`judge`, `paramTexts`, `Gate`의 필드), `internal/gate/export_test.go`. Test `internal/gate/enforce_test.go`.

**Interfaces — Consumes:** `config.Held.ExpireAfter`(Task 1). `store.HeldWrite`의 새 필드, `(*store.Store).MarkDelivered(ctx, id, inTransaction)`, `MarkUndelivered(ctx, id)`(Task 2). `proxy.SessionInfo`의 `TimeZone`, `DateStyle`, `IntervalStyle`, `SetPending`(Task 4). `proxy.Outcome`의 `Refused`, `Undelivered`, `Executed`, `Lost`, `InTransaction`(기존).

**Interfaces — Produces:** 내보내는 이름은 없다. `stet.held_writes`의 줄이 아래 값으로 들어가고 상태가 스펙 4장의 표대로 바뀐다. 테스트용: `Gate.markDelivered func(ctx context.Context, id string, inTransaction bool) error`(기본 `st.MarkDelivered`)와 `export_test.go`의 `FailDelivered(g *Gate, n int)`, `OnDelivered(g *Gate, f func(id string))`.

- [ ] **Step 1: 실패하는 테스트** (`heldRow`와 `e.held`에 새 컬럼을 더한다).
  - `TestEnforceHoldsWhatTheModelFlags`: Task 2에서 바꾼 단언을 `e.waitHeldStatus(t, "pending")`으로 되돌린다.
  - `TestHeldWriteCarriesWhatApprovalNeeds`: 단일 행 `UPDATE`를 보류한다 → `pending`이 되고, `in_transaction = false`, `expires_at = created_at + 24h`, `settings`의 세 값이 그 에이전트 연결의 `show timezone`·`show datestyle`·`show intervalstyle`과 같고, `planned_rows = 1`, `rows_exact = true`, `row_absent = false`, `row_version`이 있다. `held: {expire_after: 1h}`를 붙인 설정에서는 `created_at + 1h`다.
  - `TestHeldWriteOfARowThatIsNotThere`: 없는 id의 `UPDATE`를 보류한다 → `row_absent = true`, `row_version IS NULL`.
  - `TestHeldWritePlannedRows`:

| 보류된 쓰기 | planned_rows | rows_exact |
|---|---|---|
| 단일 행 `INSERT` | 1 | true |
| `max_rows`를 넘는 여러 행 `INSERT ... VALUES`(방법 `syntax`) | 그 행 수 | true |
| 상한 아래로 센 `UPDATE`가 누적 한도로 보류(`impact:cumulative`) | 센 값 | true |
| `max_rows`를 넘어 상한에 닿은 `UPDATE`(`impact:rows`) | `max_rows + 1` | false |
| 추정으로 잰 쓰기(`impact:uncertain`) | 추정값 | false |
| 자리가 없어 재지 못한 다중 행 쓰기(`gate.HoldInflight`) | NULL | false |

  - `TestHeldWriteBehindASetUnderOneSyncIsNotStored`: 파이프라인으로 한 Sync 아래 `set timezone = 'UTC'`와 쓰기를 보낸다 → `ST001`, `DETAIL`에 `held_write`가 없고 `HINT`에 `this write`가 있다. 줄이 없고, 에러 로그 `held write not stored`가 한 줄이다. 행은 그대로다.
  - `TestHeldWriteWithoutReportedSettingsIsNotStored`: `TimeZone`이 빈 `SessionInfo`로 `Begin`을 직접 부른다 → 막히고 줄이 없다.
  - `TestFirstWriteAfterBeginIsHeldInTransaction`: `BEGIN` 뒤 첫 쓰기 → `ST001`, 줄은 `pending`이고 `in_transaction = true`다.
  - `TestHeldWriteTheProxyReportsAsSentIsUndelivered`: 세 설정을 채운 `SessionInfo`로 `Begin`한 뒤 `End`에 `{Refused: true, Executed: true}`와 `{Refused: true, Lost: true}`를 준다 → 두 줄 모두 `undelivered`다. 기존 `TestEndRecordsARefusedWriteTheProxyReportsAsSent`의 단언은 그대로 통과한다.
  - `TestHeldWriteStaysUnconfirmedWhenItCannotBeMarked`: `gate.FailDelivered(e.gate, 1)` → 줄은 `unconfirmed`로 남고 에러 로그 `held write not marked pending`이 한 줄이다.
  - `TestPendingComesAfterTheDecisionIsRecorded`: `gate.OnDelivered`의 함수가 불린 때 그 줄의 `decision_id`가 이미 `stet.decisions`에 있다.
  - `TestHeldWriteKeepsTextValuesAsSentAmongBinaryOnes` (Review Focus 1, B1): `ExecParams`로 `update <t> set company_id = $1, title = $2 where id = $3`에 값 `[" 07"(텍스트), NULL, 2(바이너리 int4)]`, 타입 `[0, 0, 23]`을 보낸다 → `params = [" 07", null, "2"]`, `param_types = {0,0,23}`. 기존 `TestHeldWriteKeepsBinaryAndNullParamsAsText`는 그대로 통과한다.
  - 기존 `TestEnforceMarksAHiddenHoldUndelivered`, `TestEnforceMarksAnUnconfirmedHeldWriteUndelivered`가 고치지 않고 통과한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/ -run 'Held|Pending|FirstWrite'` → FAIL.
- [ ] **Step 3: 구현.**
  - `hold`: `ExpiresAt = p.at + cfg.Held.ExpireAfter`. `Settings`는 `p.session`의 세 값. `PlannedRows`·`RowsExact`는 `p.res.Kind == sqlparse.Gated`면 `1, true`, `p.impact`에 `Method`와 `Rows`가 있으면 `*Rows`와 `Method == "syntax" || (Method == "count" && !Capped)`, 그 밖에는 `nil, false`다. `RowAbsent`는 `row_version`을 적는 모양(단일 행 `UPDATE`·`DELETE`)이고 `p.pk != nil`, `p.state != nil`, `p.stateErr == nil`인데 `p.stateVersion`에 `xmin`이 없을 때다.
  - `unstorable`: `p.session.SetPending`이거나 세 설정 가운데 빈 것이 있으면 저장하지 않는다(지금의 "저장하지 못한 보류 건" 경로 그대로: 막고, `held_write` 없이, 에러 로그).
  - `judge`: 상태 단계를 `g.store.Insert` 뒤로 옮긴다(삽입이 실패해도 한다). `p.heldUnsure`는 `MarkUndelivered`. `p.heldID`는 `o.Refused && !o.Undelivered && !o.Executed && !o.Lost`일 때 `markDelivered(id, o.InTransaction)`(B3), 아니면 `MarkUndelivered`. `markDelivered`가 실패하면 에러 로그 `held write not marked pending`(id, decision)만 남긴다.
  - `paramTexts`(B1): 바이너리 값이 있어 서버에서 풀어 읽은 뒤, `Format == 0`인 값은 결과 대신 온 바이트를 쓴다(NULL은 NULL).
  - 손으로 만든 `SessionInfo`로 보류 줄을 기대하는 기존 테스트가 있으면 세 설정을 채운다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`.
- [ ] **Step 5:** 커밋 `Gate: store what an approval needs with a held write, and make it pending only once its refusal was delivered`.

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P8) `settings`의 키는 서버가 알리는 ParameterStatus 이름 그대로 `TimeZone`, `DateStyle`, `IntervalStyle`이다. Task 2와 Task 6의 테스트와 실행부가 이 이름으로 읽는다.

---

### Task 6: 실행부 — 한 줄의 처리

**Files:** Create `internal/apply/apply.go`, `internal/apply/searchpath.go`, `internal/apply/export_test.go`, `internal/apply/apply_test.go`. Modify `cmd/stet/check.go`(`schemaNames`를 옮기고 `apply.SchemaNames`를 부른다).

**Interfaces — Consumes:** `store.Approved`, `store.Claimed`, `store.ClaimApproved`, `store.MarkApplied`, `store.MarkFailed`, `(*store.Store).InsertHeldWrite`·`MarkDelivered`, SQL `stet.approve`(Task 2·3). `config.Config`의 `Tables`(키와 `PK`)와 `Held.ApplyTimeout`(Task 1).

**Interfaces — Produces:**

```go
package apply

// Applier runs approved held writes (spec-v0.2-approve 7).
type Applier struct{ /* cfg, pool, store, log, test seams */ }

func New(cfg *config.Config, pool *pgxpool.Pool, st *store.Store, log *slog.Logger) *Applier

// Apply processes one approved held write on a connection of its own, in one
// transaction, and closes the connection. It returns nil when the write was
// applied, was recorded as failed, or was not there to take; an error when
// one of Stet's own steps failed: the transaction is discarded and the row
// stays approved. It runs under a bound of its own, held.apply_timeout +
// applyGrace, whatever the caller's context.
func (a *Applier) Apply(w store.Approved) error

// SchemaNames splits a search_path value into the names Postgres reads from it (moved from cmd/stet).
func SchemaNames(path string) []string

var applyGrace = 5 * time.Second // a var so tests can shorten it
```

`export_test.go`: `FailMarks(a *Applier, n int)`(다음 n번의 `MarkApplied`가 실패한다), `BeforeStatement(a *Applier, f func())`(실행 전 확인을 통과한 뒤, 저장점 앞에서 불린다), `SetGrace(d time.Duration) (restore func())`.

- [ ] **Step 1: 실패하는 테스트.** 헬퍼 `setup(t)`: `stet_scram`의 풀(`cmd/stet`의 `openPool`처럼 `lock_timeout = 200ms`를 준다)과 `Migrate`, 테스트 테이블(`id int primary key, name text, at timestamptz, owner text, n int`), `grant stet_md5 to stet_scram`, 그 테이블을 게이트 대상으로 둔 설정. `approved(t, h, maxRows)`: `InsertHeldWrite` → `MarkDelivered` → Stet의 풀에서 `select stet.approve(...)`. 끝날 때 그 테이블의 보류 줄을 지운다. 아래에서 "줄"은 `stet.held_writes`의 줄이다.
  - `TestApplyRunsAnApprovedUpdate`: 값 없는 문장(`params` NULL)과 `RETURNING *`이 붙은 값 있는 문장 → 줄은 `applied`, `rows_affected = 1`, `applied_at`이 있고 `error`는 NULL이며 테이블에 값이 들어갔다. 그 테이블의 `stet.decisions` 줄 수는 앞뒤가 같다(규칙 3). info 로그 `held write applied`에 id, `login_role`, `table`, `rows`가 있다.
  - `TestApplySendsValuesAsStored`: `params ["CTO", null, "2"]`를 `param_types {25,0,0}`으로도, `param_types` NULL로도 → 같은 값이 들어가고 NULL은 NULL이다.
  - `TestApplyRunsWithTheAgentsRights`: `stet_md5`에 `UPDATE` 권한이 없는 테이블의 줄 → `failed`, `error`가 `SQLSTATE 42501: `으로 시작하고 테이블은 그대로다. 행 수준 보안(`using (owner = current_user)`)이 켜진 테이블에 `update ... set name = 'x'`(`max_rows` 10) → `owner = 'stet_md5'`인 행만 바뀌고 `rows_affected`가 그 수다.
  - `TestApplyFailsWhenStetCannotBecomeTheRole` (Review Focus 4, B6): `login_role = 'stet_trust'`인 줄 → `failed`, `SQLSTATE 42501: `. 이어서 `stet_md5`의 다른 줄은 `applied`다. warn 로그 `held write failed`가 있다.
  - `TestApplyReadsValuesUnderTheAgentsSettings`: 타입 없는 값 `'01/02/2026 10:00'`을 `timestamptz` 컬럼에 넣는 두 줄. `settings`가 `{TimeZone: UTC, DateStyle: "ISO, DMY", IntervalStyle: postgres}`면 `2026-02-01 10:00:00+00`, `TimeZone: Asia/Seoul`이면 `2026-02-01 01:00:00+00`이 들어간다.
  - `TestApplyChecksTheRowVersion`: (a) 보류 뒤 행이 바뀌었다 → `failed`(`row_changed`), 행은 바꾼 쪽의 값 그대로다. (b) `BeforeStatement`에서 다른 연결이 그 행을 바꾸고 커밋한다 → `failed`(`row_changed`), 같다. (c) 행이 지워졌다 → `applied`, `rows_affected = 0`. (d) `row_absent`인 줄인데 지금은 행이 있다 → `failed`(`row_changed`). (e) `row_absent`이고 여전히 없다 → `applied`, 0행.
  - `TestApplyRefusesAnotherSearchPath`: 테스트가 만든 역할(Stet에 부여, `ALTER ROLE ... SET search_path = other_schema`)의 줄 → `failed`(`search_path_differs`), 테이블은 그대로다. `IN DATABASE`로 준 설정도 같다.
  - `TestApplyKeepsToTheApprovedRows`: `planned_rows = 2`, `rows_exact = true`인데 문장이 3행을 건드린다 → `failed`, `error = 'rows_exceeded: 3 rows, approved up to 2'`, 테이블은 그대로다. 같은 문장을 `max_rows` 5로 승인하면 `applied`, 3행이다. 상한이 없는 `approved` 줄(SQL로 넣는다)은 `failed`(`no_row_limit`)이고 문장은 실행되지 않는다(B5).
  - `TestApplyUndoesAWriteThatChangesRole`: 문장 `update <t> set name = set_config('role', '<other>', false) where id = 1`(`<other>`는 테스트가 만들어 Stet에 부여한 역할) → `failed`(`role_changed`), 테이블은 그대로다. 이어서 다른 줄은 `applied`다.
  - `TestApplyRecordsTheServersError`: 유일성 위반 → `failed`, `error`가 `SQLSTATE 23505: `으로 시작한다. `held.apply_timeout: 1s`에서 `pg_sleep(3)`이 든 문장 → `SQLSTATE 57014: `. 다른 연결이 그 행을 잠그고 있으면 → `SQLSTATE 55P03: `. 셋 다 테이블은 그대로이고, 다시 `Apply`해도 실행되지 않는다(`failed`는 끝이다).
  - `TestApplyLeavesOtherStatusesAlone`: `rejected`, 만료된 `pending`, `pending`, `undelivered`, `unconfirmed`인 줄에 `Apply` → 아무것도 바뀌지 않고 nil이다. `resolved_at`이 11분 전인 `approved` 줄 → `failed`(`approval_lapsed`), 테이블은 그대로다.
  - `TestApplyChecksTheDatabaseAndTheTable`: `database`가 다른 줄 → `other_database`. `target_table`이 설정에 없는 줄 → `not_gated`.
  - `TestTwoAppliersRunAWriteOnce`: 문장 `update <t> set n = n + 1 where id = 1`. 첫 실행부를 `BeforeStatement`에 세워 둔 동안 둘째 실행부의 `Apply`는 nil로 바로 돌아온다. 풀어 준 뒤 셋째 `Apply`도 nil이다. `n = 1`, 줄은 `applied`다.
  - `TestApplyLeavesTheRowApprovedWhenItsOwnStepFails`: `FailMarks(a, 1)` → `Apply`가 에러를 돌려주고 줄은 `approved`, 테이블은 그대로다. 다음 `Apply`는 `applied`이고 쓰기는 한 번만 반영됐다. 다른 연결이 테이블에 `ACCESS EXCLUSIVE` 잠금을 쥔 동안의 `Apply`(행 버전 읽기가 Stet 연결의 `lock_timeout`에 걸린다)도 에러이고 줄은 `approved`다.
  - `TestApplyQuotesRoleAndTableNames` (Review Focus 5): 대문자가 섞인 역할 이름(테스트가 만들어 Stet에 부여)과 설정 키가 `<schema>.<table>`인 테이블의 줄 → `applied`.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/apply/` → FAIL (패키지 없음).
- [ ] **Step 3: 구현** (스펙 7장의 순서. 정한 것만 적는다).
  1. 연결은 `pgx.ConnectConfig(ctx, pool.Config().ConnConfig.Copy())`로 연다. Stet의 계정이고 풀과 같은 시작 설정이다. `Apply`가 끝나면 어느 길로든 닫는다.
  2. `w.Snapshot`이면 `BEGIN ISOLATION LEVEL REPEATABLE READ`, 아니면 `READ COMMITTED`. `store.ClaimApproved`가 nil이면 그만둔다. 그 질의의 `40001`도 그만두는 것이다(B7, debug 로그).
  3. 실행 전 확인은 이 순서다: `Lapsed` → `approval_lapsed`. `current_database()`와 다르면 `other_database`. `cfg.Tables`에 `TargetTable`이 없으면 `not_gated`. `Limit == nil`이면 `no_row_limit`. `RowVersion`이나 `RowAbsent`가 있으면 게이트의 `readRowVersion`과 같은 질의(`select t.xmin::text from <테이블> as t where t.<pk> = $1`, 값은 텍스트로 `PgConn().ExecParams`)로 읽어 비교한다 → `row_changed`. 그 계정의 역할 수준 `search_path`(이 데이터베이스용이 전체용보다 먼저다)가 있고 `SchemaNames`로 본 것이 이 연결의 `current_setting('search_path')`와 다르면 `search_path_differs`. 걸리면 `store.MarkFailed` 뒤 커밋한다.
  4. `SAVEPOINT`, `SET LOCAL ROLE <인용한 login_role>`, 세 설정은 `set_config($1, $2, true)`, `statement_timeout`과 `lock_timeout`은 `Held.ApplyTimeout`(ms).
  5. 문장은 `PgConn().ExecParams(ctx, statement, values, paramTypes, nil, nil)`로 보내고 `Close()`로 결과 행을 버리며 완료 태그의 행 수를 얻는다.
  6. 4·5단계가 서버 에러(`*pgconn.PgError`)로 끝나면 저장점까지 되돌리고 `failed`: `40001`이면 `row_changed`, 아니면 `SQLSTATE <코드>: <메시지>`. 성공했으면 `select current_user`가 `LoginRole`과 다를 때 되돌리고 `role_changed`, 행 수가 `*Limit`보다 크면 되돌리고 `rows_exceeded: <n> rows, approved up to <m>`.
  7. `RESET ROLE`, `store.MarkApplied`, 커밋.
  - Stet 자신의 단계(연결, 집기, 확인의 질의, `Mark*`, 커밋)가 실패하거나 서버 에러가 아닌 실패(연결 끊김, 시간 초과)가 나면 에러를 돌려준다. 트랜잭션은 연결을 닫으면서 버려진다.
  - 로그: 실행하면 info `held write applied`, `failed`로 적으면 warn `held write failed`. 둘 다 `id`, `login_role`, `table`과 `rows`나 `error`를 담는다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`(`cmd/stet`의 `search_path` 경고 테스트 포함).
- [ ] **Step 5:** 커밋 `Apply: run one approved held write in one transaction, as its role and only as far as it was approved`.

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P1) `internal/apply`의 테스트는 공유 데이터베이스 `stet_test`가 아니라 테스트가 만들고 지우는 자기 데이터베이스에서 돈다(`cmd/stet`의 `cleanDatabase`와 같은 방식. 헬퍼는 이 패키지에 새로 둔다). 실행부는 그 데이터베이스의 `approved` 줄을 모두 집으므로, 공유 데이터베이스에서 돌면 다른 패키지의 줄을 `not_gated`로 끝내 버린다.
- (P9) 역할 수준 `search_path`를 읽는 질의는 한 곳에만 둔다. `internal/apply`가 읽는 함수 하나를 내보내고(예: `RoleSearchPath`), `cmd/stet/check.go`의 `searchPath`도 그것을 부르게 고친다. 질의와 우선순위(이 데이터베이스용 설정이 전체용보다 먼저)가 두 벌이 되지 않게 한다. `ident`와 행 버전을 읽는 짧은 질의를 이 패키지에 한 벌 더 두는 것은 받아들인다(게이트의 것은 내보내지 않은 메서드다).

---

### Task 7: 실행부의 루프와 `cmd/stet` 배선

**Files:** Create `internal/apply/loop.go`, `internal/apply/loop_test.go`. Modify `cmd/stet/main.go`(`openGate`, `openPool`, `run`의 주석). Test `cmd/stet/main_test.go`.

**Interfaces — Consumes:** `apply.New`, `(*Applier).Apply(w store.Approved) error`, `apply.SetGrace`, `apply.BeforeStatement`(Task 6). `(*store.Store).ApprovedWrites(ctx, limit)`(Task 3).

**Interfaces — Produces:**

```go
// Start polls for approved held writes every second, in a goroutine, and
// applies them one at a time in the order they were approved.
func (a *Applier) Start()
// Close stops the polling and waits for the write being applied, which ends
// within held.apply_timeout + applyGrace. No write is taken after it.
func (a *Applier) Close()
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestLoopAppliesApprovalsInOrder`(apply): 승인된 세 줄(문장 `update <t> set n = nextval('<seq>') where id = <k>`) → `Start` 뒤 3초 안에 모두 `applied`이고 `n`의 순서가 승인 순서다.
  - `TestLoopPicksUpApprovalsMadeWhileItWasDown`(apply): `Start` 전에 승인된 줄은 실행된다. `resolved_at`이 11분 전인 줄은 `failed`(`approval_lapsed`)다. `pending`인 줄은 그대로다.
  - `TestLoopGoesOnAfterAFailure`(apply): `FailMarks(a, 1)` → 그 줄은 다음 차례에 `applied`가 되고 쓰기는 한 번만 반영된다.
  - `TestCloseWaitsForTheWriteBeingApplied`(apply): `pg_sleep(1)`이 든 문장을 처리하는 중에 `Close` → 돌아온 뒤 줄은 `applied`이고 테이블에 값이 있다. 그 뒤에 승인한 줄은 `approved`로 남는다.
  - `TestCloseAbandonsAWritePastItsBound`(apply): `held.apply_timeout: 1s`에 `SetGrace(-700 * time.Millisecond)`로 상한을 300ms로 두고 `pg_sleep(2)`가 든 문장 → `Close`가 1초 안에 돌아오고, 줄은 `approved`, 테이블은 그대로다(반쯤 반영된 것이 없다).
  - `TestPoolPinsDateStyleAndIntervalStyle`(cmd): `openPool`의 연결에서 `show datestyle`이 `ISO`로 시작하고 `show intervalstyle`이 `postgres`다.
  - `TestApprovedWriteRunsAsTheAgentSentIt`(cmd, `start`로 `run`을 띄운다. Review Focus 1): 설정은 `model: {adapter: none, on_failure: pass}`에 테이블의 `policy` 규칙 하나(질문이 생긴다)를 두고, `parseConfig` 뒤 `cfg.Model.OnFailure = "hold"`로 바꾼다(`Parse`는 이 조합을 받지 않는다). 모델이 없으므로 단일 행 쓰기가 모두 `no_model`로 보류된다. 에이전트(`stet_md5`)가 `set timezone = 'Asia/Seoul'`과 `set datestyle = 'ISO, DMY'`를 따로 보낸 뒤 `ExecParams`로 `update <t> set at = $1, name = $2 where id = $3`(값: 텍스트 `'01/02/2026 10:00'`, NULL, 바이너리 int4 `1`, 타입 `[0, 0, 23]`)을 보낸다 → `ST001`. 테스트가 만든 검토자가 `stet.review`에서 그 줄(`pending`, `in_transaction = false`)을 보고 `stet.approve`를 부른다 → 3초 안에 뷰가 `applied`, `rows_affected = 1`이고 `at = 2026-02-01 01:00:00+00`, `name IS NULL`이다.
  - `TestApprovingTheFirstWriteOfATransactionRunsOnlyIt`(cmd, 같은 설정): 에이전트가 `BEGIN` 뒤 id 1의 쓰기를 보내 `ST001`을 받고, 그 트랜잭션에서 이어 보낸 id 2의 쓰기도 실패한다. 뷰에 줄이 하나이고 `in_transaction = true`다. 승인하면 id 1만 바뀐다.
  - `TestShadowModeRunsNoApprovedWrite`(cmd): `mode: shadow`로 띄우고 `approved`인 줄을 SQL로 넣는다 → 2.5초 뒤에도 `approved`이고 테이블은 그대로다(규칙 6).
  - `TestAppliedRowsDoNotCountTowardTheCumulativeLimit`(cmd): `model: {adapter: none, on_failure: pass}`, `impact: {max_rows: 3, cumulative: {max_rows: 4, window: 10m}}`. 5행 `UPDATE`가 보류되고(`impact:rows`) `max_rows` 5로 승인되어 `applied`가 된다. 그 뒤 같은 계정의 2행 `UPDATE`는 통과하고, 그 판단 기록의 `cumulative.used`는 0이다.
  - `TestRunStopsTheApplierBeforeThePool`(cmd): 승인된 `pg_sleep(1)` 문장이 처리되는 중에 `r.stop` → nil을 돌려주고, 줄은 `applied`, 로그에 ERROR가 없다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/apply/ ./cmd/stet/ -run 'Loop|Close|Pool|Approv|Shadow|Applied|Applier'` → FAIL.
- [ ] **Step 3: 구현.** 루프는 1초 간격(`pollEvery`)으로 `ApprovedWrites(ctx, 100)`을 풀로 읽고, 나온 순서대로 `Apply`한다. 줄 사이마다 멈춤을 확인하고, `Apply`의 에러는 warn 로그로 남기고 다음 줄로 간다(그 줄은 다음 차례에 다시 나온다). `openGate`: `cfg.Mode == config.ModeEnforce`일 때만 `apply.New(...).Start()`(게이트가 없는 설정에서는 풀도 실행부도 없다). `closeGate`는 `g.Close()`, 실행부의 `Close()`, `pool.Close()` 순서다. `openPool`의 `RuntimeParams`에 `DateStyle = ISO`, `IntervalStyle = postgres`를 더한다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`.
- [ ] **Step 5:** 커밋 `Stet: apply approved held writes in enforce mode, stop the applier before the pool, and pin the pool's DateStyle and IntervalStyle`.

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P1) 이 Task의 `internal/apply` 테스트와, `cmd/stet`에서 줄을 승인하거나 게이트 대상 테이블을 두고 enforce로 `run`을 띄우는 테스트는 모두 자기 데이터베이스에서 돈다. 이미 있는 `TestRunServesInEnforceMode`도 준비 단계만 그렇게 바꾼다(단언은 그대로다). 실행부가 켜진 뒤로는 공유 데이터베이스에서 enforce로 도는 Stet이 다른 패키지의 `approved` 줄을 집기 때문이다.
- (P10) Consumes에 Task 6의 `export_test.go`가 내보내는 `apply.FailMarks`가 더 있다. `TestLoopGoesOnAfterAFailure`가 쓴다.

---

### Task 8: 시작할 때의 두 경고

**Files:** Modify `cmd/stet/check.go`. Test `cmd/stet/main_test.go`.

**Interfaces — Produces:** `check`가 enforce에서 내는 경고 두 가지. 문구는 아래와 같고, `GRANT`의 두 이름은 `pgx.Identifier`로 인용한다(`gate.warnRole`과 같다).

- `Stet's account "<계정>" is a superuser: an approved write runs on Stet's connection, and a statement that undoes SET LOCAL ROLE runs with a superuser's rights; fix: connect Stet as a role that has only the stet schema and membership in the agent roles`
- `Stet's account "<계정>" is not a member of agent role "<역할>": an approved write of that role ends as failed, and its multi-row writes are not measured; fix: GRANT <역할> TO <계정>`

- [ ] **Step 1: 실패하는 테스트.**
  - `TestCheckWarnsWhenStetIsASuperuser`: `postgres` 계정의 풀로 `check`(enforce) → `is a superuser`와 `SET LOCAL ROLE`이 든 경고가 있다. `stet_scram`의 풀에서는 없다.
  - `TestCheckWarnsWhenStetIsNotAMemberOfAnAgentRole`: `createRole`로 만든 역할을 `agents.roles`에 둔다 → `is not a member of agent role`과 `GRANT "<역할>" TO "stet_scram"`이 든 경고가 있다. `grant <역할> to stet_scram` 뒤에는 없다. `stet_trust`를 목록에 두어도 경고가 나온다(부여하지 않는다). 없는 역할에는 이 경고가 없다(이미 "does not exist"를 경고한다).
  - `TestCheckInShadowSaysNothingOfApprovals`: 섀도에서는 두 경고 모두 없다.
  - 경고가 하나도 없거나 정확한 수이기를 기대하는 기존 테스트는 준비 단계에서 그 에이전트 역할을 `stet_scram`에 부여한다(준비이지 단언 변경이 아니다): `TestCheckWarnsAboutAgentRoles`의 `want == ""`인 경우(`stet_clear`, `stet_md5`), `TestCheckWarnsAboutPassSettings`(`stet_md5`), `TestCheckWarnsAboutRightsToCreateObjects`가 만든 역할. `stet_clear`의 부여는 테스트가 끝날 때 거둔다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./cmd/stet/ -run Check` → FAIL.
- [ ] **Step 3: 구현.** superuser는 `select rolsuper from pg_roles where rolname = session_user`로 한 번 읽는다. 구성원인지는 `agentRole`이 이미 읽은 역할의 oid로 `pg_has_role(session_user, $1::oid, 'MEMBER')`를 본다(NOINHERIT 구성원도 `SET ROLE`이 닿으므로 센다. superuser인 Stet에는 이 경고가 나오지 않는다). 읽지 못하면 "확인하지 못했다"고 경고하고 시작한다. 지금처럼 읽기 전용 트랜잭션에서, 이름은 파라미터로만 넘긴다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...` → 모든 패키지 `ok`.
- [ ] **Step 5:** 커밋 `Stet: warn at startup when Stet's account is a superuser or cannot act as an agent role`.

---

### Task 9: 문서와 마무리 확인

**Files:** Modify `stet.example.yaml`, `docs/spec-v0.2-enforce.md`, `docs/spec-v0.2-approve.md`.

- [ ] **Step 1: `stet.example.yaml`.** `limits` 아래에 주석으로 `held:` 블록(`expire_after: 24h`, `apply_timeout: 30s`, 범위와 뜻)과 검토자에게 줄 `GRANT` 세 줄을 더한다. `agents`의 주석에 "Stet의 계정은 각 에이전트 계정의 구성원이어야 하고 superuser가 아니어야 한다. enforce로 시작할 때 경고한다"를 더한다. `TestLoadExampleConfig`와 `TestExampleConfigIsShadowWithTheEnforceKeysCommentedOut`이 통과해야 한다.
- [ ] **Step 2: `docs/spec-v0.2-enforce.md`의 낡은 문장.** 뜻만 고치고, 고친 곳마다 "(2b에서 바뀜: spec-v0.2-approve N장)"을 붙인다.
  - 8장: "Stet 계정이 각 에이전트 계정의 구성원인지는 시작할 때 확인하지 않는다"(B8).
  - 9장: 첫 문단의 "이 단계에서는 넣기만 한다". `status` 주석의 `pending | undelivered`. "서버의 응답이 끝난 뒤 … `undelivered`로 바꾼다. 2b는 `pending`만 승인 대상으로 본다". "`undelivered`로 바꾸는 것은 `pending`인 줄뿐이다". "누가 이 표를 읽고 고칠 수 있는지 … 2b가 정한다". "실행됐다고 보고하면 … `pending`으로 남는다". 텍스트 값의 저장(B1)은 문장이 이미 맞으므로 그대로 둔다.
  - 13장: "hold와 escalate는 이 단계에서 reject와 같아 보인다", "보류 건 삽입의 틈", "프록시가 막은 보류 건을 실행됐다고 보고하면"의 세 줄.
  - 17장 "남긴 것": 승인과 거부, 2b가 넘겨받는 것의 두 줄.
- [ ] **Step 3: `docs/spec-v0.2-approve.md`.** 상태 줄을 "구현됨"으로 바꾼다. "스펙 보정" B1~B7을 해당 장(2·4·6·7장)에 옮겨 적는다. 13장의 줄마다 `[x]`와 테스트 이름을 아래 표대로 적는다. 구현하다 이름이 달라졌으면 실제 이름을 쓴다.
- [ ] **Step 4: 전체 확인.** `go vet ./...`. `STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...` → 모든 패키지 `ok`. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run LatencyBudget -count=1 -v` → 다섯 테스트 PASS.
- [ ] **Step 5: 빌드한 바이너리로 끝에서 끝까지.** `go build -o bin/stet ./cmd/stet`. 스크래치 디렉터리에 설정(`mode: enforce`, `agents: {roles: [stet_md5]}`, `listen: 127.0.0.1:56432`, `upstream`은 `stet_scram`, `model: {adapter: none, on_failure: pass}`, 5행을 넣은 테이블 하나에 `impact: {max_rows: 3}`)을 쓰고 `bin/stet proxy -config <파일>`을 띄운다. `psql`로 다음을 차례로 확인하고 출력을 보고에 붙인다.
  1. 프록시를 거친 `stet_md5`의 `update <t> set name = 'all'` → `ERROR`와 SQLSTATE `ST001`, `DETAIL`에 `held_write=<id>`.
  2. 서버에 직접 붙은 검토자(이 확인을 위해 만든 역할)의 `select id, status, planned_rows, rows_exact, reasons from stet.review` → `pending`, `4`, `f`, `{impact:rows}`.
  3. `select stet.approve('<id>')` → `ST005`와 `pass max_rows`. `select stet.approve('<id>', 'ok', 5)` → `approved`.
  4. 2초 뒤 뷰의 그 줄 → `applied`, `rows_affected = 5`, `resolved_by` = 검토자. 테이블의 5행이 모두 `all`이다.
  5. Stet의 로그에 `held write applied`가 있다. `SIGTERM`으로 끄면 에러 없이 끝난다.
  끝나면 테이블, 검토자 역할, 그 테이블의 보류 줄, 스크래치 파일을 지운다(판단 기록은 append-only라 남는다).
- [ ] **Step 6:** 커밋 `Docs: held.* in the example config, what step 2b changed in the enforce spec, and the approve spec's acceptance checklist`.

**수용 기준(스펙 13장)과 Task.**

| # | 수용 기준 | Task: 테스트 |
|---|---|---|
| 1 | `unconfirmed`로 들어가 `pending`·`undelivered`가 된다 | 2: `TestInsertHeldWrite`, `TestMarkDelivered`, `TestMarkUndelivered`. 5: `TestHeldWriteCarriesWhatApprovalNeeds`, `TestEnforceMarksAHiddenHoldUndelivered`, `TestEnforceMarksAnUnconfirmedHeldWriteUndelivered`, `TestHeldWriteTheProxyReportsAsSentIsUndelivered` |
| 2 | `stet.review`가 근거와 함께, 판단 기록 없는 줄도, `expired` | 3: `TestReviewShowsAHeldWriteWithItsDecision` |
| 3 | 검토자의 승인, 권한 없는 계정, 다시 올려도 남는 권한 | 3: `TestApproveMarksTheRow`, `TestOnlyGrantedRolesReview` |
| 4 | 6장 표의 줄마다 `ST005` | 3: `TestApproveAndRejectRefusals` |
| 5 | 한 Sync 아래 `SET TimeZone` 뒤의 쓰기 | 4: `TestSetPendingUntilReadyForQuery`, `TestGateSeesSessionSettings`. 5: `TestHeldWriteBehindASetUnderOneSyncIsNotStored` |
| 6 | `BEGIN` 뒤 첫 쓰기의 `in_transaction`, 승인하면 그것만 | 5: `TestFirstWriteAfterBeginIsHeldInTransaction`. 7: `TestApprovingTheFirstWriteOfATransactionRunsOnlyIt` |
| 7 | 단일 행 `UPDATE`가 보낸 값 그대로 실행된다 | 5: `TestHeldWriteKeepsTextValuesAsSentAmongBinaryOnes`, `TestHeldWriteKeepsBinaryAndNullParamsAsText`. 6: `TestApplyRunsAnApprovedUpdate`, `TestApplySendsValuesAsStored`. 7: `TestApprovedWriteRunsAsTheAgentSentIt` |
| 8 | 에이전트 계정의 권한, `42501`, 행 수준 보안 | 6: `TestApplyRunsWithTheAgentsRights` |
| 9 | `TimeZone`·`DateStyle`이 다를 때 같은 값 | 6: `TestApplyReadsValuesUnderTheAgentsSettings`. 7: `TestApprovedWriteRunsAsTheAgentSentIt` |
| 10 | `row_changed`의 네 경우, `search_path_differs` | 6: `TestApplyChecksTheRowVersion`, `TestApplyRefusesAnotherSearchPath` |
| 11 | `rows_exceeded`, `max_rows`를 준 승인 | 6: `TestApplyKeepsToTheApprovedRows` |
| 12 | `role_changed`, 그 뒤의 줄은 정상 | 6: `TestApplyUndoesAWriteThatChangesRole` |
| 13 | 서버의 실패와 시간 상한 | 6: `TestApplyRecordsTheServersError` |
| 14 | 다른 상태의 줄은 실행되지 않는다, `approval_lapsed` | 6: `TestApplyLeavesOtherStatusesAlone`. 7: `TestLoopPicksUpApprovalsMadeWhileItWasDown` |
| 15 | 두 실행부, 중간에 실패한 질의 | 6: `TestTwoAppliersRunAWriteOnce`, `TestApplyLeavesTheRowApprovedWhenItsOwnStepFails` |
| 16 | 섀도에서는 실행되지 않는다 | 7: `TestShadowModeRunsNoApprovedWrite` |
| 17 | 누적 한도에 들지 않는다 | 7: `TestAppliedRowsDoNotCountTowardTheCumulativeLimit` |
| 18 | `ST004` | 2: `TestHeldWritesContentIsFixed`, `TestHeldWritesStatusFollowsTheArrows` |
| 19 | 끌 때 끝나거나 버려진다 | 7: `TestCloseWaitsForTheWriteBeingApplied`, `TestCloseAbandonsAWritePastItsBound`, `TestRunStopsTheApplierBeforeThePool` |
| 20 | 10장의 두 경고 | 8: `TestCheckWarnsWhenStetIsASuperuser`, `TestCheckWarnsWhenStetIsNotAMemberOfAnAgentRole` |
| 21 | 2ms 예산, `go test -race ./...` | 4: Step 4. 9: Step 4 |

**실행 전 점검의 판정.** 이 Task의 본문과 다르면 이것을 따른다.

- (P4) Step 3은 B1~B9 전부를 스펙의 해당 장(2·4·5·6·7장)에 옮긴다. B3의 5장 설명과 B9의 `clock_timestamp()`(6·7장, `applied_at` 포함)가 빠지지 않게 한다. 실행 전 점검의 판정 가운데 스펙의 문장을 바꾸는 것(P2: 스키마가 맞으면 `Migrate`가 `stet.held_writes`를 잠그지 않는다)도 4장에 한 줄로 적는다.

