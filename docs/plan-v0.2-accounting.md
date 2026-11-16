# 접속 계정별 누적 한도와 읽기 기록 (섀도) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 접속 계정과 테이블별로 시간 창 안의 쓰기 행 수를 세어 누적 한도를 판정하고, 게이트 대상 테이블을 읽는 문장의 수와 나간 행 수를 접속 계정별로 기록한다. 섀도 모드에서만 동작한다.

**Architecture:** 판단 기록에 접속 계정과 실제 행 수를 더한다. 게이트는 (접속 계정, 테이블)별 사용량을 메모리에 들고, 쓰기를 보내기 전에 잡아 두었다가 결과로 확정한다. 읽기는 프록시가 가벼운 경로로 흘려보내며 나간 행 메시지를 세고, 응답이 끝난 뒤 게이트에 알린다. 게이트는 분 단위로 집계해 `stet.reads`에 쓰고, 큰 읽기는 `stet.read_log`에 남긴다. SQL 커서는 프록시가 세션별로 이름을 기억해 `FETCH`를 읽기로 알린다.

**Tech Stack:** Go 1.27, `github.com/jackc/pgx/v5` v5.11.0 (`pgconn`, `pgproto3`), `github.com/pganalyze/pg_query_go/v6` v6.2.5, 테스트용 PostgreSQL 14+ (`scripts/testdb.sh`).

**Spec:** [docs/spec-v0.2-accounting.md](spec-v0.2-accounting.md). 앞 문서는 [docs/spec-v0.2-impact.md](spec-v0.2-impact.md)다. 실행하는 사람은 둘 다 읽는다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 특히 규칙 6: 섀도 모드는 원래 문장과 그 결과를 바꾸지 않는다. 프록시는 전달되는 바이트를 고치거나 늦추거나 순서를 바꾸지 않는다.
- `cumulative`도 `reads`도 없는 테이블의 기록은 이 변경 전과 같다. 더해지는 것은 `login_role`과 `rows_affected` 두 컬럼의 값뿐이다. 기존 테스트의 단언은 고치지 않는다. 고쳐도 되는 것은 둘이다: `classify_test.go`에서 읽기로 바뀌는 행의 Kind 기대값, 그리고 `internal/proxy` 테스트용 게이트에 새 인터페이스 메서드를 더하는 것.
- 한도와 집계의 기준은 접속 계정(`SessionInfo.User`)이다. `application_name`과 `on_behalf_of`는 기준으로 쓰지 않는다.
- 스펙의 값: reason `impact:cumulative`. 누적 기록의 키 `used`, `planned`, `limit`, `window_s`, `error`. 집계 주기 1분. 쓰지 못한 집계는 한 시간 뒤 버린다. `cumulative.max_rows`는 1 이상 10억 이하이고 `impact.max_rows` 이상. `window`는 0 초과 24시간 이하. `log_above`는 1 이상.
- 읽기는 보내기 전에 게이트를 부르지 않는다. 읽기 경로에 DB 왕복을 넣지 않는다. 읽기의 바인딩 값은 복사하지도 해석하지도 않는다.
- 나간 행을 세는 일은 행마다 정수 하나를 올리는 것이다. 본문을 읽거나 복사하거나 잠금을 잡지 않는다.
- 게이트 대상이 아닌 문장과 `reads`가 켜진 테이블의 읽기 모두 2ms 예산(p95) 안이다. `internal/proxy/latency_test.go`를 고치지 않고 통과한다.
- 테스트: `scripts/testdb.sh start <dir>` 뒤 `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다.
- 코드의 주석과 식별자는 영어, 문서와 YAML·SQL 스키마 주석은 한국어다.
- `main`에 직접 커밋하지 않는다. 브랜치 `v0.2-accounting`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고, 커밋한 모델의 `Co-Authored-By` 줄을 붙인다.

## Review Focus

스펙이 암시하지만 놓치기 쉬운 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **접속 계정을 모르는 세션.** 누적 한도를 적용하지 않고 `cumulative.error`에 남긴다. 다른 계정의 사용량에 섞이지 않는다. (Task 5)
2. **0행에 걸린 쓰기와 실패한 쓰기.** 0행은 `rows_affected = 0`이고 누적 값에 0으로 확정된다. 실패한 쓰기는 `rows_affected`가 NULL이고 잡아 둔 수가 지워진다. (Task 5)
3. **행이 한 줄도 안 나간 읽기와 실패한 읽기.** 문장 수는 세고 행 수는 0이다. 실패하기 전에 나간 행은 센다. (Task 7, Task 9)
4. **같은 분에 여러 번 쓰는 집계, 재시작 뒤 같은 분.** `stet.reads`의 같은 키가 더해지고 덮어써지지 않는다. (Task 2, Task 9)
5. **한 문장이 게이트 대상 테이블 둘을 읽고, 그중 하나만 `reads`가 켜져 있다.** 켜진 테이블에만 센다. (Task 9)

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/config/config.go` | `impact.cumulative`, `reads` 블록 (수정) |
| `internal/store/schema.sql`, `store.go` | `stet.decisions`의 새 컬럼과 인덱스 (수정) |
| `internal/store/reads.go` | `stet.reads`, `stet.read_log` 쓰기와 누적 값 채우기 질의 (신규) |
| `internal/decide/cumulative.go` | `Cumulative` 타입과 누적 판정 (신규) |
| `internal/proxy/session.go` | 끊긴 쓰기 구분, 읽기 경로, 나간 행 세기 (수정) |
| `internal/proxy/cursor.go` | 세션별 커서 표 (신규) |
| `internal/proxy/proxy.go` | 서버→클라이언트 경로에 본문 없이 종류만 알리는 훅 (수정) |
| `internal/sqlparse/read.go` | 읽기 분류, 읽은 테이블 목록, 커서 문장 (신규) |
| `internal/gate/usage.go` | (접속 계정, 테이블)별 사용량: 잡아 두기, 확정, 채우기 (신규) |
| `internal/gate/reads.go` | 읽기 집계, 주기적 쓰기, 큰 읽기 기록 (신규) |
| `internal/gate/gate.go` | 위 둘의 연결 (수정) |

