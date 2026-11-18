# 되돌리기 (4단계) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 에이전트 계정이 바꾼 행의 이전·이후 값을 DB의 트리거가 남기고, 검토자가 `stet.undo_plan`으로 미리 본 뒤 자기 트랜잭션에서 `stet.undo`로 되돌린다. 남긴 변경은 `stet.history`에서 판단 기록에 이어져 보인다.

**Architecture:** 캡처와 되돌리기는 전부 `schema.sql`의 PL/pgSQL이다. 테이블 소유자가 설치한 문장 단위 트리거가 `stet.capture()`를 부르고, 이 함수가 `stet.change_sets`와 `stet.changes`에 컬럼별 글자 값을 남긴다. `stet.undo`는 검토자의 권한으로 돌고, Stet 자신의 읽기와 비교는 설정을 고정한 도우미 함수 안에서 한다. Go 쪽은 판단 기록에 백엔드 pid와 시간 창을 적고(프록시·게이트·저장소), 설치 SQL을 출력하고(`stet undo-sql`), 시작할 때 캡처 설정을 맞추고 오래된 기록을 지운다.

**Tech Stack:** Go 1.27, `github.com/jackc/pgx/v5` v5.11.0, PL/pgSQL, 테스트 서버 PostgreSQL 14.18.