---

### Task 0: 브랜치

- [ ] **Step 1:** `git switch -c v0.2-accounting`
- [ ] **Step 2:** 스펙과 계획을 커밋한다.

```bash
git add docs/spec-v0.2-accounting.md docs/plan-v0.2-accounting.md
git commit -m "Spec and plan: per-role cumulative limits and read recording"
```

- [ ] **Step 3:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`가 모든 패키지에서 `ok`인지 확인한다.

---

### Task 1: 설정

**Files:** Modify `internal/config/config.go`, `stet.example.yaml`. Test `internal/config/config_test.go`.

**Interfaces — Produces:**

```go
// Cumulative limits what one login role may touch in a table within a window.
type Cumulative struct {
	MaxRows int           // 1 to maxImpactRows, and at least Impact.MaxRows
	Window  time.Duration // above 0, at most 24h
}
// in Impact:
Cumulative *Cumulative // nil: no cumulative limit

// Reads turns on read recording for a table.
type Reads struct {
	LogAbove int // 0: aggregate only; otherwise a read sending more rows is logged by itself
}
// in Table:
Reads *Reads // nil: reads of the table are not recorded
```

- [ ] **Step 1: 실패하는 테스트.** `TestCumulativeAndReads`: 블록이 없으면 둘 다 nil. `impact: {max_rows: 100, cumulative: {max_rows: 500, window: 10m}}` → `Cumulative{500, 10 * time.Minute}`. `reads: {}` → `&Reads{}`. `reads: {log_above: 1000}` → `&Reads{LogAbove: 1000}`. `TestInvalidConfigs`에 행을 더한다.

| 이름 | 설정 | 에러에 들어갈 문자열 |
|---|---|---|
| cumulative needs max_rows | `cumulative: {window: 10m}` | `cumulative.max_rows` |
| cumulative needs window | `cumulative: {max_rows: 500}` | `cumulative.window` |
| cumulative below the statement limit | `impact: {max_rows: 100, cumulative: {max_rows: 50, window: 10m}}` | `cumulative.max_rows` |
| cumulative above a billion | `max_rows: 1000000001` | `cumulative.max_rows` |
| window zero | `window: 0s` | `cumulative.window` |
| window above a day | `window: 25h` | `cumulative.window` |
| cumulative without impact max_rows | `impact: {cumulative: {max_rows: 5, window: 1m}}` | `impact.max_rows` |
| log_above zero | `reads: {log_above: 0}` | `reads.log_above` |
| reads unknown key | `reads: {sample: 1}` | `sample` |

- [ ] **Step 2:** `go test ./internal/config/ -run 'Cumulative|InvalidConfigs'` → FAIL.
- [ ] **Step 3: 구현.** 원시 구조체는 포인터 필드로 받는다(`max_rows`, `log_above: 0`의 명시를 구분). `window`는 기존 `parseDuration`으로 읽는다. `stet.example.yaml`의 `impact` 아래와 테이블 아래에 한국어 주석과 함께 예시를 넣는다: `cumulative: {max_rows: 500, window: 10m}`, `reads: {log_above: 1000}`.
- [ ] **Step 4:** `go test ./internal/config/` → PASS. 예시 설정 테스트에 두 블록의 단언을 더한다.
- [ ] **Step 5:** 커밋 `Config: cumulative impact limits and read recording per table`.

---

### Task 2: 저장소

**Files:** Modify `internal/store/schema.sql`, `store.go`. Create `internal/store/reads.go`. Test `internal/store/store_test.go`, `internal/store/reads_test.go`.

**Interfaces — Produces:**

```go
// in Decision:
LoginRole    *string // the role the database authenticated; nil when unknown
RowsAffected *int64  // rows the write changed in the gated table; nil when unknown
Cumulative   any     // stored as jsonb; nil is NULL

// UsageEntry is one executed write counted toward a cumulative limit.
type UsageEntry struct {
	At   time.Time
	Rows int64
}
// Usage returns executed writes by role to table since a time, oldest first.
// A decision with no rows_affected counts as 0 rows and is left out.
func (s *Store) Usage(ctx context.Context, role, table string, since time.Time) ([]UsageEntry, error)

// ReadBucket is one minute of reads by one role on one table.
type ReadBucket struct {
	Bucket     time.Time // start of the minute
	LoginRole  string    // "" when unknown
	Table      string
	Statements int64
	Rows       int64
	MaxRows    int64
}
// AddReads adds buckets to stet.reads, summing into rows that already exist
// and keeping the larger max_rows.
func (s *Store) AddReads(ctx context.Context, buckets []ReadBucket) error

// ReadLog is one large read.
type ReadLog struct {
	ID, AgentID, SessionID, Statement, StatementFP string
	CreatedAt                                      time.Time
	LoginRole, OnBehalfOf, ExecError               *string
	Tables                                         []string
	Rows                                           int64
}
func (s *Store) InsertReadLog(ctx context.Context, r *ReadLog) error
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestInsertRecordsLoginRoleAndRows`: `LoginRole: ptr("agent_a")`, `RowsAffected: ptr(int64(0))`, `Cumulative: map[string]any{"used": 480, "planned": 20, "limit": 500, "window_s": 600}` → 읽으면 `agent_a`, `0`(NULL이 아님), `cumulative->>'limit' = '500'`.
  - `TestInsertLeavesNewColumnsNull`: 기존 `sample`을 그대로 넣으면 세 컬럼이 NULL.
  - `TestUsage`: 같은 역할·테이블에 실행된 기록 3건(행 수 5, NULL, 0)과 실행 안 된 1건, 다른 역할 1건, 창 밖 1건 → 결과는 `[{…, 5}]`의 한 건(0과 NULL은 빠진다).
  - `TestAddReadsSums`: 같은 키로 `{Statements: 2, Rows: 10, MaxRows: 7}`와 `{Statements: 1, Rows: 30, MaxRows: 30}`을 따로 두 번 부르면 `statements=3, rows=40, max_rows=30`. 다른 분의 버킷은 따로 남는다.
  - `TestInsertReadLog`: 한 줄을 넣고 `target_tables`, `rows`, `exec_error`(NULL)를 확인한다.
  - 기존 `TestMigrateIsIdempotentAndConcurrent`가 새 스키마로 통과해야 한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/` → FAIL.
- [ ] **Step 3: 구현.** `schema.sql`에 더한다(스펙 3·4·5장의 정의 그대로, 주석은 한국어):

```sql
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS login_role text;
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS rows_affected bigint;
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS cumulative jsonb;
CREATE INDEX IF NOT EXISTS decisions_role_idx ON stet.decisions (login_role, target_table, created_at DESC);
```

  `stet.reads`와 `stet.read_log`는 스펙 5장의 `CREATE TABLE IF NOT EXISTS` 그대로다. `AddReads`는 한 문장의 `INSERT ... ON CONFLICT (bucket, login_role, target_table) DO UPDATE`로 더하고 `max_rows`는 `greatest`로 둔다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/store/` → PASS.
- [ ] **Step 5:** 커밋 `Store: login role, rows affected, cumulative, and read tables`.

---

### Task 3: 누적 판정

**Files:** Create `internal/decide/cumulative.go`. Modify `internal/decide/decide.go`, `impact.go`. Test `internal/decide/decide_test.go`.

**Interfaces — Produces:**

```go
// Cumulative is what one login role has touched in a table within the window,
// against the limit. It is stored as stet.decisions.cumulative.
type Cumulative struct {
	Used    *int64 `json:"used,omitempty"`    // rows in the window, without this write
	Planned *int64 `json:"planned,omitempty"` // rows this write is expected to touch
	Limit   int64  `json:"limit,omitempty"`
	WindowS int    `json:"window_s,omitempty"`
	Error   string `json:"error,omitempty"` // set when the usage is not known; the limit is then not applied
}
// Over reports whether this write takes the role past the limit.
func (c *Cumulative) Over() bool

// added to Input:
Cumulative *Cumulative
```

- [ ] **Step 1: 실패하는 테스트.** `TestVerdictCumulative` (한도 500):

| 이름 | 입력 | verdict | reasons |
|---|---|---|---|
| single row, within | `used 480, planned 1`, 질문 없음 | `commit` | 없음 |
| single row, over | `used 500, planned 1`, 모델 답은 모두 정상 | `hold` | `impact:cumulative` |
| over wins over a model hold | `used 500, planned 1`, duplicate 답이 hold 조건 | `hold` | `impact:cumulative` |
| over wins over state error | `used 500, planned 1`, `StateErr` | `hold` | `impact:cumulative` |
| exactly at the limit | `used 499, planned 1` | `commit` | 없음 |
| measured, within both | `Impact{count, rows 20}`, `used 480, planned 20` | `commit` | 없음 |
| measured, over cumulative | `Impact{count, rows 21}`, `used 480, planned 21` | `hold` | `impact:cumulative` |
| rows limit wins | `Impact{count, rows 101, capped}`, `used 0, planned 101` | `hold` | `impact:rows` |
| share limit wins | share 초과, 누적도 초과 | `hold` | `impact:share` |
| cumulative before uncertain | `Impact{estimate, rows 50}`, `used 480, planned 50` | `hold` | `impact:cumulative` |
| estimate within cumulative | `Impact{estimate, rows 5}`, `used 0, planned 5` | `escalate` | `impact:uncertain` |
| unknown usage is not applied | `Cumulative{Error: "no login role"}`, 단일 행 정상 | `commit` | 없음 |
| unsupported stays | `Unsupported: "multi_row"`, 누적 초과 | `unsupported` | `multi_row` |
| failed measure stays | `Impact{Error}`, 누적 초과 | `unsupported` | `ShapeReason` |

  그리고 JSON: `Cumulative{Used: ptr(0), Planned: ptr(1), Limit: 500, WindowS: 600}`은 `"used":0`을 담고, `Cumulative{Error: "x"}`는 `{"error":"x"}`뿐이다.
- [ ] **Step 2:** `go test ./internal/decide/ -run Cumulative` → FAIL.
- [ ] **Step 3: 구현.** 순서는 스펙 4장의 표다. `impactVerdict` 안에서는 `impact:share` 다음, `impact:uncertain` 앞에 넣는다. 측정하지 않은 쓰기에서는 `Verdict`의 `Impact` 분기 다음, 상태·모델 실패 앞에 넣는다. `Over`는 `Error == ""`이고 `Used`·`Planned`가 있고 `*Used + *Planned > Limit`일 때 참이다.
- [ ] **Step 4:** `go test ./internal/decide/` → PASS (기존 `TestVerdict`, `TestVerdictImpact` 포함).
- [ ] **Step 5:** 커밋 `Decide: hold a write that takes its role past the cumulative limit`.

---

### Task 4: 프록시 — 답 없이 끊긴 쓰기

**Files:** Modify `internal/proxy/session.go` (`Outcome`, `abandon`). Test `internal/proxy/gate_test.go`.

**Interfaces — Produces:**

```go
// in Outcome:
// Lost means the connection ended before the server answered. The
// statement may have run and committed; Executed is false because nothing
// said so.
Lost bool
```

- [ ] **Step 1: 실패하는 테스트.** `TestGateSeesLostWrites`: 감시되는 `insert into T select 1, 'x' from pg_sleep(1)`를 보낸 직후 클라이언트 연결을 닫는다. 결과는 `Lost == true`, `Executed == false`. 그리고 서버가 에러로 답한 쓰기(PK 중복)는 `Lost == false`.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run LostWrites` → FAIL.
- [ ] **Step 3: 구현.** `abandon`이 `Lost: true`를 넣는다. `Error`의 문구는 그대로 둔다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/` → PASS.
- [ ] **Step 5:** 커밋 `Proxy: tell a lost write apart from a failed one`.

---

### Task 5: 게이트 — 접속 계정, 실제 행 수, 누적 한도

**Files:** Create `internal/gate/usage.go`, `internal/gate/usage_test.go`. Modify `internal/gate/gate.go`.

**Interfaces — Consumes:** `config.Cumulative` (Task 1). `store.Decision.LoginRole/RowsAffected/Cumulative`, `Store.Usage` (Task 2). `decide.Cumulative`, `Input.Cumulative` (Task 3). `proxy.Outcome.Lost` (Task 4).

**Produces (패키지 안):**

```go
// usage tracks, per login role and table, the rows writes touched within the
// table's window. A write is reserved before it is sent and settled when its
// outcome is known.
type usage struct{ /* mutex, entries by (role, table) */ }