**Spec:** [docs/spec-v0.2-undo.md](spec-v0.2-undo.md). 스펙과 이 계획이 다르면 아래 "스펙 보정"에 적힌 곳만 계획을 따르고, 나머지는 스펙을 따른다. 보정은 실행을 맡은 쪽이 받아들였고(U1~U9), Task 11이 스펙에 옮겨 적는다. 소유자에게는 끝날 때 보고한다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 규칙 6: 섀도에서 프록시가 넘기는 바이트와 Stet 프로세스의 동작은 그대로다. 프록시가 더 읽는 것은 BackendKeyData 하나이고 고치지 않는다. 규칙 3: `stet.decisions`의 새 컬럼 셋은 줄을 넣을 때만 적는다. 규칙 1: 캡처와 되돌리기는 모델을 부르지 않는다. 규칙 5: 남긴 값, `query`, `app`은 모델에 보내지 않고 판정에 쓰지 않는다. 규칙 7: `decide.Decider`와 판정 규칙은 바꾸지 않는다.
- 스펙의 값. 트리거 이름 `stet_capture_ins`, `stet_capture_upd`, `stet_capture_del`, `stet_capture_trunc`. `op`: `insert`, `update`, `delete`, `truncate`. `skipped`: `too_many_rows`, `too_many_statements`, `truncate`, `capture_failed`. 다섯 설정: `DateStyle = 'ISO, MDY'`, `IntervalStyle = 'postgres'`, `extra_float_digits = 1`, `TimeZone = 'UTC'`, `bytea_output = 'hex'`. 그리고 `client_min_messages = warning`, `search_path = pg_catalog, pg_temp`. 한 문장의 상한 기본 10,000(1 이상 1,000,000 이하). 한 트랜잭션에서 캡처가 표에 쓰는 것은 32번까지이고 32번째는 언제나 `too_many_statements` 표지다. 잇기의 앞쪽 여유 10ms. 건너뛰는 이유: `table_gone`, `columns_changed`, `key_changed`, `interleaved`, `changed_since`, `no_net_change`, `referenced: <테이블들>`. `action`: `delete`, `insert`, `update`. `outcome`: `would_restore`, `skipped`, `not_captured`(계획), `restored`, `restored_differs`, `skipped`, `failed`, `not_captured`(되돌리기). `failed`의 `reason`: `SQLSTATE <코드>: <메시지>`. 경고: `children_not_captured: <테이블들>`, `not_selected: <테이블들>`. SQLSTATE: 기록을 지키는 트리거 `ST004`, 되돌리기의 거절 `ST006`. `keep` 기본 `30d`(1h 이상), 지우기는 한 시간마다 10,000묶음씩. `stet undo-sql`의 `lock_timeout`은 5초.
- 테스트: `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. PostgreSQL 14.18이 이미 떠 있다(superuser `postgres`/`postgres-pw`, 데이터베이스 `stet_test`, 계정 `stet_scram`/`scram-pw`, `stet_md5`/`md5-pw`, 비밀번호 없는 `stet_trust`). 테스트 서버를 켜거나 끄지 않는다. 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다. 모든 Task는 이 명령이 모든 패키지에서 `ok`인 채로 끝난다.
- 테스트 계정: Stet 자신은 `stet_scram`, 에이전트는 `stet_md5`다. **`stet_trust`를 어떤 역할에도 부여하지 않고, 어떤 역할도 `stet_trust`에 부여하지 않는다.** PUBLIC, `public` 스키마, 공유 데이터베이스 `stet_test` 자체에는 GRANT·REVOKE하지 않는다. `stet` 스키마 안에서 이 단계의 함수들의 `EXECUTE`를 PUBLIC에서 거두는 것은 이 단계의 일이므로 된다.
- **캡처 트리거를 다는 테스트와 `stet.capture_settings`에 기대는 테스트는 자기 데이터베이스에서 돈다**(`internal/store`의 `ownDatabase`, `cmd/stet`의 `cleanDatabase`, 다른 패키지는 같은 모양의 헬퍼를 둔다). `stet.capture_settings`는 데이터베이스마다 한 줄이고 패키지들은 `stet_test`를 함께 쓰며 나란히 돌기 때문이다. 테스트가 만든 역할은 `DROP OWNED BY`와 `DROP ROLE`로 지운다. 서버에 이미 있는 벤치마크 데이터베이스와 역할(`crm_owner`, `gate_svc`, `agent_*`)은 지우지도 고치지도 않는다.
- TDD: 동작이 바뀌는 곳은 실패하는 테스트를 먼저 쓰고, 돌려서 실패를 본 뒤에 구현한다. SQL 함수도 같다.
- `internal/proxy`를 건드리는 Task는 `internal/proxy/latency_test.go`(p95 2ms)도 돌린다. 그 파일의 기존 테스트는 고치지 않는다.
- 기존 테스트의 단언은 Task가 이름을 적어 허락한 곳만 고친다. 테스트용 게이트와 헬퍼를 새 필드에 맞추는 것은 단언 변경이 아니다.
- README.md, docs/concept-v0.2.md, docs/decisions.md(소유자의 문서)와 앞 단계의 계획 문서는 고치지 않는다.
- 문서와 YAML·SQL 스키마 주석은 한국어, 코드의 주석·식별자·에러 메시지·로그·프로그램의 출력은 영어다.
- API 키(`~/.config/stet/*.env`)는 읽지 않는다. 이 단계는 모델을 부르지 않는다.
- 브랜치 `v0.2-undo`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고 마지막 줄은 `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`이다.

## Review Focus

스펙이 암시하지만 수용 기준에 줄이 없는 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **인용이 필요한 이름.** 대문자·공백·따옴표가 든 테이블과 컬럼 이름, 스키마까지 적은 설정 키(`crm."Deal Notes"`), 예약어인 키 컬럼(`"order"`), 대문자가 든 에이전트 계정 이름. 캡처되고, 되돌려지고, 설치 SQL이 그대로 실행된다. (Task 2 `TestCaptureQuotesNames`, Task 7 `TestUndoQuotesNames`, Task 8 `TestInstallSQLQuotesNames`)
2. **에이전트가 캡처가 읽는 세션 값을 건드린다.** `stet.captures`에 숫자가 아닌 값이나 큰 값, `stet.held_write`에 uuid가 아닌 값, 에이전트 접속이 `SET ROLE`로 다른 역할이 된 것. 쓰기는 성공하고, 변경은 접속 계정의 것으로 남는다. (Task 3 `TestAgentCannotBreakCaptureThroughSessionSettings`)
3. **글자로 헷갈리는 값.** 키 값이 `null`, 빈 글자, 쉼표와 따옴표가 든 글자인 행과, 값이 글자 `NULL`인 컬럼. 키가 `uuid`·`bigint`·`text`인 테이블. 남고 그대로 돌아온다. (Task 2 `TestCaptureKeepsOddKeysApart`, Task 7 `TestUndoRestoresOddKeys`)
4. **남긴 뒤에 테이블이 달라졌다.** 컬럼을 더했다, 지웠다, 이름을 바꿨다(`columns_changed`). 테이블을 지우고 같은 이름으로 다시 만들었다(`table_gone`). 테이블 이름만 바꿨다(되돌려진다). (Task 6 `TestPlanSkipsWhatNoLongerFitsTheTable`)
5. **큰 되돌리기와 없는 이름.** 한 번에 5,000개 키를 되돌린다(각 키가 한 번씩 처리되고 끝난다). `tables`에 설정에 없는 이름을 준다(빈 결과이고 에러가 아니다). `since`가 미래다(빈 결과). (Task 7 `TestUndoOfManyKeys`, `TestUndoOfNothing`)

## 스펙 보정

코드와 PostgreSQL 14.18에 맞춰 보니 스펙이 정하지 않았거나 스펙대로는 모자란 곳이다. 가장 작은 보정을 고르고, Task 11이 스펙에 옮겨 적는다.

| # | 스펙 | 확인한 것 | 보정 (Task) |
|---|---|---|---|
| U1 | 4장 `stet.change_sets`에 키 컬럼의 이름이 없다 | 되돌리기는 그 행을 키 컬럼으로 찾는다. 이름은 트리거의 인자에만 있어서, 트리거가 지워진 테이블의 변경은 되돌릴 수 없게 된다 | `stet.change_sets`에 `key_column text NOT NULL`(트리거의 둘째 인자)을 더한다. 지금 테이블에 그 컬럼이 없으면 `columns_changed`다 (2, 6) |
| U2 | 7장 "키마다 … 그 키만 되돌리고(저장점)" | 저장점은 키마다 하위 트랜잭션 하나다. 64개를 넘으면 4장이 캡처에서 피한 것과 같은 느려짐이 검토자의 트랜잭션이 열려 있는 동안 생긴다. PL/pgSQL에서 에러를 잡는 다른 방법이 없다 | 그대로 둔다. 11장에 한계로 적는다: 큰 되돌리기는 범위를 나눠 부르고 결과를 본 뒤 오래 열어 두지 않는다 (7, 11) |
| U3 | 7장 3 "`update`와 `insert`는 B의 값을 준다" | 모든 컬럼을 `SET`하면 값이 같은 키 컬럼까지 다시 쓰여 외래 키 확인과 `UPDATE OF` 트리거가 불필요하게 돈다. `referenced`의 "참조되는 컬럼의 값을 바꿀 때"도 바뀌는 컬럼을 알아야 한다 | `update`는 지금 행(A)과 B가 다른 컬럼만 `SET`한다. 결과의 행은 같다 (7) |
| U4 | 7장 "거절하는 경우"가 `stet.undo` 아래에만 있다 | `stet.undo_plan`은 바로 이어 부른 `stet.undo`와 같은 것을 말해야 한다(13장) | 네 거절은 두 함수 모두에 적용한다 (6, 7) |
| U5 | 7장 `stet.undos`의 "`restored`·`skipped`·`failed`의 수" | `outcome`은 다섯이다 | `restored`는 `restored`와 `restored_differs`를, `skipped`는 `skipped`와 `not_captured`를 센다 (7) |
| U6 | 9장 "`undo` 절이 없으면 …" | `undo:`만 있고 `tables`가 비어 있는 경우가 없다 | 설정 오류다(`undo.tables`) (1) |
| U7 | 9장 "Stet이 시작할 때(`undo.tables`가 있으면, 두 모드 모두)" | `cmd/stet`의 `openGate`는 게이트 대상 테이블이 없으면 풀을 열지 않는다. `undo.tables`에는 게이트 대상이 아닌 테이블만 있을 수 있다 | `undo`가 있으면 게이트 대상 테이블이 없어도 풀을 열고 스키마를 올리고 9장의 일을 한다. 게이트와 기존의 시작 확인은 지금처럼 없다 (9) |
| U8 | 9장 `stet undo-sql` "아무것도 실행하지 않는다" | 첫 묶음의 `GRANT EXECUTE ON FUNCTION stet.capture()`는 `stet` 스키마가 있어야 실행된다. 이 명령은 스키마를 올리지 않는다 | `stet.capture()`가 없으면 "Stet을 한 번 시작해 스키마를 만들라"고 말하고 0이 아닌 코드로 끝난다 (8) |
| U9 | 4장 "세는 방법은 만드는 쪽이 정한다", 7장 "도우미 함수의 이름과 나눔은 만드는 쪽이 정한다" | | 세는 값은 트랜잭션 안에서만 사는 설정 `stet.captures`다. 읽을 때 숫자가 아니면 0으로 본다(그 값 때문에 쓰기가 실패하지 않는다). 도우미의 이름은 Task 6·7의 Interfaces가 정한다 (3, 6, 7) |

**스펙이 구현에 맡겨 이 계획이 정한 것.** 설치 SQL과 시작할 때의 확인은 새 패키지 `internal/undo`에 둔다(`cmd/stet`과 `internal/lab`이 함께 쓴다). 판단 기록의 두 시각은 `store.Decision`이 Go의 시각 둘(`CreatedAt`, `DoneAt`)로 들고, `Insert`가 문장을 보내기 직전에 흐른 시간으로 바꾼다. 한 키의 변경은 `(target_table, relid, key)`로 묶는다(같은 이름으로 다시 만든 테이블의 변경이 섞이지 않는다). `stet.history`의 `undo_id`는 그 변경을 `restored`나 `restored_differs`로 되돌린 마지막 되돌리기다. 결과의 줄은 `target_table`, 그 키의 첫 변경 id 순이다. 오버헤드의 수용 기준(1ms)은 프록시의 지연 테스트와 같은 모양의 테스트로 잰다.

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/config/config.go` | `undo` 절 (수정) |
| `internal/store/schema.sql` | 4장의 표·함수·트리거·권한, 5장의 컬럼, 6장의 뷰, 7장의 함수와 표 (수정) |
| `internal/store/store.go` | `Decision`의 새 필드와 삽입 (수정) |
| `internal/store/capture.go` | 캡처 설정 맞추기, 오래된 묶음 지우기 (신규) |
| `internal/proxy/session.go` | BackendKeyData의 pid를 `SessionInfo`로 (수정) |
| `internal/gate/gate.go` | 판단 기록에 pid와 결과를 받은 때 (수정) |
| `internal/apply/apply.go` | 승인된 문장 앞에 `stet.held_write` (수정) |
| `internal/undo/tables.go` | 설정의 테이블을 DB에서 확인하기 (신규) |
| `internal/undo/sql.go` | 설치·제거 SQL (신규) |
| `internal/undo/purge.go` | 한 시간마다 지우는 루프 (신규) |
| `cmd/stet/undosql.go` | `stet undo-sql` (신규) |
| `cmd/stet/main.go` | 명령 등록, 시작할 때의 맞추기·경고·지우기 (수정) |
| `internal/lab/bench/undo.go`, `cmd/stet-lab/bench.go` | 10장의 확인 (신규, 수정) |
| `stet.example.yaml`, `docs/lab.md`, `docs/spec-v0.1.md`, `docs/spec-v0.2-undo.md` | 문서 (수정) |

테스트 파일은 각 Task가 적는다. `schema.sql`의 새 부분은 파일 끝에 스펙의 장 순서로 더하고, 권한을 거두는 문장은 파일의 맨 끝에 모은다.

---

### Task 0: 계획 커밋

- [ ] **Step 1:** `git status`에 이 계획만 추적되지 않은 파일로 보여야 한다. 브랜치는 `v0.2-undo`다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`가 모든 패키지에서 `ok`인지 확인한다.
- [ ] **Step 3:** 커밋 `Plan: undo (stage 4)`.

---

### Task 1: 설정 `undo`

**Files:** Modify `internal/config/config.go`. Test `internal/config/config_test.go`.

**Interfaces — Produces:**

```go
// in Config:
Undo *Undo // nil: no undo clause; nothing of this stage runs

// Undo turns on capture for tables (spec-v0.2-undo 9). The same in both modes.
type Undo struct {
	Tables  map[string]string // table key (a key of Config.Tables, or a table name) → its key column
	MaxRows int64             // undo.max_rows: default 10000
	Keep    time.Duration     // undo.keep: default 30d
}
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestUndoAbsent`: `undo`가 없으면 `Undo == nil`.
  - `TestUndoSettings`: 스펙 9장의 YAML(게이트 대상 `contacts`, `deals`와 `public.deal_notes: {pk: id}`) → `Tables == {contacts: <contacts의 pk>, deals: <deals의 pk>, public.deal_notes: id}`, `MaxRows == 10000`, `Keep == 720h`. `max_rows: 1`, `max_rows: 1000000`, `keep: 1h`는 유효하다. 게이트 대상 키에 같은 `pk`를 적어도 유효하다. `tables`가 없는 설정(게이트 대상 테이블 없이 `undo.tables`에 `pk`를 적은 것만)도 유효하다.
  - `TestInvalidConfigs`에 행을 더한다.

| 이름 | 설정 | 에러에 들어갈 문자열 |
|---|---|---|
| undo without tables | `undo: {max_rows: 5}` | `undo.tables` |
| undo empty tables | `undo: {tables: {}}` | `undo.tables` |
| undo pk differs | `undo: {tables: {contacts: {pk: other}}}` | `undo.tables.contacts` |
| undo pk missing | `undo: {tables: {public.notes: }}` | `undo.tables.public.notes` |
| undo max_rows zero | `max_rows: 0` | `undo.max_rows` |
| undo max_rows too big | `max_rows: 1000001` | `undo.max_rows` |
| undo keep too short | `keep: 59m` | `undo.keep` |
| undo keep not a duration | `keep: forever` | `undo.keep` |
| undo unknown key | `undo: {table: {}}` | `table` |
| undo table unknown key | `undo: {tables: {contacts: {key: id}}}` | `key` |

- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/config/`
- [ ] **Step 3:** 구현한다. 기존의 알 수 없는 키 거절과 `parseDuration`(`d` 단위)을 그대로 쓴다. `pk`의 값은 다른 테이블의 `pk`와 같은 규칙으로 확인한다.
- [ ] **Step 4:** `go test -race ./internal/config/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Config: the undo clause`.

---

### Task 2: 캡처 — 표와 `stet.capture()`의 값 남기기

스펙 4장의 "저장"과 "`stet.capture()`가 하는 일" 1~7이다. 실패를 삼키는 것과 상한은 Task 3이다. 이 Task가 끝나면 캡처는 값을 남기지만 아직 쓰기를 실패시킬 수 있다.

**Files:** Modify `internal/store/schema.sql`. Create `internal/store/capture_test.go`.

**Interfaces — Produces** (`schema.sql`):

- 표 `stet.change_sets`, `stet.changes`, `stet.capture_settings`. 컬럼은 스펙 4장 그대로에 `change_sets.key_column text NOT NULL`(U1)을 더한다. 인덱스 `change_sets_role_idx (agent_role, at)`, `change_sets_backend_idx (pid, stmt_at)`, `changes_set_idx (set_id)`.
- `stet.capture() RETURNS trigger`: `SECURITY DEFINER`, Global Constraints의 일곱 `SET`. 인자는 `TG_ARGV[0]`(설정의 테이블 키), `TG_ARGV[1]`(키 컬럼).
- 테스트 헬퍼(이 파일에 두고 뒤 Task들이 쓴다): `captureDB(t) (stet *pgxpool.Pool, db string)`(자기 데이터베이스에 스키마를 올리고 `stet_md5`가 쓸 수 있게 한다), `installCapture(t, owner *pgx.Conn, table, key, pk string)`(스펙 4장의 트리거 넷), `setCapture(t, pool, roles []string, maxRows int64)`, `changesOf(t, pool, key string) []capturedChange`.

**방법.** 전이 테이블 `o`, `n`은 트리거 함수 안의 동적 SQL에서도 보인다. 컬럼 목록을 `pg_attribute`에서 읽어(`attnum > 0 AND NOT attisdropped`) 행의 값을 다음 모양으로 만든다. 한 함수가 네 트리거를 받으므로 `TG_OP`로 갈라, 그 트리거에 없는 전이 테이블을 가리키는 문장은 실행하지 않는다.

```sql
jsonb_object(ARRAY['id','name',...], ARRAY[n.id::text, n.name::text, ...])
```

- [ ] **Step 1: 실패하는 테스트** (`capture_test.go`, 모두 자기 데이터베이스).
  - `TestCaptureRecordsEachKindOfWrite`: 에이전트의 `INSERT`(2행), `UPDATE`(2행), `DELETE`(1행)가 각각 묶음 하나와 키마다 한 줄을 남긴다. `insert`는 `after`만, `delete`는 `before`만, `update`는 둘 다 있고 값이 맞다. 묶음의 `op`, `rows`, `target_table`(트리거의 첫 인자), `key_column`, `relid`, `pid`(`pg_backend_pid()`), `login_role`, `agent_role`, `app`, `query`(보낸 문장 그대로), `txid`가 맞다. `stmt_at <= at`.
  - `TestCaptureFollowsWritesStetDoesNotSee`: 뷰를 거친 `UPDATE`, `SECURITY DEFINER` 함수 안의 `UPDATE`, `INSERT ... ON CONFLICT DO UPDATE`(넣은 행은 `insert` 묶음, 고친 행은 `update` 묶음), 연쇄 삭제된 자식 행(부모와 자식 묶음의 `stmt_at`과 `query`가 같다)이 남는다.
  - `TestCaptureSkipsRolledBackAndEmptyWrites`: 롤백된 쓰기와 0행을 바꾼 `UPDATE`는 아무것도 남기지 않는다.
  - `TestCaptureOnlyForAgentRoles`: `roles = {stet_md5}`일 때 다른 계정의 쓰기는 남지 않는다. `roles`가 NULL이면 남는다. `stet.capture_settings`에 줄이 없어도 남는다.
  - `TestCaptureOfStetActingAsTheAgent`: Stet의 계정이 `SET LOCAL ROLE stet_md5`와 `set_config('stet.held_write', <uuid>, true)` 뒤에 쓴 것은 `agent_role = stet_md5`, `login_role = stet_scram`, `held_write = <uuid>`로 남는다. 에이전트 세션이 `stet.held_write`를 적고 쓴 것은 `held_write`가 NULL이다. (자기 데이터베이스 안에서 `GRANT stet_md5 TO stet_scram`이 필요하다. 테스트 끝에 거둔다.)
  - `TestCaptureKeepsValuesAsText`: 수용 기준의 타입 전부(정수, `numeric`, 실수의 `NaN`·무한대·`0.30000000000000004`, 줄바꿈·따옴표·역슬래시가 든 글자, `timestamptz`와 무한대, `date`, `interval`, `uuid`, `bytea`, NULL 원소가 든 배열과 다차원 배열, `jsonb`와 `json`의 JSON `null`과 SQL `NULL`, 중복 키가 든 `json`, NULL)을 가진 행을 넣고 지운 뒤, `before`의 값들을 그 컬럼의 타입으로 읽어 다시 넣으면 원래 행과 같다(`IS NOT DISTINCT FROM`, `json`은 글자 비교). 생성 컬럼과 `GENERATED ALWAYS AS IDENTITY` 컬럼의 값도 든다.
  - `TestCaptureIgnoresSessionOutputSettings`: 세션의 `TimeZone`, `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output`을 바꾼 연결과 기본 연결이 같은 행을 써서 남긴 값의 글자가 같다.
  - `TestCaptureSplitsAKeyChange`: 키를 바꾼 `UPDATE`는 옛 키의 줄(`before`만)과 새 키의 줄(`after`만)을 남긴다. 같은 문장에서 키가 그대로인 행은 한 줄이다.
  - `TestCaptureQuotesNames` (Review Focus 1): 스키마 `"Crm"`의 테이블 `"Deal Notes"`, 키 컬럼 `"order"`, 컬럼 `"it's"`. 값이 남는다.
  - `TestCaptureKeepsOddKeysApart` (Review Focus 3): 키가 `null`(글자), 빈 글자, `a,b`, `"q"`인 행과 값이 글자 `NULL`인 컬럼, 키가 `uuid`·`bigint`인 테이블. 키마다 한 줄이고 값이 구분된다.
- [ ] **Step 2:** 돌려서 실패를 본다. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/ -run Capture`
- [ ] **Step 3:** 구현한다. 표는 `CREATE TABLE IF NOT EXISTS`다. 인덱스는 `to_regclass`로 없는지 본 뒤에만 만든다(`DO` 블록. Task 3이 이유를 테스트한다). 함수의 `EXECUTE`는 PUBLIC에서 거둔다.
- [ ] **Step 4:** `go test -race ./internal/store/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Capture: record what an agent's write changed, per key, as text`.

---

### Task 3: 캡처 — 쓰기를 실패시키지 않기, 상한, 기록 지키기

스펙 4장의 "캡처는 쓰기를 실패시키지 않는다", "한 트랜잭션의 상한", 권한과 기록을 지키는 트리거, 11장의 "스키마의 잠금", 13장의 오버헤드다.

**Files:** Modify `internal/store/schema.sql`. Create `internal/store/capture_safety_test.go`, `internal/store/capture_latency_test.go`.

**Interfaces — Consumes:** Task 2의 표, 함수, 헬퍼. **Produces:** `stet.changes_guard() RETURNS trigger`와 두 표의 트리거(`UPDATE`와 `TRUNCATE`를 `ST004`로 거절, `DELETE`는 둔다). 캡처 함수의 완성된 동작.

**조건.**
- 1~7 전부가 한 `EXCEPTION WHEN OTHERS` 블록 안이다. 블록 밖의 코드는 실패할 수 없는 것만 한다.
- 한 트랜잭션에서 캡처가 표에 쓰는 것은 32번까지다. 값을 남기거나 표지(`too_many_rows`, `truncate`, `capture_failed`)를 남긴 것이 31번이면 다음에 쓸 것은 `too_many_statements` 표지 하나이고, 그 뒤로는 아무것도 쓰지 않는다. 실패한 캡처의 `capture_failed`도 한 번으로 센다.
- 세는 값은 `set_config('stet.captures', n, true)`다(U9). 숫자가 아닌 값은 0으로 본다. 블록이 되돌려지면 그 안의 `set_config`도 되돌려지는 것에 주의한다.
- 표에 쓰지 않는 길(캡처 대상이 아닌 세션, 0행)은 세지 않고 쓰지 않는다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestCaptureFailureKeepsTheWrite`: `stet.changes`에 어기게 될 `CHECK`를 더한 뒤 에이전트가 쓴다. 쓰기는 성공하고 행이 있고, 묶음 하나가 `skipped = 'capture_failed'`와 `detail`의 SQLSTATE(`23514`)로 남고, `stet.changes`에는 그 문장의 줄이 없다.
  - `TestCaptureFailureWhileReadingSettings`: `stet.capture_settings`의 컬럼 이름을 바꾼 뒤 에이전트가 쓴다. 쓰기는 성공하고 `capture_failed` 묶음이 남는다.
  - `TestCaptureThatCannotRecordItsFailure`: `stet.change_sets`에도 어기게 될 `CHECK`를 더한다. 쓰기는 성공하고 아무것도 남지 않는다.
  - `TestCaptureSendsTheClientNothing`: 위의 실패하는 캡처를 `client_min_messages = log`인 에이전트 연결에서 한다. 연결이 받은 알림(`OnNotice`)이 하나도 없다. `debug5`에서는 받은 알림 가운데 `stet`이 든 것이 없다.
  - `TestCaptureLeavesTimeoutsAlone`: `statement_timeout`을 넘게 만든 쓰기(캡처가 `stet.changes`의 잠금을 기다리게 한다)는 `57014`로 실패한다. `lock_timeout`에 걸린 캡처는 쓰기를 성공시키고 `capture_failed`를 남긴다.
  - `TestCaptureCapsRowsPerStatement`: `max_rows = 3`일 때 4행을 바꾼 문장은 `too_many_rows` 묶음 하나(`rows = 4`)만 남긴다. 3행은 값이 남는다. `TRUNCATE`는 `truncate` 묶음 하나(`rows` NULL)다.
  - `TestCaptureCapsStatementsPerTransaction`: 한 트랜잭션에서 한 행씩 바꾸는 쓰기 40개. 31번째까지 값이 남고, 32번째에 `too_many_statements` 묶음이 남고, 그 뒤는 없다. 그 `txid`의 묶음들의 서로 다른 `xmin`이 32개 이하다. 실패하는 캡처(`CHECK`)를 섞은 40개에서도 32개 이하다. 다음 트랜잭션은 다시 0에서 센다. 캡처 대상이 아닌 계정의 쓰기 40개는 Stet의 표에 한 줄도 남기지 않는다.
  - `TestAgentCannotBreakCaptureThroughSessionSettings` (Review Focus 2): 에이전트가 `SET stet.captures = 'x'`, `= '999999999999'`, `SET stet.held_write = 'not-a-uuid'`를 하고 쓴다. 모두 성공한다. `SET ROLE`로 다른 역할이 된 에이전트 접속의 쓰기는 `agent_role = stet_md5`로 남는다.
  - `TestChangeRecordsAreImmutable`: Stet의 계정이 `stet.change_sets`와 `stet.changes`를 `UPDATE`하거나 `TRUNCATE`하면 `ST004`다. `DELETE`는 되고 줄이 연쇄로 지워진다.
  - `TestAgentCannotReachTheRecords`: `stet_md5`가 두 표를 읽거나 쓰면, `stet.capture()`를 부르면 `42501`이다.
  - `TestTableOwnerNeedsGrantsToInstall`: 테스트가 만든 소유자 계정이 권한 없이 트리거를 만들면 `42501`이다. `GRANT USAGE ON SCHEMA stet`만으로도 `42501`이다. `GRANT EXECUTE ON FUNCTION stet.capture()`까지 주면 된다. 그 계정이 `SELECT stet.capture()`를 불러도 값을 얻지 못한다(트리거로만 불린다는 에러).
  - `TestMigrateDoesNotWaitForAnOpenCapture`: 에이전트가 캡처된 쓰기를 한 트랜잭션을 열어 둔다. `stet.schema_version`의 값을 바꾸고 `Migrate`를 `lock_timeout` 2초로 다시 돌린다(기존 `migrateWithLockTimeout`의 모양). 기다리지 않고 끝난다.
  - `TestCaptureAddsUnderAMillisecond` (`capture_latency_test.go`, `-short`에서는 건너뛴다): 14개 컬럼의 테이블에서 키로 한 행을 고치는 `UPDATE`를 트리거 없는 테이블과 캡처하는 테이블에 번갈아 2,000번씩 보낸다(`internal/proxy/latency_test.go`의 모양). p95의 차이가 1ms 이하다. 세 숫자(없음, 에이전트 아님, 캡처)의 평균과 p95를 `t.Logf`로 남긴다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. 두 표의 트리거는 `pg_trigger`에 없을 때만 만든다(`DO` 블록).
- [ ] **Step 4:** `go test -race ./internal/store/`와 전체 명령이 통과한다. 지연 테스트를 세 번 돌려 숫자를 보고에 적는다.
- [ ] **Step 5:** 커밋 `Capture: never fail the agent's write, cap what one statement and one transaction record`.

---

### Task 4: 판단 기록에 백엔드 pid와 시간 창, 승인된 쓰기의 `stet.held_write`

스펙 5장의 컬럼과 4장 7, 8장의 마지막 줄이다.

**Files:** Modify `internal/store/schema.sql`, `internal/store/store.go`, `internal/proxy/session.go`, `internal/gate/gate.go`, `internal/apply/apply.go`. Test `internal/store/store_test.go`, `internal/proxy/gate_test.go`, `internal/gate/gate_test.go`, `internal/apply/apply_test.go`.

**Interfaces — Produces:**

```go
// proxy.SessionInfo
// BackendPID is the server process of the session, from its BackendKeyData;
// 0 until the server sent it.
BackendPID uint32

// store.Decision
BackendPID *uint32   // the session's server process; nil when unknown or the write was not sent
DoneAt     time.Time // when the gate was told the outcome; zero when the write was not sent
```

- `stet.decisions`의 컬럼 `backend_pid integer`, `sent_after timestamptz`, `done_before timestamptz`와 인덱스 `decisions_backend_idx (backend_pid, sent_after)`.
- `Store.Insert`: `BackendPID`가 있고 `DoneAt`이 0이 아니면 `sent_after = clock_timestamp() - (CreatedAt부터 흐른 시간)`, `done_before = clock_timestamp() - (DoneAt부터 흐른 시간)`으로 적는다. 흐른 시간은 `Exec` 직전에 재고 마이크로초로 보낸다. 아니면 셋 다 NULL이다.
- 게이트: `End`가 결과를 받은 때를 `pending`에 적고, `judge`가 보낸 쓰기(`p.refused == nil`)이고 pid를 아는 세션일 때만 두 필드를 채운다.
- 실행부: `become`이 `select set_config('stet.held_write', <보류 줄의 id>, true)`를 한다.

- [ ] **Step 1: 실패하는 테스트.**
  - store `TestInsertRecordsTheBackendWindow`: `CreatedAt = 지금-300ms`, `DoneAt = 지금-100ms`, `BackendPID = 4242`인 판단을 넣는다. DB의 `clock_timestamp()`를 넣기 전후로 읽어, `sent_after`가 그 사이에서 300ms를 뺀 범위(±50ms)에, `done_before`가 100ms를 뺀 범위에 있고 `sent_after < done_before`다. `BackendPID`가 nil이거나 `DoneAt`이 0이면 세 컬럼이 NULL이다.
  - proxy `TestGateIsToldTheBackendPID`: 프록시를 지난 세션에서 `select pg_backend_pid()`가 돌려준 값과 그 세션의 쓰기에서 게이트가 받은 `SessionInfo.BackendPID`가 같다. 섀도와 enforce(`Policy.Enforce`) 모두.
  - gate `TestDecisionCarriesTheBackendWindow`: 실행된 쓰기의 판단 기록에 `backend_pid`와 두 시각이 있다. enforce에서 거절된 쓰기는 셋 다 NULL이다.
  - apply `TestApprovedStatementSeesItsHeldWrite`: 승인된 문장 `update ... set note = current_setting('stet.held_write', true)`가 그 줄의 id를 쓴다. 같은 연결의 다음 트랜잭션에서는 그 설정이 비어 있다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. 프록시는 서버 쪽 탭의 `want`에 `'K'`를 두 모드 모두 더하고, 본문의 앞 4바이트를 읽는다. 메시지는 고치지 않는다.
- [ ] **Step 4:** 전체 명령과 `go test -race ./internal/proxy/ -run Latency`가 통과한다.
- [ ] **Step 5:** 커밋 `Decisions: record the backend pid and when the write was on the server`.

---

### Task 5: `stet.history`와 잇는 규칙

스펙 5장의 규칙과 6장이다.

**Files:** Modify `internal/store/schema.sql`. Create `internal/store/history_test.go`, `cmd/stet/history_test.go`.

**Interfaces — Consumes:** Task 2의 표, Task 4의 컬럼과 `stet.held_write`. **Produces:** 뷰 `stet.history`(컬럼은 스펙 6장의 표 그대로, 그 순서). `undo_id`와 `undone_at`은 Task 7이 채운다. 이 Task에서는 NULL 컬럼으로 둔다(`CREATE OR REPLACE VIEW`가 받도록 타입은 `bigint`, `timestamptz`).

- [ ] **Step 1: 실패하는 테스트.**
  - store `history_test.go`(판단 기록을 손으로 넣는다. 자기 데이터베이스):
    - `TestHistoryLinksASetToItsOnlyDecision`: pid·문장·시간 창이 맞는 판단 기록이 하나면 `decision_id`, `verdict`, `reasons`가 나오고 `decision_candidates = 1`이다.
    - `TestHistoryLinkBounds`: `stmt_at`이 `sent_after - 10ms`보다 이르거나 `done_before`보다 늦으면, pid가 다르면, 문장의 글자가 다르면, `executed`가 거짓이면 잇지 않는다(`decision_candidates = 0`). `sent_after - 5ms`는 잇는다.
    - `TestHistoryNarrowsByTargetKey`: 후보가 둘이고 `target_pk`가 서로 다른 행이면 각 묶음이 자기 키의 판단 기록에 이어진다. 둘 다 같은 키이면 `decision_id`가 NULL이고 `decision_candidates = 2`다. `target_pk`가 없는 후보 둘도 같다.
    - `TestHistoryShowsSkippedSets`: 값을 남기지 않은 묶음은 `key`가 NULL인 한 줄로 나온다.
    - `TestHistoryIsReadWithTheOwnersRights`: `USAGE`와 뷰의 `SELECT`만 받은 계정이 뷰를 읽는다. 받지 않은 계정은 `42501`이다.
  - cmd `history_test.go`(`start(t, cfg)`로 Stet을 띄운다. 모델 어댑터는 `none`):
    - `TestChangeIsLinkedToItsDecision`: 섀도에서 프록시를 지난 쓰기의 변경이 그 판단 기록에 이어진다. 단순 프로토콜, 확장 프로토콜(바인딩 값), 명시적 트랜잭션 안의 쓰기 각각.
    - `TestCascadedChildIsLinkedToTheParentsDecision`: 연쇄로 지워진 자식의 묶음이 부모의 문장의 판단 기록에 이어진다.
    - `TestWriteThroughAFunctionIsNotLinked`: 함수를 거친 쓰기의 변경은 `decision_candidates = 0`이다.
    - `TestBatchedWritesLinkToTheirOwnDecisions`: 한 Sync 아래 같은 글자로 서로 다른 행을 고친 쓰기 둘(`pgx.Batch`)은 각자의 판단 기록에 이어진다. 같은 행을 고친 둘은 이어진 것이 있으면 그 판단 기록의 `created_at` 순서가 변경의 순서와 같거나, 둘 다 `decision_candidates = 2`다. 다른 행의 판단 기록에 이어진 것이 없다.
    - `TestApprovedWriteIsLinkedThroughItsHeldWrite`: enforce에서 보류된 쓰기를 승인하면 그 변경의 `agent_role`이 에이전트이고, `held_write`가 그 줄이고, `decision_id`가 원래의 판단 기록이다.
    - `TestStatementTextMatchesWhatTheServerSaw`: 주석과 공백이 든 문장, 여러 줄 문장에서 `stet.decisions.statement`와 `stet.change_sets.query`가 같은 글자다(두 프로토콜).
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. 묶음마다 후보를 `LATERAL`로 모으고, 좁힌 뒤 하나일 때만 잇는다. `held_write`가 있는 묶음은 `stet.held_writes.decision_id`로 잇는다.
- [ ] **Step 4:** 전체 명령이 통과한다. 글자가 다르게 남는 경로를 찾았으면 고치지 않고 보고에 적는다(스펙 5장의 마지막 줄).
- [ ] **Step 5:** 커밋 `History: show each change with the decision that judged its write`.

---

### Task 6: `stet.undo_plan`

스펙 7장의 "고르기", "키마다 하나의 결정", 경고, 거절, 권한이다. 아무것도 바꾸지 않는 쪽이다.

**Files:** Modify `internal/store/schema.sql`. Create `internal/store/undo_plan_test.go`.

**Interfaces — Consumes:** Task 2·3의 표. **Produces** (Task 7이 쓴다):

```sql
stet.undo_plan(agent_role text, since timestamptz, until timestamptz DEFAULT NULL, tables text[] DEFAULT NULL)
  RETURNS TABLE (target_table text, key text, action text, outcome text, reason text, warning text, change_ids bigint[])
  -- SECURITY INVOKER, 일곱 SET. 읽기만 한다.

-- 도우미. 이름은 이대로 둔다. 모두 일곱 SET을 갖고, EXECUTE를 PUBLIC에서 거둔다.
stet.undo_selection(agent_role text, since timestamptz, until timestamptz, tables text[])
  -- SECURITY DEFINER. 고른 변경을 (target_table, relid, key)로 묶은 줄과, 값을 남기지 않은 묶음마다 한 줄(key NULL).
  -- 돌려주는 것: target_table, relid, key_column, key, b jsonb, a jsonb, change_ids bigint[], first_change bigint,
  --            key_changed boolean, interleaved boolean, skipped text, also_changed text[] (같은 문장이 바꾼, 고르지 않은 테이블)
stet.undo_row(relid oid, key_column text, key text, lock boolean) RETURNS jsonb
  -- SECURITY INVOKER. 지금 그 키의 행을 캡처와 같은 글자로. 없으면 NULL. lock이면 FOR UPDATE.
stet.undo_judge(relid oid, key_column text, key text, b jsonb, a jsonb, key_changed boolean, interleaved boolean, lock boolean)
  RETURNS TABLE (action text, reason text)   -- SECURITY INVOKER. 스펙의 여섯 확인을 순서대로. reason이 NULL이면 action대로 한다
stet.undo_warning(relid oid, action text, also_changed text[]) RETURNS text   -- children_not_captured, not_selected
stet.undo_refuse(agent_role text, since timestamptz, until timestamptz) RETURNS void   -- ST006의 네 거절
```

- 권한 예시의 문장(검토자에게 줄 `GRANT`들)을 `schema.sql`의 주석에 적는다. `stet.undo_plan`만 쓰는 계정에는 `undo_plan`, `undo_selection`, `undo_row`, `undo_judge`, `undo_warning`, `undo_refuse`의 `EXECUTE`가 필요하다.
- 테스트 헬퍼: `undoReviewer(t, db string, tables ...string) *pgx.Conn`(검토자 계정을 만들고 권한을 준 연결), `planOf(t, conn, role string, args ...any) []undoRow`.

- [ ] **Step 1: 실패하는 테스트** (자기 데이터베이스. 에이전트의 쓰기는 `stet_md5`로 실제로 한다).
  - `TestPlanNamesTheActionPerKey`: 에이전트가 넣은 행은 `delete`, 지운 행은 `insert`, 고친 행은 `update`이고 `outcome = would_restore`다. `change_ids`가 그 키의 변경들이다. 줄의 순서는 `target_table`, 첫 변경 id다.
  - `TestPlanFoldsSeveralChangesOfAKey`: 한 키를 세 번 고쳤으면 한 줄이다. 넣고 고친 행은 `delete`, 고치고 지운 행은 `insert`, 넣고 지운 행은 `no_net_change`로 건너뛴다. 고쳤다가 원래 값으로 돌려놓은 행도 `no_net_change`다.
  - `TestPlanSelectsByRoleTimeAndTable`: 다른 계정의 변경, `since` 앞과 `until`부터의 변경, `tables`에 없는 테이블의 변경은 나오지 않는다. `until`이 NULL이면 지금까지다.
  - `TestPlanSkipsWhatOthersChanged`: 그 뒤에 다른 계정이 고친 행은 `changed_since`다. 다른 계정이 지운 행, 에이전트가 지운 뒤 다른 계정이 다시 넣은 행도 `changed_since`다. 에이전트의 두 변경 사이에 다른 계정이 고친 행은 `interleaved`다. 다른 계정이 고쳤다가 같은 값으로 돌려놓은 행은 `would_restore`다.
  - `TestPlanSkipsAKeyChange`: 키를 바꾼 `UPDATE`의 옛 키와 새 키가 `key_changed`로 나온다.
  - `TestPlanSkipsWhatNoLongerFitsTheTable` (Review Focus 4): 컬럼을 더한, 지운, 이름을 바꾼 테이블의 변경은 `columns_changed`다. 지우고 같은 이름으로 다시 만든 테이블의 변경은 `table_gone`이다. 이름만 바꾼 테이블의 변경은 `would_restore`다.
  - `TestPlanCheckOrder`: 여러 확인에 함께 걸리는 키는 스펙의 순서에서 앞선 이유로 나온다(`key_changed`가 `changed_since`보다, `interleaved`가 `changed_since`보다, `changed_since`가 `no_net_change`보다 앞).
  - `TestPlanShowsWhatWasNotCaptured`: 고른 범위의 `too_many_rows`, `truncate`, `capture_failed`, `too_many_statements` 묶음이 `key` 없이 `outcome = not_captured`, `reason = <skipped>`로 나온다.
  - `TestPlanWarnsAboutUncapturedChildren`: 연쇄 외래 키의 자식 테이블에 캡처 트리거가 없으면 부모의 `insert` 줄에 `children_not_captured: <자식>`이 나온다. 트리거가 있으면 없다.
  - `TestPlanWarnsAboutUnselectedTables`: 부모와 자식이 함께 지워졌는데 `tables`로 부모만 고르면 부모의 줄에 `not_selected: <자식의 키>`가 나온다.
  - `TestPlanChangesNothing`: 계획을 부른 뒤 고객 테이블과 Stet의 표가 그대로다. 행을 잠그지 않는다(다른 연결이 `FOR UPDATE NOWAIT`로 그 행을 잡는다).
  - `TestPlanRefusals`: `agent_role`이 NULL, `since`가 NULL, `until <= since`, `session_user = agent_role`이 각각 `ST006`이다.
  - `TestPlanNeedsGrants`: 권한을 받지 않은 계정은 `42501`이다. `stet.change_sets`를 직접 읽을 권한 없이 계획이 된다. 고객 테이블의 `SELECT`가 없는 검토자에게는 에러가 나지 않고, 그 테이블의 키가 `outcome = skipped`와 `SQLSTATE 42501: …`인 `reason`으로 나온다.
  - `TestPlanIgnoresTheCallersSearchPath`: 검토자의 `search_path` 맨 앞 스키마에 `jsonb = jsonb` 연산자와 `format`, `jsonb_object` 같은 이름의 함수를 둔다(언제나 참이나 다른 값을 돌려준다). 계획의 결과가 그것이 없을 때와 같다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. 지금 행의 값은 Task 2와 같은 식으로 만든다(같은 `SET` 아래이므로 같은 글자다). 식을 만드는 부분은 캡처와 한 도우미로 묶을 수 있으면 묶는다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Undo: plan what restoring an agent's changes would do`.

---

### Task 7: `stet.undo`

스펙 7장의 `stet.undo`, `referenced`, 다시 해 보기, 기록이다.

**Files:** Modify `internal/store/schema.sql`. Create `internal/store/undo_test.go`.

**Interfaces — Consumes:** Task 6의 도우미. **Produces:**

```sql
stet.undo(agent_role text, since timestamptz, until timestamptz DEFAULT NULL, tables text[] DEFAULT NULL, note text DEFAULT NULL)
  RETURNS TABLE (target_table text, key text, action text, outcome text, reason text, warning text, change_ids bigint[])
  -- SECURITY INVOKER. SET 절이 없다: 복구 문장이 부른 세션의 설정으로 돈다.

stet.undos (id bigint identity PK, at timestamptz, by text, agent_role text, since timestamptz, until timestamptz,
            tables text[], note text, restored integer, skipped integer, failed integer)
stet.undo_items (undo_id bigint → stet.undos, target_table text, key text, action text, outcome text, reason text,
                 warning text, change_ids bigint[])

-- 도우미 (일곱 SET, PUBLIC에서 거둔다)
stet.undo_statement(relid oid, key_column text, key text, action text, b jsonb, a jsonb) RETURNS text
  -- SECURITY INVOKER. 실행할 복구 문장의 글자. 이름은 스키마까지, 연산자는 OPERATOR(pg_catalog.=), 값은 글자 리터럴을 컬럼의 타입으로
stet.undo_referenced(relid oid, action text, b jsonb, a jsonb) RETURNS text
  -- SECURITY INVOKER. 복구 문장이 없애거나 바꾸는 값을 연쇄 외래 키로 가리키는 행이 있는 테이블들. 없으면 NULL
stet.undo_record(agent_role text, since timestamptz, until timestamptz, tables text[], note text, items jsonb) RETURNS bigint
  -- SECURITY DEFINER. stet.undos와 stet.undo_items에 적는다. by는 session_user다. 인자로 받지 않는다
stet.undo_failure(sqlstate text, message text) RETURNS text   -- 'SQLSTATE <코드>: <메시지>'
```

- `stet.history`의 `undo_id`, `undone_at`을 채운다(인덱스로 찾을 수 있게 한다).

**조건.**
- `stet.undo`의 몸통에는 연산자를 맨이름으로 쓰지 않고(`OPERATOR(pg_catalog.…)`), 함수와 타입은 스키마까지 적는다. 판단과 글자 만들기는 전부 도우미가 한다. 몸통이 하는 일은 도우미를 부르고, 받은 문장을 `EXECUTE`하고, 키마다의 블록에서 에러를 잡는 것이다.
- 키마다: 블록 안에서 `undo_judge(..., lock => true)` → `undo_referenced` → `EXECUTE undo_statement(...)` → `undo_row`로 다시 읽어 B와 비교. 블록의 에러는 `failed`다.
- 한 바퀴 뒤에 `23503`, `23505`, `referenced`로 되지 않은 키만 다시 한다. 한 바퀴에서 `restored`나 `restored_differs`가 하나도 늘지 않으면 그만둔다. 첫 바퀴의 순서는 `delete`를 나중 변경부터, 그다음 `update`, `insert`를 앞선 변경부터로 한다(다시 해 보기를 줄이려는 것이고, 맞는지는 다시 해 보기가 보장한다).
- `update`는 A와 B가 다른 컬럼만 `SET`한다(U3). 생성 컬럼과 `GENERATED ALWAYS AS IDENTITY` 컬럼은 `SET`하지 않는다. `insert`는 생성 컬럼을 빼고 모두 주며 `OVERRIDING SYSTEM VALUE`를 쓴다.
- 인자의 거절(`stet.undo_refuse`) 말고는 에러를 내지 않는다.

- [ ] **Step 1: 실패하는 테스트** (자기 데이터베이스).
  - `TestUndoRestoresEachKindOfWrite`: 에이전트의 `INSERT`가 지워지고, `DELETE`가 다시 들어오고, `UPDATE`가 이전 값으로 돌아간다. 한 키를 여러 번 바꿨으면 처음 값이다. `outcome = restored`이고 테이블의 내용이 에이전트의 쓰기 전과 같다(Task 2의 타입들을 가진 테이블).
  - `TestUndoWithIdentityAndGeneratedColumns`: `GENERATED ALWAYS AS IDENTITY` 키와 생성 컬럼이 있는 테이블에서 셋 다 되고, 다시 넣은 행의 identity 값이 원래 값이다.
  - `TestUndoLeavesWhatOthersChanged`: `changed_since`와 `interleaved`의 행은 건너뛰고 그대로다. 다른 키는 되돌려진다.
  - `TestUndoWaitsForAConcurrentChangeAndSkips`: 다른 연결이 그 행을 고치고 커밋하지 않은 동안 `stet.undo`를 부른다. 기다리다가, 그쪽이 커밋하면 그 키는 `changed_since`이고 그쪽의 값이 남는다. `REPEATABLE READ`의 검토자에서는 그 키가 `failed`(`SQLSTATE 40001`)이고 값이 남는다.
  - `TestUndoRestoresParentAndChild`: 연쇄로 함께 지워진 부모와 자식이 둘 다 돌아온다. 에이전트가 함께 만든 부모와 자식이 둘 다 지워진다.
  - `TestUndoOfParentOnly`: 자식 테이블에 트리거가 없으면 부모만 돌아오고 `children_not_captured`가 나온다. `tables`로 부모만 고르면 부모만 돌아오고 `not_selected`가 나온다.
  - `TestUndoRestoresSwappedUniqueValues`: 유일 제약의 값을 맞바꾼 두 행이 둘 다 돌아온다. 셋이 돌려 가며 바꾼 것은 `failed`(`SQLSTATE 23505`)로 남고 행은 그대로다.
  - `TestUndoSkipsReferencedRows`: 에이전트가 만든 행에 다른 계정이 `ON DELETE CASCADE`의 자식을 붙였으면 `skipped`와 `referenced: <자식 테이블>`이고 부모도 자식도 그대로다. `SET NULL`도 같다. `update`가 참조되는 컬럼을 바꾸는데 `ON UPDATE CASCADE`의 자식이 있을 때도 같다. `NO ACTION` 외래 키의 자식이 있으면 `failed`(`SQLSTATE 23503`)다.
  - `TestUndoKeepsGoingAfterAFailure`: 복구 문장이 실패하는 키(고객의 트리거가 에러를 낸다)는 `failed`와 `SQLSTATE …`이고 다른 키는 되돌려진다. 실패한 키의 행은 그대로다.
  - `TestUndoReportsWhenTheRowDiffers`: 고객의 `BEFORE UPDATE` 트리거가 값을 바꾸면 `restored_differs`다.
  - `TestUndoRunsUnderTheCallersSettings`: 고객의 트리거 함수가 스키마 없는 이름으로 다른 테이블에 쓴다. 검토자의 `search_path`로 그 이름이 풀려 되돌리기가 된다. 검토자의 `TimeZone`이 `Asia/Seoul`이고 `DateStyle`이 `German`이어도 `timestamptz`·`date` 값이 그대로 돌아온다.
  - `TestUndoIgnoresTheCallersSearchPath`: Task 6의 가로채는 스키마를 둔 채 되돌린다. 결과와 테이블의 내용이 그것이 없을 때와 같다.
  - `TestUndoMatchesThePlan`: 여러 종류가 섞인 변경에서 `stet.undo_plan`의 `(target_table, key, action)`과 바로 이어 부른 `stet.undo`의 것이 같다.
  - `TestUndoIsTheReviewersTransaction`: 검토자가 롤백하면 테이블도 `stet.undos`도 `stet.undo_items`도 그대로다. 커밋하면 `stet.undos`에 한 줄(`by`가 검토자, 고른 조건, `note`, 세 수)과 결과의 줄마다 `stet.undo_items`가 남고, `stet.history`의 그 변경들에 `undo_id`와 `undone_at`이 보인다.
  - `TestUndoTwice`: 두 번째는 모든 키가 `changed_since`이고 아무것도 바꾸지 않는다.
  - `TestUndoNeedsRightsOnTheTable`: 테이블에 쓰기 권한이 없는 검토자의 키는 `failed`(`SQLSTATE 42501`)다. 자식 테이블을 읽을 권한이 없을 때도 같다.
  - `TestUndoGrantsAreSeparate`: `stet.undo_plan`의 권한만 받은 계정은 `stet.undo`와 `stet.undo_record`를 부르지 못한다(`42501`). `stet.undo_record`를 직접 부른 검토자의 기록은 `by`가 자기 이름이다.
  - `TestUndoRefusals`: Task 6의 네 거절이 `ST006`이다.
  - `TestUndoQuotesNames` (Review Focus 1): Task 2의 이름들을 가진 테이블에서 셋 다 된다.
  - `TestUndoRestoresOddKeys` (Review Focus 3): Task 2의 키들이 각자 돌아온다.
  - `TestUndoOfManyKeys` (Review Focus 5): 에이전트가 한 문장으로 고친 5,000행을 한 번에 되돌린다. 5,000줄이 `restored`이고 30초 안에 끝난다.
  - `TestUndoOfNothing` (Review Focus 5): `tables`에 없는 이름, 미래의 `since`는 빈 결과다. `stet.undos`에는 한 줄이 남고 세 수가 0이다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Undo: restore an agent's changes in the reviewer's transaction`.

---

### Task 8: `internal/undo`와 `stet undo-sql`

스펙 9장의 명령과, 시작할 때 경고할 것을 찾는 부분이다.

**Files:** Create `internal/undo/tables.go`, `internal/undo/sql.go`, `internal/undo/undo_test.go`, `cmd/stet/undosql.go`, `cmd/stet/undosql_test.go`. Modify `cmd/stet/main.go`(명령 등록과 `usage`).

**Interfaces — Produces:**

```go
package undo

// Table is one table of undo.tables as the database has it.
type Table struct {
	Key, PK             string   // the config key and the key column
	Schema, Name, Owner string   // "" when the table was not found
	OID                 uint32
	Problem             string   // why capture cannot be installed on it; "" when it can
	Triggers            []string // what is wrong with its capture triggers, one line each; empty when all four are right
	Children            []string // schema-qualified tables that reference it with a cascading foreign key and are not in undo.tables
}

// Inspect looks up every table of cfg.Undo, sorted by key. Names resolve as the gate resolves them.
func Inspect(ctx context.Context, q store.Querier, cfg *config.Config) ([]Table, error)

// InstallSQL is what `stet undo-sql` prints: the grants for stetRole to run, then one transaction per table owner.
// It fails, naming each table and its Problem, when any table has one.
func InstallSQL(stetRole string, ts []Table) (string, error)

// DropSQL is what `stet undo-sql -drop` prints.
func DropSQL(ts []Table) string

// Warnings are the lines Stet logs at startup (spec 9).
func Warnings(ts []Table) []string
```

```go
// cmd/stet
func runUndoSQL(args []string, out io.Writer) error
```

- `Problem`의 경우: 찾지 못했다, 보통 테이블이 아니다(뷰, 파티션 테이블, 파티션, 외부 테이블), 상속 자식이 있다, 키 컬럼이 없다, 키 컬럼이 그 테이블의 기본 키도 `NOT NULL`인 한 컬럼 유일 제약도 아니다(부분 인덱스와 식 인덱스는 유일로 보지 않는다).
- `Triggers`의 경우: 넷 가운데 없는 것, 꺼진 것(`tgenabled = 'D'`), `stet.capture`가 아닌 함수를 부르는 것, 인자가 설정의 키·키 컬럼과 다른 것.
- 출력의 모양. 주석은 영어다. 소유자가 Stet의 계정이면 그 소유자의 `GRANT`는 내지 않는다.

```sql
-- 1. Run as stet_scram (the account Stet connects as).
GRANT USAGE ON SCHEMA stet TO crm_owner;
GRANT EXECUTE ON FUNCTION stet.capture() TO crm_owner;

-- 2. Run as crm_owner (the owner of public.deals, public.deal_notes).
BEGIN;
SET LOCAL lock_timeout = '5s';
DROP TRIGGER IF EXISTS stet_capture_ins ON public.deals;
CREATE TRIGGER stet_capture_ins AFTER INSERT ON public.deals
  REFERENCING NEW TABLE AS n
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
-- ... the other three, then the next table
COMMIT;
-- note: public.deal_tags references public.deals with ON DELETE CASCADE and is not under undo.tables:
--       rows of it deleted along with a deal are not captured and will not come back.
```

- [ ] **Step 1: 실패하는 테스트.**
  - undo `TestInspectFindsTablesAndProblems`: `Problem`의 경우마다 하나씩, 그리고 문제없는 테이블(기본 키, `NOT NULL` 유일 컬럼).
  - undo `TestInspectReportsTriggerState`: 트리거가 없는 테이블은 넷 다 없다고 한다. 하나를 끄거나, 인자를 다르게 만들거나, 하나를 지우면 그것을 말한다. 넷이 맞으면 비어 있다.
  - undo `TestInspectFindsUncapturedChildren`: 연쇄 외래 키의 자식이 `undo.tables`에 없으면 `Children`에 나오고, 있으면 없다. `NO ACTION`의 자식은 나오지 않는다.
  - undo `TestInstallSQLRuns`: 출력의 첫 묶음을 Stet의 계정으로, 둘째를 테이블 소유자(테스트가 만든 계정)로 실행하면 트리거 넷이 생기고 `Inspect`의 `Triggers`가 빈다. 한 번 더 실행해도 된다. `DropSQL`의 출력을 실행하면 넷이 없어진다. 에이전트의 쓰기가 실제로 남는다.
  - undo `TestInstallSQLRefusesWhatItCannotCapture`: `Problem`이 있는 테이블이 하나라도 있으면 에러이고 SQL이 없다. 에러가 그 테이블과 이유를 말한다.
  - undo `TestInstallSQLQuotesNames` (Review Focus 1): 스키마 `"Crm"`의 `"Deal Notes"`, 키 컬럼 `"order"`, 설정 키에 따옴표가 든 이름, 대문자가 든 소유자 이름. 출력이 그대로 실행된다.
  - undo `TestInstallSQLTimesOutOnALockedTable`: 다른 연결이 그 테이블에 쓰는 트랜잭션을 열어 둔 동안 둘째 묶음을 실행하면 `55P03`으로 실패하고 트리거가 하나도 생기지 않는다. (테스트는 `lock_timeout`의 값을 출력에서 읽어 확인하고, 기다림을 줄이려고 실행할 때는 `1s`로 바꿔 넣는다.)
  - undo `TestWarnings`: 찾지 못한 테이블, 트리거의 문제(`stet undo-sql`을 실행하라는 말이 든다), 캡처하지 않는 자식이 각각 한 줄이다.
  - cmd `TestUndoSQLCommand`: `runUndoSQL`이 설정 파일을 읽어 표준 출력에 SQL을 쓰고, `-drop`이면 지우는 SQL을 쓴다. 데이터베이스는 바뀌지 않는다. `undo` 절이 없는 설정, 받지 않는 테이블, `stet` 스키마가 없는 데이터베이스(U8)는 에러다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. 이름의 풀이는 게이트의 `ident(key)`와 같은 방법(Stet의 `search_path`)이다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Add stet undo-sql: print the triggers a table owner installs`.

---

### Task 9: 시작할 때 — 캡처 설정 맞추기, 경고, 오래된 기록 지우기

스펙 9장의 "Stet이 시작할 때"다.

**Files:** Create `internal/store/capture.go`, `internal/undo/purge.go`. Modify `cmd/stet/main.go`. Test `internal/store/capture_test.go`, `internal/undo/undo_test.go`, `cmd/stet/undo_test.go`(신규).

**Interfaces — Consumes:** Task 8의 `undo.Inspect`, `undo.Warnings`. **Produces:**

```go
// store
// SyncCaptureSettings makes stet.capture_settings say roles (nil or empty: every session) and maxRows.
// It writes only when the row differs.
func (s *Store) SyncCaptureSettings(ctx context.Context, roles []string, maxRows int64) error

// PurgeChanges deletes up to limit change sets recorded more than keep ago, oldest first, with their
// changes, and reports how many sets it deleted.
func (s *Store) PurgeChanges(ctx context.Context, keep time.Duration, limit int) (int64, error)

// undo
type Purger struct{ /* ... */ }
func NewPurger(st *store.Store, keep time.Duration, log *slog.Logger) *Purger
func (p *Purger) Start() // purges at once, then every hour, 10,000 sets at a time until fewer are deleted
func (p *Purger) Close() // stops and waits for a purge in progress
```

- `cmd/stet`: `undo`가 있으면 게이트 대상 테이블이 없어도 풀을 열고 `Migrate`한다(U7). `SyncCaptureSettings(cfg.AgentRoles, cfg.Undo.MaxRows)`, `undo.Inspect`와 `undo.Warnings`의 줄을 `log.Warn`, `Purger`의 시작. 확인하다 난 에러는 "could not check"로 경고하고 시작한다. 멈출 때는 게이트, 실행부, `Purger`, 풀의 순서다.
- `PurgeChanges`의 기준 시각은 DB의 시계다. 가장 오래된 묶음부터 `limit`개를 보고 그 가운데 `keep`보다 오래된 것만 지운다(`id` 순으로 읽으므로 지울 것이 없을 때 표 전체를 읽지 않는다).

- [ ] **Step 1: 실패하는 테스트.**
  - store `TestSyncCaptureSettings`: 줄이 없을 때 넣고, 값이 다르면 고치고, 같으면 쓰지 않는다(`xmin`이 그대로다). `roles`가 비면 NULL이다.
  - store `TestPurgeChanges`: `at`이 `keep`보다 오래된 묶음(슈퍼유저로 넣는다)과 그 줄들이 지워지고 새 묶음은 남는다. `limit`보다 많으면 `limit`개만 지우고 그 수를 돌려준다. `stet.undos`와 `stet.undo_items`는 그대로다.
  - undo `TestPurgerPurgesAtStartAndStops`: `Start` 뒤 곧 오래된 묶음이 없어진다. 25,000개가 있으면(한 번에 10,000) 한 번의 깨어남에서 모두 지워진다. `Close`가 돌아온 뒤에는 지우지 않는다.
  - cmd `TestRunSyncsCaptureSettings`: `agents.roles: [stet_md5]`, `undo.max_rows: 7`로 띄우면 `stet.capture_settings`가 그 값이다. 섀도와 enforce 모두.
  - cmd `TestRunWarnsAboutUndoTables`: 트리거가 없는 테이블, 없는 테이블, 캡처하지 않는 자식이 로그에 경고로 나오고 Stet은 시작한다.
  - cmd `TestRunWithUndoButNoGatedTables` (U7): 게이트 대상 테이블 없이 `undo.tables`만 있는 설정으로 띄우면 스키마가 올라가고 설정이 맞춰지고, 세션은 지금처럼 그대로 지나간다.
  - cmd `TestRunWithoutUndoLeavesCaptureSettingsAlone`: `undo`가 없으면 `stet.capture_settings`를 건드리지 않는다.
  - cmd `TestUndoEndToEnd`: 섀도로 띄운 Stet을 지나 에이전트가 넣고, 고치고, 지운다. 검토자가 `stet.history`에서 판단 기록에 이어진 변경을 보고, `stet.undo_plan`과 `stet.undo`를 부르고 커밋하면 테이블이 처음과 같다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Start-up: sync the capture settings, warn about undo tables, purge old changes`.