// reserve records that write id is about to touch planned rows and returns
// what the role had already used in the window. The first call for a pair
// loads the window from stet.decisions.
func (u *usage) reserve(ctx context.Context, role, table, id string, at time.Time, planned int64, window time.Duration) (used int64, err error)

// settle replaces the reservation with what happened: rows when known, the
// reservation itself when the write ran or was lost with no count, nothing
// when the server said it failed.
func (u *usage) settle(role, table, id string, rows *int64, executed, lost bool)
```

- [ ] **Step 1: 실패하는 테스트** (`usage_test.go`, 패키지 `gate_test`, `setup`·`calm`·`seedMany`·`connectAs` 재사용). 설정: `cumYAML = tableYAML + "    impact: {max_rows: 3, cumulative: {max_rows: 5, window: 1h}}\n"`, 그리고 창이 지나는 것을 보는 테스트 하나만 쓰는 `shortCumYAML`(같은 설정에 `window: 2s`). 새 헬퍼 `(*env).accounts(t, n)`은 기록에서 `verdict, reasons, executed, login_role, rows_affected, cumulative, questions, answers`를 읽는다. 기존 `row`와 `decisions`는 고치지 않는다.

| 테스트 | 쓰기 | 단언 |
|---|---|---|
| `TestEveryDecisionRecordsRoleAndRows` | 기본 `tableYAML`. 단일 행 update, 0행 update(`where id = 999`), PK 중복 insert, 다중 행 insert | `login_role = stet_scram` 모두. `rows_affected`는 1, 0, NULL, 2. `cumulative`는 모두 NULL. verdict와 reasons는 이 변경 전과 같다 |
| `TestRowsAffectedIsNullWhenTheTagIsNotTheTablesCount` | `with d as (delete from T where id = 1 returning 1) select count(*) from d`, 그리고 `update T set status='x' where id = 2; select 1` | 둘 다 `rows_affected` NULL (reason `nested_write`, `multi_statement`) |
| `TestCumulativeHoldsOnceTheWindowIsFull` | `cumYAML`. 2행 update, 2행 update, 단일 행 update, 단일 행 update | 넷째가 `hold [impact:cumulative]`. 기록의 `cumulative`는 차례로 `used 0/planned 2`, `2/2`, `4/1`, `5/1`, `limit 5`, `window_s 3600`. 넷째에도 `questions`와 `answers`가 있다 |
| `TestCumulativeWindowSlides` | `shortCumYAML`. 5행을 채운 뒤 2.2초를 기다리고 단일 행 update | `commit`, `used 0` |
| `TestCumulativeIsPerLoginRole` | `stet_scram`이 5행을 채운 뒤 `stet_md5`가 단일 행 update. `stet_scram`이 새 연결로 단일 행 update | `stet_md5`는 `commit`, `used 0`. `stet_scram`의 새 연결은 `hold` |
| `TestCumulativeCountsConcurrentWrites` | 고루틴 8개가 각자 연결로 2행 update를 동시에 | `commit`은 최대 2건, 나머지는 `hold [impact:cumulative]` |
| `TestCumulativeSurvivesARestart` | 5행을 채우고 기록이 쓰인 것을 확인한 뒤, 같은 테이블과 설정으로 게이트와 프록시를 새로 만들어 단일 행 update | `hold`, `used 5` |
| `TestCumulativeCountsWritesItDoesNotJudge` | 다중 행 upsert 2행(`unsupported`), 트랜잭션 안의 2행 update(`in_transaction`), 그 뒤 2행 update | 앞의 둘은 verdict가 그대로이고 `cumulative`가 채워진다. 셋째는 `used 4`, `hold` |
| `TestCumulativeForgetsFailedWrites` | PK 중복으로 실패하는 단일 행 insert 5번, 그 뒤 단일 행 update | 마지막은 `used 0`, `commit` |
| `TestCumulativeKeepsALostWrite` | 2행 update에 `pg_sleep(1)`을 걸어 보내고 연결을 닫는다. 새 연결로 단일 행 update | 둘째는 `used 2` |
| `TestCumulativeIsNotAppliedWithoutALoginRole` | 프로토콜 3.1의 원시 세션(`rawSimpleQuery`)으로 단일 행 update를 6번 | 모두 `cumulative`가 `{"error": …}`뿐이고 verdict에 `impact:cumulative`가 없다. 그 뒤 `stet_scram`의 쓰기는 `used 0` |

- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/ -run 'Cumulative|RoleAndRows|RowsAffected'` → FAIL.
- [ ] **Step 3: 구현.**
  - `judge`: 모든 기록에 `LoginRole`(빈 문자열이면 nil)과 `RowsAffected`를 넣는다. `RowsAffected`는 `o.Rows`가 있고 reason이 `nested_write`·`multi_statement`가 아닐 때만 넣는다.
  - `Begin`: 테이블에 `Cumulative`가 있으면, 상태 조립과 측정이 끝난 뒤 `planned`를 정해 `reserve`를 부른다. `planned`는 스펙 4장의 표다(단일 행 1, 측정값 또는 상한값, 그 밖은 0). 접속 계정이 없으면 `reserve`를 부르지 않고 `Error: "no login role"`. `reserve`가 실패하면 그 에러를 `Error`에 넣는다. 결과는 `pending`에 둔다.
  - `End`: 기록 고루틴을 띄우기 전에 `settle`을 부른다. 메모리 연산이라 막히지 않는다.
  - `judge`: `in.Cumulative`와 `d.Cumulative`에 넣는다. 단일 행 쓰기가 누적 한도에 걸려도 모델 호출 분기는 그대로 탄다(섀도).
  - 채우기: 쌍을 처음 볼 때 `store.Usage`로 창 안의 항목을 읽는다. 실패하면 그 쌍은 "아직 채우지 않음"으로 남겨 다음 쓰기에서 다시 읽는다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/` → PASS. 기존 테스트는 고치지 않는다.
- [ ] **Step 5:** 커밋 `Gate: record the login role and rows, and count writes toward a cumulative limit`.

---

### Task 6: 분류기 — 읽기와 커서

**Files:** Create `internal/sqlparse/read.go`. Modify `internal/sqlparse/classify.go` (`Kind`, `Result`, `Classify`), `internal/gate/gate.go` (`Inspect` 한 줄). Test `internal/sqlparse/classify_test.go`.

**Interfaces — Produces:**

```go
const Read Kind = 4 // reads a gated table and writes none; String() returns "read"

type CursorOp int
const (
	CursorDeclare CursorOp = iota + 1
	CursorFetch
	CursorClose    // CLOSE name
	CursorCloseAll // CLOSE ALL
)
// Cursor says what a statement does to a SQL cursor.
type Cursor struct {
	Op   CursorOp
	Name string // as the parser normalizes it
	Hold bool   // DECLARE ... WITH HOLD
}

// in Result:
Reads  []string // config keys of the gated tables the statement reads, sorted; set when Kind is Read
Cursor *Cursor  // set for a single DECLARE CURSOR, FETCH, or CLOSE
```

- [ ] **Step 1: 실패하는 테스트.** `gated` 맵에 `deals`를 더한 맵을 따로 쓴다(`gated2 = {"contacts": "id", "deals": "id"}`).
  - `TestClassifyKinds`에서 Kind가 바뀌는 행: `select`(`select * from contacts where id = 1`)와 `copy out`(`copy contacts to stdout`)이 `Pass`에서 `Read`로. 다른 행은 그대로다.
  - `TestClassifyReads`:

| sql | Kind | Reads |
|---|---|---|
| `select * from contacts` | Read | `[contacts]` |
| `table contacts` | Read | `[contacts]` |
| `select c.name from contacts c join deals d on d.contact_id = c.id` | Read | `[contacts deals]` |
| `select 1 where exists (select 1 from contacts)` | Read | `[contacts]` |
| `with x as (select * from deals) select * from x` | Read | `[deals]` |
| `copy contacts to stdout` | Read | `[contacts]` |
| `copy (select * from contacts) to stdout` | Read | `[contacts]` |
| `insert into log select * from contacts` | Read | `[contacts]` |
| `update log set n = c.n from contacts c where log.id = c.id` | Read | `[contacts]` |
| `select 1; select * from contacts` | Read | `[contacts]` |
| `select * from leads` | Pass | 없음 |
| `alter table contacts add column x int` | Pass | 없음 |
| `analyze contacts` | Pass | 없음 |
| `update contacts set title = l.t from deals l where contacts.id = 1` | Unsupported (`join`) | 없음 |
| `insert into contacts (name) select name from deals` | Measured | 없음 |

  - `TestClassifyCursors`:

| sql | Kind | Reads | Cursor |
|---|---|---|---|
| `declare c cursor for select * from contacts` | Read | `[contacts]` | `{CursorDeclare, "c", false}` |
| `declare "My C" cursor with hold for select * from contacts` | Read | `[contacts]` | `{CursorDeclare, "My C", true}` |
| `declare c cursor for select 1` | Pass | 없음 | `{CursorDeclare, "c", false}` |
| `fetch 100 from c` | Pass | 없음 | `{CursorFetch, "c", false}` |
| `FETCH FORWARD 5 FROM C` | Pass | 없음 | `{CursorFetch, "c", false}` |
| `move 10 in c` | Pass | 없음 | nil |
| `close c` | Pass | 없음 | `{CursorClose, "c", false}` |
| `close all` | Pass | 없음 | `{CursorCloseAll, "", false}` |
| `fetch 1 from c; fetch 1 from d` | Pass | 없음 | nil |

- [ ] **Step 2:** `go test ./internal/sqlparse/` → FAIL.
- [ ] **Step 3: 구현.** `Classify`에서 게이트 대상 쓰기가 없을 때(`touching == 0`) `read.go`의 함수로 읽은 테이블을 모은다. 읽는 자리는 넷이다: `SelectStmt`의 `FromClause`(조인과 그 안의 `RangeVar`), `UpdateStmt`·`DeleteStmt`·`MergeStmt`의 `FromClause`·`UsingClause`·원본 관계, `CopyStmt`(`IsFrom`이 거짓)의 `Relation`. 트리 전체를 기존 `walk`로 걸으면 서브쿼리·`WITH`·`INSERT ... SELECT` 안의 `SelectStmt`에도 닿는다. DDL과 유지보수 문장의 `RangeVar`는 이 자리에 없으므로 잡히지 않는다. 커서는 메시지에 문장이 하나일 때만 본다. `WITH HOLD`는 `DeclareCursorStmt.Options`의 비트로 판별하고, 위 테스트가 그 비트를 확인한다.
- [ ] **Step 4:** `go test ./internal/sqlparse/` → PASS. `go test -bench . -run xxx ./internal/sqlparse/`로 문장당 분류 시간이 수십 마이크로초 안인지 보고한다.
- [ ] **Step 5: 게이트가 읽기를 아직 감시하지 않게 한다.** `gate.Inspect`의 `Watch: r.Kind != sqlparse.Pass`를 `Watch: r.Kind == sqlparse.Gated || r.Kind == sqlparse.Measured || r.Kind == sqlparse.Unsupported`로 바꾼다. Task 9가 읽기를 연결한다.

  Run: `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/ ./internal/proxy/` → PASS, 기존 테스트를 고치지 않는다.
- [ ] **Step 6:** 커밋 `Classifier: reads of gated tables and cursor statements`.

---

### Task 7: 프록시 — 읽기 경로와 나간 행 세기

**Files:** Modify `internal/proxy/session.go`, `internal/proxy/proxy.go`, `internal/gate/gate.go`(빈 `Observe` 메서드만). Test `internal/proxy/gate_test.go`, `internal/proxy/latency_test.go`(새 테스트만 더한다).

**Interfaces — Produces:**

```go
// in Gate:
// Observe reports a read after its response ended, or when the connection
// ended first. It is never called before the statement goes upstream, and it
// must not block.
Observe(s SessionInfo, st Statement, o Outcome)