---

### Task 10: 벤치마크 스트림으로 끝에서 끝까지 확인

스펙 10장과 13장의 끝에서 둘째·셋째 줄이다. 모델을 부르지 않는다.

**Files:** Create `internal/lab/bench/undo.go`, `internal/lab/bench/undo_test.go`. Modify `cmd/stet-lab/bench.go`. 결과는 `docs/lab.md`의 새 절에 적는다(Task 11이 다듬는다).

**Interfaces — Consumes:** `undo.Inspect`, `undo.InstallSQL`, `store.Migrate`, `store.SyncCaptureSettings`, 벤치마크의 기존 `Clone`, `LoadStream`, 보내는 함수들. **Produces:** `stet-lab bench undo-check`(이름과 플래그는 기존 `bench` 하위 명령의 모양을 따른다)와 그 결과.

**하는 일.**
1. 벤치마크 템플릿을 복제한다. 서버에 이미 있는 템플릿 데이터베이스와 역할을 읽기만 한다. 없으면 `bench prepare`와 같은 방법으로 자기 접두어의 것을 만든다. 자기가 만든 복제본만 지운다.
2. 복제본에 Stet의 스키마를 올리고(게이트 계정으로), 게이트 대상 CRM 테이블과 그 연쇄 자식에 캡처를 설치하고(소유자로), 캡처 설정을 에이전트 계정들로 맞춘다.
3. 캡처한 테이블마다 키별 행의 지문을 떠 둔다.
4. V와 X 스트림을 `none` arm으로(프록시 없이) 보낸다. X가 데이터베이스를 다시 복제해야 하는 지점에서는 그 전에 5~6을 하고, 새 복제본에서 2~3을 다시 한다.
5. 에이전트 계정마다 소유자의 연결에서 `stet.undo(계정, 스트림을 보내기 전의 시각)`를 부르고 커밋한다.
6. 테이블마다 지문을 다시 떠서 비교한다.