// in Statement:
// Observe marks a read: it is forwarded at once, its bind values are not
// kept, and Observe is called when its response ends.
Observe bool

// in Outcome:
// Sent is how many row messages (DataRow, CopyData) went to the client in
// answer to the statement, also when it failed or was cancelled.
Sent int64
```

- [ ] **Step 1: 실패하는 테스트.** `recordingGate`에 `observeMatch func(sql string) bool`과 `Observe` 메서드, 받은 호출을 기다리는 `observed(t, n)`을 더한다(기존 필드와 단언은 그대로). 테이블에 1,000행을 넣어 둔다.

| 테스트 | 보내는 것 | 단언 |
|---|---|---|
| `TestGateObservesReads` | 단순 프로토콜 `select * from T`, 확장 프로토콜 `select * from T where id <= $1`(250), `copy T to stdout`, 0행 `select` | `Sent`는 1000, 250, 1000, 0. `Executed` 참. `Begin`은 한 번도 불리지 않는다. 세션 정보의 `User`가 맞다 |
| `TestGateObservesRowsOfAFailedRead` | `select id, 1 / (500 - id) from T order by id` (500번째 행에서 0으로 나눔) | `Executed` 거짓, `Error`에 `22012`, `Sent`는 0보다 크고 500보다 작다 |
| `TestGateObservesRowsOfACancelledRead` | `select * from T, pg_sleep(0.001)`류로 느리게 흘리다가 컨텍스트를 취소 | `Sent > 0`, `Executed` 거짓 |
| `TestGateObservesPortalFetchedInParts` | `stet_trust`(trust 인증)의 원시 연결과 `pgproto3`로 Parse, Bind, `Execute{MaxRows: 100}`을 세 번, Sync | `Observe`가 세 번, 각각 `Sent == 100` |
| `TestGateObservesReadsInAMultiStatementQuery` | `select 1; select * from T` | `Observe` 한 번, `Sent == 1001` |
| `TestGateObservesALostRead` | 큰 읽기를 받다가 클라이언트 연결을 닫는다 | `Lost` 참, `Sent > 0` |
| `TestObservedReadsKeepNoBindValues` | 관찰되는 문장을 값과 함께 실행 | `Observe`에 온 `Statement`가 맞고, `Begin`이 불리지 않았으므로 값이 게이트에 가지 않는다 |
| `TestObservedReadLatencyBudget` | `TestPassthroughLatencyBudget`와 같은 방식으로, `select 1`을 관찰 대상으로 표시한 게이트를 붙여 잰다 | p95 추가 지연 2ms 미만 |

  기존 프로토콜 묶음(`TestWatchedPassthrough`)을 관찰 게이트로도 한 번 더 돌리는 `TestObservedPassthrough`를 더한다. 결과가 직접 연결과 같아야 한다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'Observ'` → FAIL.
- [ ] **Step 3: 구현.**
  - `tap`에 본문 없이 메시지 종류만 받는 훅을 더한다. `pump`는 메시지 머리를 읽은 직후 그 훅을 부르고, 본문은 지금처럼 읽지 않고 흘려보낸다.
  - 서버 쪽 훅은 `'D'`와 `'d'`에서 세션의 카운터를 올린다. 이 카운터는 서버 쪽 고루틴만 만지므로 잠금이 없다.
  - `fromServer`가 완료(`'C'`, `'I'`, `'s'`)나 에러(`'E'`)를 볼 때, 그동안 쌓인 수를 그 완료 순번에 옮기고 카운터를 0으로 돌린다. 단위에 관찰되는 읽기가 없으면 버린다.
  - `fromClient`: `st.Observe`인 문장은 단위에 읽기로만 적는다(세션 정보의 사본, 문장, Execute 순번). `flush`도 `Begin`도 부르지 않는다. Bind에서는 관찰되는 문장의 포털에 값을 복사하지 않는다.
  - `finish`: 읽기마다 `Observe`를 부른다. Query 단위는 그 단위에서 나간 행 전체가 `Sent`다. Execute는 그 순번의 것이다. 실패 판정은 쓰기와 같은 규칙이다.
  - `abandon`: 답을 못 받은 읽기를 `Lost: true`와 그때까지의 `Sent`로 알린다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/` → PASS. `internal/gate`도 컴파일되도록 `Gate`에 빈 `Observe`를 더한다(Task 9가 채운다).
- [ ] **Step 5: 큰 결과의 수신 시간을 잰다.** `select generate_series(1, 2000000)`을 직접 연결, 게이트 없는 프록시, 게이트 있는 프록시로 각 5번 받아 중앙값을 보고서에 적는다. 단언은 두지 않는다. 게이트 유무의 차이가 5%를 넘으면 `DONE_WITH_CONCERNS`로 보고한다.
- [ ] **Step 6:** 커밋 `Proxy: a light path for reads that counts the rows sent to the client`.

---

### Task 8: 프록시 — 커서

**Files:** Create `internal/proxy/cursor.go`. Modify `internal/proxy/session.go`. Test `internal/proxy/gate_test.go`.

**Interfaces — Consumes:** Task 7의 `Observe`.

**Produces:**

```go
type CursorOp int
const (
	CursorDeclare CursorOp = iota + 1
	CursorFetch
	CursorClose
	CursorCloseAll
)
// Cursor says what a statement does to a SQL cursor.
//
// A CursorDeclare statement is never reported itself: it sends no rows. When
// its Statement.Observe is set, the statement is remembered under the name
// and reported as the read behind each later FETCH. When it is not set, the
// name is forgotten, so a cursor redeclared over an unobserved query stops
// being reported.
type Cursor struct {
	Op   CursorOp
	Name string
	Hold bool
}
// in Statement:
Cursor *Cursor
```

- [ ] **Step 1: 실패하는 테스트.** `recordingGate`의 `Inspect`가 정규식으로 `declare <이름> cursor [with hold] for …`, `fetch … from <이름>`, `close <이름>`, `close all`을 알아보고 `Cursor`를 채우게 한다(테스트용). `declare`의 질의가 `observeMatch`에 맞으면 `Observe`도 켠다. 테이블에 1,000행.

| 테스트 | 보내는 것 | 단언 |
|---|---|---|
| `TestGateObservesFetchesAsReadsOfTheCursor` | `begin; declare c cursor for select * from T; fetch 300 from c; fetch 300 from c; close c; commit` (문장마다 따로) | `Observe` 두 번, 각 `Sent == 300`, 온 `Statement.SQL`은 `declare …` 문장이다. `declare` 자체로는 `Observe`가 불리지 않는다 |
| `TestCursorIsForgottenWhenClosed` | 닫은 뒤 같은 이름으로 `fetch` (서버가 에러를 낸다) | 추가 `Observe` 없음 |
| `TestCursorIsForgottenAtTransactionEnd` | `begin; declare c …; commit;` 뒤에 새 트랜잭션에서 관찰되지 않는 질의로 `declare c cursor for select 1; fetch 1 from c` | 둘째 `fetch`에 `Observe` 없음 |
| `TestHeldCursorSurvivesCommit` | `begin; declare c cursor with hold for select * from T; commit; fetch 10 from c` | `Observe` 한 번, `Sent == 10` |
| `TestFailedDeclareIsNotRemembered` | `declare c cursor for select * from no_such_table`(에러), 롤백, 그 뒤 관찰되지 않는 `declare c cursor for select 1; fetch 1 from c` | `Observe` 없음 |
| `TestRedeclaredCursorReplacesTheOld` | `c`를 닫고 다른 관찰 질의로 다시 선언한 뒤 `fetch` | 온 `Statement.SQL`이 새 선언이다 |
| `TestCloseAllForgetsEveryCursor` | 둘을 선언하고 `close all`, 그 뒤 `fetch` | 추가 `Observe` 없음 |
| `TestCursorsThroughTheExtendedProtocol` | `declare`와 `fetch`를 `conn.Exec`(확장 프로토콜)로 | 단순 프로토콜과 같은 결과 |

- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'Cursor|Fetches'` → FAIL.
- [ ] **Step 3: 구현.** 세션에 커서 표(이름 → 선언한 `Statement`, `Hold`)를 둔다. 선언은 문장이 올 때 적고, 그 단위가 실패로 끝나면 지운다. `fetch`는 표에서 찾아, 있으면 그 선언의 `Statement`로 읽기를 적는다(Task 7의 경로). `close`, `close all`, 세션 종료에서 지우고, ReadyForQuery의 상태가 `I`로 돌아오면 `Hold`가 아닌 것을 지운다. 표는 두 고루틴이 만지므로 `s.mu`로 지킨다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/` → PASS.
- [ ] **Step 5:** 커밋 `Proxy: remember SQL cursors and report each FETCH as a read`.

---

### Task 9: 게이트 — 읽기 집계와 큰 읽기 기록

**Files:** Create `internal/gate/reads.go`, `internal/gate/reads_test.go`. Modify `internal/gate/gate.go` (`New`, `Close`, `Inspect`, `Observe`), `internal/gate/export_test.go`.

**Interfaces — Consumes:** `config.Reads` (Task 1). `store.ReadBucket`, `AddReads`, `ReadLog`, `InsertReadLog` (Task 2). `sqlparse.Read`, `Result.Reads`, `Result.Cursor` (Task 6). `proxy.Statement.Observe`, `Cursor`, `Gate.Observe`, `Outcome.Sent`/`Lost` (Task 7, 8).

**Produces (테스트 전용, `export_test.go`):**

```go
// FlushReads writes every read bucket held in memory now.
func (g *Gate) FlushReads()
```

- [ ] **Step 1: 실패하는 테스트** (`reads_test.go`). 설정: `readsYAML = tableYAML + "    reads: {log_above: 3}\n"`. 헬퍼 `(*env).reads(t)`는 `stet.reads`에서 이 테이블의 행을, `(*env).readLog(t)`는 `stet.read_log`에서 이 테이블을 읽은 행을 돌려준다. 집계는 `FlushReads` 뒤에 읽는다.

| 테스트 | 하는 일 | 단언 |
|---|---|---|
| `TestReadsAreAggregatedPerRoleAndTable` | `seedMany`(7행). `stet_scram`이 `select *` 두 번과 2행짜리 `select`, `stet_md5`가 `select *` 한 번 | `stet_scram`: `statements 3, rows 16, max_rows 7`. `stet_md5`: `statements 1, rows 7`. 클라이언트가 받은 행은 직접 연결과 같다 |
| `TestLargeReadsAreLogged` | 2행짜리 `select`와 7행짜리 `select` | `stet.read_log`에 7행짜리만. `statement`, `login_role`, `agent_id`, `target_tables = {T}`, `rows 7`, `exec_error` NULL |
| `TestReadsWithoutTheBlockAreNotSeen` | 기본 `tableYAML`. `select *` | `stet.reads`에 이 테이블의 행이 없다. 판단 기록도 없다 |
| `TestReadOfTwoTablesCountsOnlyTheOneTurnedOn` | 두 번째 게이트 대상 테이블을 `reads` 없이 더한 설정. 둘을 조인 | `reads`가 켜진 테이블에만 센다 |
| `TestFailedReadCountsTheRowsItSent` | 도중에 0으로 나누는 `select` | `statements 1`, `rows`는 0보다 크다. 7행을 넘겼으면 `read_log`의 `exec_error`에 `22012` |
| `TestEmptyReadCountsTheStatement` | 0행 `select` | `statements 1, rows 0` |
| `TestCursorReadsAreCounted` | `begin; declare c cursor for select * from T; fetch 5 from c; fetch 5 from c; commit` | `statements 2, rows 7`(5와 2). `read_log`에 5행짜리 한 줄, `statement`는 `declare …` |
| `TestReadsWithoutALoginRoleUseAnEmptyRole` | 프로토콜 3.1 원시 세션으로 `select *` | `login_role = ''` |
| `TestSameMinuteIsSummedAcrossFlushes` | `select *`, `FlushReads`, `select *`, `FlushReads` | 이 테이블의 행을 모두 더하면 `statements 2, rows 14`이고 행은 둘 이하다. 두 읽기가 같은 분이면 한 행이다(덮어쓰지 않고 더한다) |
| `TestCloseWritesWhatIsStillInMemory` | `select *` 뒤에 게이트를 닫는다 | 집계가 쓰여 있다 |
| `TestWritesAreNotCountedAsReads` | `insert into T select …`, `update T … returning *` | `stet.reads`에 행이 없다 |
| `TestReadsDoNotTouchTheDatabaseBeforeTheStatement` | `reads`를 켠 채 풀의 연결을 모두 잡아 둔 상태에서 `select *` | 읽기가 지연 없이 끝난다(500ms 미만). 풀을 풀어 준 뒤 `FlushReads`하면 집계가 있다 |

- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/ -run 'Read'` → FAIL.
- [ ] **Step 3: 구현.**
  - `Inspect`: `Read`이고 읽은 테이블 중 `Reads`가 켜진 것이 있으면 `Observe: true`로 한다. 커서 문장(`DECLARE`, `FETCH`, `CLOSE`)은 언제나 `Statement.Cursor`를 채운다. `sqlparse.Cursor`를 `proxy.Cursor`로 옮긴다. 선언 문장을 읽기로 세지 않는 것은 프록시가 한다(Task 8).
  - `Observe`: 읽은 테이블 중 `Reads`가 켜진 것마다 (분, 접속 계정, 테이블) 버킷에 문장 1과 `o.Sent`를 더하고 `max_rows`를 올린다. `log_above`가 있고 `o.Sent`가 그보다 크면 `ReadLog`를 만들어 별도 고루틴에서 넣는다(`g.wg`). 한 문장에 한 줄이다. 막히는 일을 하지 않는다.
  - 쓰기: `Reads`가 켜진 테이블이 하나라도 있으면 `New`가 1분 주기의 고루틴을 띄운다. 지난 분의 버킷을 `AddReads`로 쓰고, 실패하면 남겨 다음에 다시 쓴다. 한 시간이 지난 버킷은 버리고 로그를 남긴다. `Close`는 고루틴을 멈추고 남은 것을 모두 쓴 뒤 `wg`를 기다린다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/` → PASS.
- [ ] **Step 5:** 커밋 `Gate: aggregate reads per login role and log the large ones`.

---

### Task 10: 마무리

**Files:** Modify `docs/spec-v0.2-accounting.md`.

- [ ] **Step 1: 전체를 확인한다.**

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...` → 모든 패키지 `ok`.
Run: `go build -o bin/stet ./cmd/stet && go vet ./... && gofmt -l .` → 출력 없음.
Run: `STET_TEST_PG=127.0.0.1:55432 go test -race -count=10 ./internal/gate/ -run 'Cumulative|Read' ./internal/proxy/ -run 'Observ|Cursor|Lost'` → 실패와 경쟁이 없다. 흔들리는 테스트가 있으면 숨기지 않고 보고한다.

- [ ] **Step 2: 예시 설정으로 띄워 본다.** `stet.example.yaml`을 복사해 `upstream`을 테스트 DB로, `model`을 `{adapter: none}`으로 바꾸고 `bin/stet proxy -config <파일>`이 시작되는지 확인한다.
- [ ] **Step 3: 스펙을 맞춘다.** 상태 줄을 `상태: 구현됨 (섀도)`로 바꾸고, 10장에서 테스트로 확인된 항목을 `[x]`로 바꾸며 테스트 이름을 적는다. 확인하지 못한 항목은 그대로 두고 이유를 적는다. 구현하면서 스펙과 달라진 곳, 9장에 더할 한계가 있으면 코드가 실제로 하는 것만 적는다.
- [ ] **Step 4:** 커밋 `Spec: mark cumulative limits and read recording as implemented`.