**결과로 내는 것.** 테이블별로 같은 행과 다른 행의 수. `outcome`과 `reason`별 줄의 수. 다른 행마다 그 이유(건너뛴 이유, `not_captured`, 캡처하지 않는 테이블, 에이전트가 아닌 계정의 변경). 경로 `s5.call`과 `s5.view`의 문장이 바꾼 행이 되돌려졌는지. 보낸 문장의 수와 캡처가 남긴 묶음의 수.

- [ ] **Step 1: 실패하는 테스트.** `TestUndoCheckOnASmallStream`: 연습용 작은 스트림(기존 벤치마크 테스트가 쓰는 것)에서 위의 절차가 돌고, 함수를 거친 쓰기와 직접 쓰기가 모두 되돌려져 다른 행이 0이거나, 다른 행마다 이유가 붙는다. 테스트가 만든 데이터베이스와 역할은 지워진다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 실제 V와 X 스트림으로 한 번 돌리고, 결과의 숫자를 `docs/lab.md`에 적는다(명령과 날짜, 서버 버전 포함). 숫자가 기대와 다르면 고치지 말고 그대로 적고 보고에 이유를 적는다. 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Lab: check undo end to end against the benchmark streams`.

---

### Task 11: 문서

**Files:** Modify `stet.example.yaml`, `docs/lab.md`, `docs/spec-v0.1.md`, `docs/spec-v0.2-undo.md`. Test `cmd/stet/main_test.go`(예시 설정의 기존 테스트가 통과해야 한다. 단언은 고치지 않는다).

- [ ] **Step 1:** `stet.example.yaml`에 `undo` 절을 주석으로 더한다(스펙 9장의 예시와 설명, `stet undo-sql`을 실행하라는 말).
- [ ] **Step 2:** `docs/lab.md`에 되돌리기 절: 켜는 순서(설정 → Stet을 한 번 시작 → `stet undo-sql` → 소유자가 실행 → 검토자에게 줄 `GRANT`), `stet.history`·`stet.undo_plan`·`stet.undo`의 예, Task 10의 명령과 결과, Task 3이 잰 오버헤드.
- [ ] **Step 3:** `docs/spec-v0.1.md` 8장의 보존 기간·판단 기록에 관한 문장 가운데 이 단계로 낡은 것을 고친다(새 컬럼 셋, 변경 기록의 보존은 `undo.keep`). 고칠 문장이 없으면 고치지 않고 보고에 적는다.
- [ ] **Step 4:** `docs/spec-v0.2-undo.md`: 상태를 "구현 (날짜)"로, 2장의 오버헤드 숫자 옆에 완성된 함수로 잰 숫자를, U1~U9를 해당 장에(4장의 `key_column`, 7장의 U3·U4·U5, 9장의 U6·U7·U8, 11장의 U2), 12장의 표에 `internal/undo`를 적는다. 13장의 체크는 건드리지 않는다(실행을 맡은 쪽이 근거와 함께 채운다).
- [ ] **Step 5:** 전체 명령이 통과한다. 커밋 `Docs: undo as built`.
