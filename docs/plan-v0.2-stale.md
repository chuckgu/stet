# 낡은 쓰기 탐지 (5단계) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 에이전트가 읽은 행을 그 뒤에 키로 고치거나 지울 때, 읽은 뒤에 그 행이 바뀌었으면 결정론적으로 `reject`(`stale_read`)한다. 섀도는 기록만 하고 enforce는 `ST003`으로 막는다.

**Architecture:** 프록시가 `stale`을 켠 테이블을 읽는 응답의 결과 행에서 키 값을 뽑아 `Observe`로 게이트에 넘긴다. 게이트는 그 읽기를 "조회를 기다리는 읽기"로 적고, 뒤의 작업자가 Stet의 연결로 그 행들의 `xmin`을 읽어 (접속 계정, 업무, 테이블, 키)마다 메모리에 기억한다. 쓰기의 상태 읽기가 이미 돌려주는 `xmin`을 그 기억과 비교한 결과가 결정론적 규칙의 입력이 되고 `stet.decisions.read_check`에 남는다. 자기 쓰기는 "쓰는 중" 표시와 고치기·지우기로 가려낸다.

**Tech Stack:** Go 1.27, `github.com/jackc/pgx/v5`(pgproto3, pgxpool), `pg_query_go`(분류기), 테스트 서버 PostgreSQL 14.18.

**Spec:** [docs/spec-v0.2-stale.md](spec-v0.2-stale.md). 스펙과 이 계획이 다르면 아래 "스펙 보정"에 적힌 곳만 계획을 따르고, 나머지는 스펙을 따른다. 보정은 Task 12가 스펙에 옮겨 적고, 소유자에게는 끝날 때 보고한다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 규칙 6: 섀도에서 프록시가 넘기는 바이트는 `stale`을 켜기 전과 같다. 결과 행은 읽기만 한다. 규칙 1: 읽기 쪽도 쓰기 쪽도 조회를 기다리지 않는다. Stet의 조회는 Stet의 풀에서 트랜잭션 밖의 문장 하나다. 규칙 3: `read_check`는 줄을 넣을 때만 적는다. 규칙 4: 재생은 기록된 판정을 쓴다. 규칙 5: `on_behalf_of`는 묶는 데만 쓰고, 한 접속 계정의 읽기·쓰기는 다른 접속 계정의 기억을 건드리지 못한다. 규칙 7: `decide.Decider`는 바꾸지 않는다.
- **이 단계의 모든 근사는 놓치는 쪽으로 틀린다.** 확신할 수 없으면 판정하지 않는다. 멀쩡한 쓰기를 `stale`로 만드는 길이 보이면 그것은 결함이다.
- 스펙의 값. 판정 `reject`, 이유 `stale_read`, SQLSTATE `ST003`. 에러 문장 `write refused by stet: the row was changed after it was read`(행이 지금 없으면 `the row was deleted after it was read`), DETAIL `decision=<id> reasons=stale_read`, HINT `read the row again, then send the write again if it still applies`. 결과 여섯: `fresh`, `stale`, `no_read`, `read_pending`, `own_write`, `not_checked`. 한 읽기의 키 1,000개. 조회 상한 500ms. `remember` 기본 `1h`(1m 이상 24h 이하). `stale.max_keys` 기본 200000(1000 이상 10000000 이하). 한 접속 계정은 전체의 4분의 1까지. 바이너리로 읽는 키 타입: `int2`, `int4`, `int8`, `uuid`, `text`, `varchar`, `bpchar`, `name`. 조회문은 `select t.<키>, t.xmin::text from <테이블> as t where t.<키> = any($1)`이고 키의 글자는 결과의 글자 형식(타입의 출력 함수)으로 받는다. 값에 `::text`를 쓰지 않는다. `stet.decisions.read_check`의 키: `result`, `read_version`, `version`, `read_age_ms`, `read_on_behalf_of`. `stet.reads`의 새 컬럼: `keyed`, `keys`.
- 테스트: `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. PostgreSQL 14.18이 이미 떠 있다(superuser `postgres`/`postgres-pw`, 데이터베이스 `stet_test`, Stet의 계정 `stet_scram`/`scram-pw`, 에이전트 `stet_md5`/`md5-pw`, 비밀번호 없는 `stet_trust`). 테스트 서버를 켜거나 끄지 않는다. 이 변수가 없으면 통합 테스트가 건너뛰어지고, 그것은 통과가 아니다. 모든 Task는 이 명령이 모든 패키지에서 `ok`인 채로 끝난다.
- **`stet_trust`를 어떤 역할에도 부여하지 않고, 어떤 역할도 `stet_trust`에 부여하지 않는다.** PUBLIC, `public` 스키마, 공유 데이터베이스 `stet_test` 자체에는 GRANT·REVOKE하지 않는다. 다른 권한이 필요한 테스트는 자기 데이터베이스와 역할을 만들고 지운다. 서버에 이미 있는 벤치마크 데이터베이스와 역할(`crm_owner`, `gate_svc`, `agent_v`, `agent_i`, `agent_x`, `agent_sync`)은 읽고 복제할 수만 있다. 지우지도, 고치지도, 비밀번호나 구성원을 바꾸지도 않는다.
- TDD: 동작이 바뀌는 곳은 실패하는 테스트를 먼저 쓰고, 돌려서 실패를 본 뒤에 구현한다.
- `internal/proxy`를 건드리는 Task는 `internal/proxy/latency_test.go`(p95 2ms)도 돌린다. 그 파일의 기존 테스트는 고치지 않는다.
- 기존 테스트의 단언은 Task가 이름을 적어 허락한 곳만 고친다. 테스트용 게이트와 헬퍼를 새 메서드·필드에 맞추는 것은 단언 변경이 아니다.
- README.md, docs/concept-v0.2.md, docs/decisions.md(소유자의 문서)와 앞 단계의 계획 문서는 고치지 않는다.
- 문서와 YAML·SQL 스키마 주석은 한국어, 코드의 주석·식별자·에러 메시지·로그·프로그램의 출력은 영어다.
- API 키(`~/.config/stet/*.env`)는 읽지 않는다. 이 단계는 모델을 부르지 않는다.
- 브랜치 `v0.2-stale`에서 작업한다. 커밋 메시지는 영어(명령형 한 줄 제목)이고 마지막 줄은 `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`이다. 어느 모델이 쓰든 이 줄을 바꾸지 않는다.

## Review Focus

스펙이 암시하지만 수용 기준에 줄이 없는 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **인용이 필요한 이름과 같은 이름의 다른 테이블.** 대문자·공백이 든 테이블과 키 컬럼, 스키마를 적은 설정 키(`crm.contacts`). 다른 스키마의 같은 이름 테이블을 `SELECT … WHERE id = $1`로 읽은 것은 기억하지 않는다. (Task 8 `TestLookupQuotesNames`, `TestSameNameInAnotherSchemaIsNotRemembered`)
2. **글자로 헷갈리는 키.** 쉼표·큰따옴표·백슬래시·중괄호가 든 글자 키, 빈 글자 키, 글자 `NULL`, 음수, 대문자로 적은 `uuid`, 외부 조인의 NULL 키. 조회의 배열 값이 깨지지 않고 다른 행의 버전과 섞이지 않는다. (Task 6 `TestNullKeyIsSkipped`, Task 8 `TestLookupKeepsOddKeysApart`)
3. **연결이 중간에 끊긴다.** 읽기 응답 도중, 쓰기가 서버에 간 뒤 답이 오기 전, 트랜잭션이 열린 채, 섀도에서 `DO` 블록을 보낸 직후. 표시가 남아 그 계정의 쓰기가 계속 `own_write`가 되지 않고, 끊긴 쓰기가 낸 변경이 `stale`이 되지 않는다. (Task 5 `TestTxEndedWhenConnectionDrops`, Task 9 `TestLostWriteLeavesNoMark`)
4. **카탈로그가 달라진다.** 추적하던 테이블을 지우고 같은 이름으로 다시 만든다(OID가 바뀐다). 시작할 때 없던 테이블이 뒤에 생긴다. 키 컬럼의 타입이 목록 밖으로 바뀐다. 1분 안에 따라가고, 그 사이에는 놓칠 뿐 쓰기가 실패하지 않는다. (Task 8 `TestCatalogIsReadAgain`)
5. **큰 읽기와 답하지 않는 DB.** 수십만 행을 돌려주는 읽기(키는 1,000개, 메모리는 늘지 않는다). 조회가 500ms를 넘긴다. Stet의 풀이 바닥났다. 게이트가 닫힌 뒤에 온 `Observe`. 읽기와 쓰기는 기다리지 않고 지나가고 수가 로그에 남는다. (Task 6 `TestLargeReadKeepsThousandKeys`, Task 8 `TestSlowLookupDelaysNothing`, `TestObserveAfterClose`)

## 스펙 보정

코드와 PostgreSQL 14.18에 맞춰 보니 스펙이 정하지 않았거나 스펙대로는 멀쩡한 쓰기를 막을 수 있는 곳이다. 모두 놓치는 쪽으로 고른 가장 작은 보정이다.

| # | 스펙 | 확인한 것 | 보정 (Task) |
|---|---|---|---|
| S1 | 4장 "글자 형식은 그대로 쓴다" | 결과의 키 글자는 클라이언트 세션의 설정으로 쓰인 것이고, Stet은 그것을 자기 연결의 설정으로 다시 읽는다. `date`·`timestamp`·`float`·`bytea` 같은 타입은 두 설정이 다르면 다른 행으로 읽힐 수 있다 | 추적하는 테이블은 키 컬럼의 타입(도메인이면 그 밑의 타입)이 Global Constraints의 여덟 타입인 것뿐이다. 아니면 시작할 때 경고하고 추적하지 않는다. 그래서 "바이너리로 온 지원하지 않는 타입"은 생기지 않는다 (6, 8) |
| S2 | 4장 | 섀도 세션에서는 프록시가 `client_encoding`을 따라가지 않는다. UTF-8이 아닌 글자 키를 Stet의 연결이 다른 글자로 읽을 수 있다 | 글자 타입의 키 값에 ASCII가 아닌 바이트가 있으면 그 키는 얻지 않는다. 1,024바이트를 넘는 키 값도 얻지 않는다 (6) |
| S3 | 7장 "그 밖의 쓰기 … 실행됐으면 … 지운다" | 여러 행 쓰기가 서버에서 커밋된 뒤 프록시가 그 답을 처리하기 전에, 같은 계정의 다른 연결이 그 행에 쓰면 자기 변경을 낡았다고 본다 | 행의 키를 모르는 쓰기도 서버로 갈 때 "쓰는 중" 표시를 한다(테이블 단위). 표시가 있는 동안 그 계정의 그 테이블 쓰기는 `own_write`다 (7, 9) |
| S4 | 8장 "`stale`이면 `reasons`에 `stale_read`가 있다" | 섀도는 더러운 트랜잭션 안의 쓰기도 상태를 읽고, 규칙 순서는 `in_transaction`이 먼저다 | `read_check`는 어느 규칙이 정했든 비교한 대로 적는다. `stale_read`는 낡음 규칙이 정했을 때만 `reasons`에 든다 (9) |
| S5 | 4장 "결과에 키가 없으면 조건절에서 얻는다" | 분류기는 테이블을 이름으로 맞춘다(다른 스키마의 같은 이름도 맞는다). 조건절의 키를 설정의 테이블에서 조회하면 읽지 않은 행을 읽었다고 기억할 수 있다 | 조건절의 키는 결과에 그 테이블(OID)의 컬럼이 하나 이상 있을 때만 쓴다. `SELECT count(*) … WHERE id = $1`은 기억하지 않는다 (6, 8) |
| S6 | 7장이 연결이 끊긴 쓰기를 말하지 않는다 | 답이 오기 전에 끊긴 쓰기는 실행되고 커밋됐을 수 있다 | 끝난 트랜잭션 안의 쓰기처럼 다룬다. 그 행(또는 테이블)의 기억을 지우고 표시를 지운다 (9) |
| S7 | 13장 "닫힌 지점의 메시지가 지나갈 때마다 알리기" | `WouldBlock`은 세션과 이유마다 한 번 부르고 로그를 낸다. 준비해 둔 `DO` 블록의 Execute와 SQL `EXECUTE`는 지금 알리지 않는다 | `WouldBlock`은 그대로 두고 `Unseen`을 새로 둔다. 섀도 세션에서 들여다보지 못하는 메시지가 갈 때마다 부른다. SQL `EXECUTE`가 든 문장도 그렇게 본다(준비된 쓰기일 수 있다). 그 메시지의 답이 오고 세션이 유휴가 되면 `TxEnded`를 부른다. `TxEnded`는 이번에 정리되는 `Unseen`의 수를 함께 넘긴다. 답을 기다리지 않고 연달아 보내는 클라이언트에서 앞 메시지의 끝이 뒤 메시지의 표시를 지우지 않게 한다 (5, 9) |
| S8 | 5장 "지운 것에는 시각을 남기고" | 자기 쓰기 뒤에 기억을 고칠 때도, 그 쓰기보다 먼저 끝난 읽기의 늦은 조회 결과가 옛 버전을 넣을 수 있다 | 고칠 때도 같은 시각을 남긴다. "쓰는 중" 표시는 고치기 조회가 끝날 때 지운다 (7, 9) |
| S9 | 7장 "그 접속 계정의 그 테이블 기억을 모두 지운다" | 분류기는 첫 게이트 대상 쓰기의 테이블만 준다. `multi_statement`, `nested_write`, `with`, `truncate`는 다른 테이블도 쓸 수 있다 | 그 네 이유의 쓰기는 그 계정의 모든 테이블에 표시하고 지운다 (9) |
| S10 | 7장 "승인되어 실행된 쓰기 … 단일 행이면 그 행" | 실행부가 아는 키는 에이전트가 적은 글자다. 기억의 키(서버의 글자)와 다를 수 있다 | 실행부는 문장을 실행하기 전에 게이트에 알리고(테이블 단위 표시), 끝나면 그 계정의 그 테이블 기억을 지운다 (10) |
| S11 | 6장 "행이 지금 없으면 `stale`" | 지금의 상태 읽기는 행이 없으면 아무것도 돌려주지 않아, 기억의 키(서버의 글자)를 알 수 없다 | 추적하는 테이블의 상태 읽기는 행이 없어도 키의 글자를 돌려주는 모양으로 바꾼다. 질의 수는 그대로 하나다 (9) |

**스펙이 구현에 맡겨 이 계획이 정한 것.** 프록시는 게이트에게 테이블 OID마다 키 컬럼의 번호와 타입을 묻는다(`Tracked`). 확장 프로토콜의 키 컬럼 자리는 Parse한 문장마다 하나인 칸에 두고 그 문장의 포털이 함께 쓴다. 조회 작업자는 둘이고 대기열은 1,024다. "읽기가 아닌 문장"은 분류기의 `ReadOnly`가 거짓인 메시지다. 승인 실행을 게이트에 알리는 길은 `Applier.Applying` 필드다. 실험실 확인은 Stet을 자기 프로세스 안에서 띄우고 `Gate.StaleIdle`로 조회가 끝나기를 기다린다.

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/config/config.go`, `stet.example.yaml` | 테이블의 `stale`, 전체 `stale` (수정) |
| `internal/sqlparse/classify.go`, `read.go` | `ReadOnly`, `ReadKey` (수정) |
| `internal/decide/decide.go`, `act.go` | `ReadCheck`, 규칙 `stale_read`, `ST003` (수정) |
| `internal/lab/replay/replay.go`, `score.go` | 기록된 `stale_read`를 그대로 두기 (수정) |
| `internal/store/schema.sql`, `store.go`, `reads.go` | `read_check`, `keyed`·`keys` (수정) |
| `internal/proxy/session.go` | `Gate`의 새 메서드, 트랜잭션 끝과 들여다보지 못한 메시지 알리기 (수정) |
| `internal/proxy/keys.go` | RowDescription·DataRow에서 키 뽑기, 문장마다의 키 컬럼 자리 (신규) |
| `internal/gate/stale_memory.go` | 기억: 자리, 조회를 기다리는 읽기, 표시, 지운 시각, 상한, 비교 (신규, DB 없음) |
| `internal/gate/stale.go` | 카탈로그, 대기열과 조회 작업자, `Tracked`, 읽기 쪽 연결 (신규) |
| `internal/gate/stale_write.go` | 쓰기 쪽: 비교, 표시, 고치기·지우기, `Unseen`·`TxEnded`·`Applying` (신규) |
| `internal/gate/gate.go`, `reads.go`, `enforce.go` | 위를 `Inspect`·`Observe`·`Begin`·`End`·`judge`·`recheck`에 잇기 (수정) |
| `internal/apply/apply.go` | 실행 전후에 게이트에 알리기 (수정) |
| `cmd/stet/main.go` | 실행부와 게이트 잇기 (수정) |
| `internal/lab/bench/stale.go`, `stale_result.go`, `cmd/stet-lab/bench.go` | `bench stale-check` (신규, 수정) |
| `docs/spec-v0.2-stale.md`, `docs/lab.md`(Task 11), `docs/spec-v0.1.md`, `docs/spec-v0.2-enforce.md` | 문서 (수정) |

테스트 파일은 각 Task가 적는다.

---

### Task 0: 계획 커밋

- [ ] **Step 1:** `git status`에 이 계획만 추적되지 않은 파일로 보여야 한다. 브랜치는 `v0.2-stale`이다.
- [ ] **Step 2:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`가 모든 패키지에서 `ok`인지 확인한다.
- [ ] **Step 3:** 커밋 `Plan: stale-write detection (stage 5)`.

---

### Task 1: 설정 `stale`

**Files:** Modify `internal/config/config.go`, `stet.example.yaml`. Test `internal/config/config_test.go`.

**Interfaces — Produces:**

```go
// in Table:
Stale *Stale // nil: reads of the table are not remembered and its writes are not checked

// Stale turns on stale-write detection for a table (spec-v0.2-stale 9).
type Stale struct {
	Remember time.Duration // stale.remember: default 1h
}

// in Config:
// StaleMaxKeys is stale.max_keys: how many row versions are remembered in all.
// 200000 unless set; 0 when no table has stale.
StaleMaxKeys int
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestStaleAbsent`: `stale`이 없으면 모든 테이블의 `Stale == nil`, `StaleMaxKeys == 0`.
  - `TestStaleSettings`: 스펙 9장의 YAML → `Tables["contacts"].Stale.Remember == time.Hour`, `StaleMaxKeys == 200000`. 값 없이 `stale:`만 쓴 테이블도 켜지고 `Remember == time.Hour`다. `remember: 1m`, `remember: 24h`, `max_keys: 1000`, `max_keys: 10000000`은 유효하다. `stale`과 `reads`를 함께 쓴 테이블도 유효하다.
  - `TestInvalidConfigs`에 행을 더한다.

| 이름 | 설정 | 에러에 들어갈 문자열 |
|---|---|---|
| stale remember too short | 테이블 `stale: {remember: 59s}` | `stale.remember` |
| stale remember too long | 테이블 `stale: {remember: 25h}` | `stale.remember` |
| stale remember not a duration | 테이블 `stale: {remember: soon}` | `stale.remember` |
| stale table unknown key | 테이블 `stale: {keep: 1h}` | `keep` |
| stale without a table | 전체 `stale: {max_keys: 5000}`, 어느 테이블도 `stale` 없음 | `stale` |
| stale max_keys too small | `max_keys: 999` | `stale.max_keys` |
| stale max_keys too big | `max_keys: 10000001` | `stale.max_keys` |
| stale unknown key | 전체 `stale: {max_key: 5}` | `max_key` |

- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/config/`
- [ ] **Step 3:** 구현한다. 값 없는 `stale:`은 `reads:`와 같은 방법(`presentKeys`)으로 알아본다. 기간은 기존 `parseDuration`을 쓴다.
- [ ] **Step 4:** `stet.example.yaml`의 `contacts`에 스펙 9장의 `stale` 절을, 파일 끝에 전체 `stale` 절을 한국어 주석과 함께 더한다. 주석은 무엇을 하는지, 기본값, 놓치는 것 하나(같은 계정을 여럿이 쓰면서 업무를 적지 않으면 흐려진다)를 적는다. 예시 파일을 읽는 기존 테스트가 통과해야 한다.
- [ ] **Step 5:** `go test -race ./internal/config/`와 전체 명령이 통과한다.
- [ ] **Step 6:** 커밋 `Config: the stale clause`.

---

### Task 2: 분류기 — `ReadOnly`와 `ReadKey`

**Files:** Modify `internal/sqlparse/classify.go`, `internal/sqlparse/read.go`. Test `internal/sqlparse/classify_test.go`.

**Interfaces — Produces** (`sqlparse.Result`):

```go
// ReadOnly: every statement is a SELECT (VALUES and set operations included)
// with no INTO and no data-modifying WITH query, or leaves the transaction
// clean (see Clean). True for SQL with no statement. A read sent after
// read-only messages alone sees no row version Stet's own connection cannot.
ReadOnly bool
// ReadKey is the value of `key = value` in the WHERE clause, as pkEquality
// reads it, when the SQL is one SELECT with no set operation whose FROM is
// exactly one plain reference to a gated table and Reads is that table alone;
// nil otherwise. Set only when Kind is Read.
ReadKey *Value
```

`Tables.Reads`의 주석을 "읽기를 기록하거나 기억하는 테이블이 있는가"로 고친다. 동작은 그대로다.

- [ ] **Step 1: 실패하는 테스트** `TestReadOnly`, `TestReadKey` (게이트 대상 `contacts`(pk `id`), `deals`(pk `id`), `Tables.Reads` 참).

| SQL | `ReadOnly` | `ReadKey` |
|---|---|---|
| `select id, title from contacts where id = 7` | 참 | Literal `7` |
| `select title from contacts c where c.id = $1 and status = 'open'` | 참 | Param 1 |
| `select title from contacts where id in ($2)` | 참 | Param 2 |
| `select title from contacts where id = 7 or id = 8` | 참 | nil |
| `select c.title from contacts c join deals d on d.contact_id = c.id where c.id = 7` | 참 | nil |
| `select title from contacts where id = 7 union select title from contacts where id = 8` | 참 | nil |
| `with contacts as (select 1 as id) select * from contacts where id = 1` | 참 | nil (Kind가 Read가 아니다) |
| `select title from (select * from contacts) s where id = 7` | 참 | nil |
| `select title from contacts where id = 7 for update` | 참 | Literal `7` |
| `select title into scratch from contacts where id = 7` | 거짓 | nil |
| `with d as (delete from notes returning 1) select title from contacts where id = 7` | 거짓 | nil |
| `begin; select title from contacts where id = 7` | 참 | nil (문장이 둘이다) |
| `select title from contacts where id = 7; create temp table x (a int)` | 거짓 | nil |
| `explain select * from contacts`, `copy contacts to stdout`, `commit`, `call p()`, `fetch 1 from c` | 거짓 | nil |
| `set search_path = crm`, `show timezone`, 빈 문자열 | 참 | nil |
| `update contacts set title = 'x' where id = 7` | 거짓 | nil |

- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/sqlparse/`
- [ ] **Step 3:** 구현한다. `ReadOnly`는 최상위 문장의 종류만 본다(PostgreSQL은 데이터를 고치는 `WITH`를 최상위에서만 받는다). `ReadKey`는 `Kind == Read`일 때만 찾는다.
- [ ] **Step 4:** `go test -race ./internal/sqlparse/`와 전체 명령이 통과한다. `go test -bench . -run xxx ./internal/sqlparse/`의 결과가 이 Task 전과 10% 넘게 느려지지 않았는지 보고에 적는다.
- [ ] **Step 5:** 커밋 `Classifier: read-only messages and the key a read selects by`.

---

### Task 3: 판정 — `stale_read` 규칙과 `ST003`, 재생

**Files:** Modify `internal/decide/decide.go`, `internal/decide/act.go`, `internal/lab/replay/replay.go`, `internal/lab/replay/score.go`. Test `internal/decide/decide_test.go`, `internal/decide/act_test.go`, `internal/lab/replay/score_test.go`.

**Interfaces — Produces:**

```go
// decide
const CodeRejected = "ST003" // reject: refused by a deterministic rule; reading again may let it through

// The results of a write's read check (spec-v0.2-stale 6).
const (
	ReadFresh      = "fresh"
	ReadStale      = "stale"
	ReadNone       = "no_read"
	ReadPending    = "read_pending"
	ReadOwnWrite   = "own_write"
	ReadNotChecked = "not_checked"
)

// ReadCheck is what comparing a write's row with the version its login role
// read found. It is stored as stet.decisions.read_check.
type ReadCheck struct {
	Result         string  `json:"result"`
	ReadVersion    string  `json:"read_version,omitempty"` // the xmin remembered; fresh and stale only
	Version        *string `json:"version"`                // the row's xmin now; null when it is not there or was not read
	ReadAgeMS      *int64  `json:"read_age_ms,omitempty"`  // fresh and stale only
	ReadOnBehalfOf *string `json:"read_on_behalf_of"`      // of the read compared; null when it set none
}

// in Input:
ReadCheck *ReadCheck // nil: the table does not remember reads

// in Facts:
RowGone bool // with stale_read: the row is not there now
```

- [ ] **Step 1: 실패하는 테스트.**
  - `TestStaleReadRule`: `ReadCheck{Result: ReadStale}` → `Deterministic`과 `Verdict`가 `reject`, `["stale_read"]`, 정해짐. 나머지 다섯 결과와 `nil`은 이 규칙으로 정해지지 않는다.
  - `TestStaleReadOrder`: `stale`과 함께 `Unsupported`, `InTransaction`, `NoLoginRole`이 있으면 각각 그것이 먼저다. `stale`과 함께 `Cumulative`가 넘었거나 모르거나 `StateErr`가 있으면 `reject`, `stale_read`다. 질문과 답이 있어도 `stale`이면 답을 보지 않는다.
  - `TestActReject`: `Act(Reject, ["stale_read"], Facts{})` → `Forward`·`Held`가 거짓, `Code == "ST003"`, `Message == "write refused by stet: the row was changed after it was read"`, `Hint("") == "read the row again, then send the write again if it still applies"`. `Facts{RowGone: true}` → `Message == "write refused by stet: the row was deleted after it was read"`. `["stale_read", "rejudged"]`도 같은 문장이다. 모르는 이유의 `reject`는 `Code == "ST003"`, `Message == "write refused by stet: it was rejected (<이유>)"`, 힌트는 빈 글자다.
  - `TestRejectIsNeverForwarded`: 테이블의 `unsupported: pass`와 어떤 `Facts`로도 `reject`는 `Forward`가 거짓이다.
  - `internal/lab/replay`: `TestModelVerdictsKeepRecordedStaleRead`: 기록된 판정이 `reject`이고 이유에 `stale_read`가 있으며 질문이 없는 기록 → `Judged{Verdict: "reject", Reasons: ["stale_read"]}`. 지금은 `commit`이 나온다.
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/decide/ ./internal/lab/replay/`
- [ ] **Step 3:** 구현한다. 규칙은 `deterministic`의 `NoLoginRole` 바로 다음에 둔다. `Verdict`의 주석(규칙의 번호)과 `act.go` 머리의 `ST003` 주석을 고친다. `replay.Record`에 `Reasons []string`을 더하고 기록을 읽을 때 채운다.
- [ ] **Step 4:** `go test -race ./internal/decide/ ./internal/lab/...`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Decide: reject a write of a row changed since it was read`.

---

### Task 4: 저장소 — `read_check`, `keyed`, `keys`

**Files:** Modify `internal/store/schema.sql`, `internal/store/store.go`, `internal/store/reads.go`. Test `internal/store/store_test.go`, `internal/store/reads_test.go`.

**Interfaces — Produces:**

```go
// in Decision:
ReadCheck any // stored as jsonb (a decide.ReadCheck); nil is NULL

// in ReadBucket:
Keyed int64 // reads that gave at least one key
Keys  int64 // keys taken
```

`schema.sql`: `ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS read_check jsonb;`(한국어 주석: 스펙 8장, 줄을 넣을 때만 적는다, `stale`을 켜지 않은 테이블은 NULL). `ALTER TABLE stet.reads ADD COLUMN IF NOT EXISTS keyed bigint NOT NULL DEFAULT 0;`와 `keys`도 같다.

- [ ] **Step 1: 실패하는 테스트.**
  - `TestInsertRecordsReadCheck`: `ReadCheck: map[string]any{"result": "stale", "read_version": "700", "version": "705", "read_age_ms": 1500, "read_on_behalf_of": nil}`(이 패키지의 테스트는 `decide`를 들이지 않는다) → 읽어 온 jsonb의 `result`, `read_version`, `version`, `read_age_ms`가 그 값이고 `read_on_behalf_of`는 JSON null이다. `ReadCheck`가 nil이면 컬럼이 NULL이다.
  - `TestReadCheckIsAppendOnly`: 넣은 줄의 `read_check`를 `UPDATE`하면 `ST004`다.
  - `TestAddReadsSumsKeys`: 같은 (분, 계정, 테이블)에 `Keyed: 1, Keys: 3`과 `Keyed: 1, Keys: 2`를 두 번에 나눠 더하면 `keyed = 2`, `keys = 5`다. 한 번의 호출 안에서 겹쳐도 같다.
- [ ] **Step 2:** 돌려서 실패를 본다. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Store: read_check on decisions, keyed reads`.

---

### Task 5: 프록시 — 트랜잭션의 끝과 들여다보지 못한 메시지 알리기

이 Task가 `Gate` 인터페이스와 `Statement`·`Outcome`의 새 필드를 한 번에 선언한다. 키를 뽑는 동작은 Task 6이다.

**Files:** Modify `internal/proxy/session.go`, `internal/gate/gate.go`(빈 구현). Create `internal/proxy/keys.go`(타입 선언만). Test `internal/proxy/session_internal_test.go`, `internal/proxy/gate_test.go`.

**Interfaces — Produces** (`internal/proxy`):

```go
// in Gate:

// Unseen tells the gate that s, a session that does not enforce, is sending
// a message whose writes the proxy cannot see: one WouldBlock is told of, an
// Execute of a portal the proxy could not read or whose statement Inspect
// marked with Block, or SQL that runs a prepared statement by name
// (Statement.Executes). It is called every time, before the message goes
// upstream, and must not block.
Unseen(s SessionInfo)
// TxEnded tells the gate that what s sent in the units the server has
// answered is committed or rolled back: the server reported s idle, or s
// ended, after a watched write that was sent ran inside a transaction (its
// End had Outcome.InTransaction), or after a message Unseen was told of.
// unseen is how many of the Unseen calls for s this call settles: those of
// the messages in the units answered since the session's last TxEnded. A
// pipelined unit not yet answered is not covered. It is called once per such
// ReadyForQuery, after the End calls of the units it answers, and once when
// the connection ends while one is still owed (unseen then counts every call
// not yet settled). It must not block.
TxEnded(s SessionInfo, unseen int)
// Tracked reports, for the table with OID table, the attribute number of the
// key column whose values the gate remembers reads by, and how its binary
// form is read. ok is false for any other table. The proxy calls it on the
// server side for the columns of a tracked read's RowDescription. It must
// not block.
Tracked(table uint32) (attr uint16, typ KeyType, ok bool)

// in Statement:
ReadOnly bool // sending the message leaves later reads of its transaction comparable (sqlparse.Result.ReadOnly)
Track    bool // with Observe: the gate wants the keys of the rows the read sends (Outcome.Read)
KeyParam int  // n when the read selects one row by `key = $n`: the proxy keeps that bind value for Outcome.Read; 0 otherwise

// in Outcome:
Read *ReadKeys // set only for Observe, see Task 6

// keys.go
type KeyType uint8

const (
	KeyInt2 KeyType = iota + 1
	KeyInt4
	KeyInt8
	KeyUUID
	KeyText // text, varchar, bpchar, name: the bytes as they are
)

// MaxReadKeys is how many key values the proxy takes from one read.
const MaxReadKeys = 1000

type RowKey struct {
	Table uint32 // the table's OID
	Text  string // the key as the type's output function writes it
}

// ReadKeys is what the proxy took from the answer to a tracked read.
type ReadKeys struct {
	Keys   []RowKey // from the rows sent, in order, at most MaxReadKeys
	Tables []uint32 // the tracked tables the result had a column of, in column order, each once
	Param  *Param   // the value bound to Statement.KeyParam; nil when there is none
}
```

`gate.Gate`에는 `Unseen`, `TxEnded`(아무것도 하지 않는다)와 `Tracked`(언제나 `ok == false`)를 둔다. Task 8과 9가 채운다. `WouldBlock`은 지금 그대로다.

- [ ] **Step 1: 실패하는 테스트.** 기록하는 테스트 게이트에 세 메서드를 더하고(호출을 순서대로 적는다), 다음을 쓴다.
  - `TestTxEndedAfterWriteInTransaction`: `BEGIN` / 감시하는 `UPDATE` / `COMMIT` → `End`(InTransaction 참) 다음, `COMMIT`의 ReadyForQuery 뒤에 `TxEnded`가 한 번. `ROLLBACK`으로 끝나도 같다.
  - `TestNoTxEndedForAutocommitWrite`: 트랜잭션 밖의 감시하는 쓰기 하나 → `TxEnded`가 없다.
  - `TestTxEndedAfterImplicitTransaction`: 한 Sync 아래 Execute 둘(감시하는 쓰기) → `End` 둘 다음에 `TxEnded` 한 번.
  - `TestTxEndedOncePerTransaction`: 한 트랜잭션 안의 쓰기 셋 → `TxEnded` 한 번. 다음 트랜잭션의 쓰기 뒤에 다시 한 번.
  - `TestNoTxEndedForRefusedWrite`: 게이트가 거절한 쓰기뿐인 트랜잭션 → `TxEnded`가 없다.
  - `TestTxEndedWhenConnectionDrops`: `BEGIN`, 감시하는 쓰기(답을 받음), 연결 끊기 → `TxEnded` 한 번. 답이 오기 전에 끊겨도(쓰기는 `Lost`) 한 번. `Unseen` 뒤에 끊겨도 한 번.
  - `TestUnseenEveryTime` (enforce가 아닌 세션): `DO` 블록 Query 둘 → `Unseen` 둘, `WouldBlock`은 지금처럼 한 번. FunctionCall, 읽지 못한 Query, 코드를 정의하는 문장도 각각 `Unseen`. 준비한 `DO` 블록을 Bind·Execute로 두 번 실행 → Parse에서 한 번과 Execute마다 한 번. `EXECUTE w(1)`이 든 Query → `Unseen`. 평범한 `SELECT`와 감시하는 쓰기 → `Unseen`이 없다.
  - `TestTxEndedAfterUnseen`: 트랜잭션 밖의 `DO` 블록 → `Unseen`, 그 답 뒤에 `TxEnded`. `BEGIN` / `DO` / `COMMIT` → `TxEnded`는 `COMMIT` 뒤에 한 번.
  - `TestEnforcedSessionHasNoUnseen`: enforce 세션에서 `DO` 블록은 지금처럼 거절되고 `Unseen`도 `TxEnded`도 없다.
- [ ] **Step 2:** 돌려서 실패를 본다. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/`
- [ ] **Step 3:** 구현한다. 알릴 것이 있는지는 단위(`unit`)에 적고, 트랜잭션이 열린 채 답을 받은 단위의 것은 `carry`처럼 세션으로 옮긴다. 파이프라인에서 앞 단위의 유휴 알림이 뒤 단위의 것을 대신하지 않게 한다. 게이트는 `s.mu`를 쥐지 않고 부른다. Execute가 포털의 문장에 `Block`이 있는지 알 수 있도록 `binding`이 그런 문장도 포털에 둔다(enforce의 동작은 바뀌지 않아야 한다).
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/ ./internal/gate/`와 지연 테스트, 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Proxy: tell the gate when a transaction with writes ends and when a message cannot be seen`.

---

### Task 6: 프록시 — 결과 행에서 키 뽑기

스펙 4장이다. 이 Task가 끝나면 `Observe`의 `Outcome.Read`가 채워진다.

**Files:** Modify `internal/proxy/session.go`, `internal/proxy/keys.go`, `internal/proxy/export_test.go`. Create `internal/proxy/keys_test.go`. Modify `internal/proxy/latency_test.go`(테스트 하나를 더한다).

**Interfaces — Consumes:** Task 5의 `Tracked`, `Statement.Track`·`KeyParam`·`ReadOnly`, `ReadKeys`. **Produces:** `Outcome.Read`의 동작과, 게이트가 바인딩 값을 같은 규칙으로 읽는 데 쓰는 함수 하나.

```go
// KeyOutput is the key value v, sent in format (0 text, 1 binary), as the
// output function of a type read as typ writes it. ok is false for NULL, for
// a binary value whose length does not fit typ, for a KeyText value holding
// a byte that is not ASCII, and for a value longer than 1024 bytes.
func KeyOutput(typ KeyType, format int16, v []byte) (text string, ok bool)
```

**동작.** 추적하는 읽기는 `Observe`와 `Track`이 서 있고 `Cursors`가 없는 문장이 Query나 Execute로 온 것이다.

1. `Outcome.Read`는 다음일 때 nil이다. 추적하는 읽기가 아니다. 그 읽기가 왔을 때, 서버가 그 세션을 마지막으로 유휴라고 알린 뒤로 `ReadOnly`가 아닌 메시지(읽지 못한 메시지, FunctionCall, 무엇을 실행하는지 모르는 Execute 포함)가 이미 갔다. 같은 단위의 앞선 메시지와 아직 답을 받지 못한 앞 단위도 센다. 확장 프로토콜에서 그 문장의 결과 컬럼을 서버가 답한 Describe로 알지 못한다. 커서의 `FETCH`다.
2. 키 컬럼은 RowDescription의 (테이블 OID, 컬럼 번호)가 `Tracked`의 답과 맞는 결과 컬럼이다. 한 결과에 여럿이면(자기 조인, 두 테이블) 모두 뽑는다. `Tables`는 `Tracked`가 참인 OID의 컬럼이 하나라도 있는 테이블이다.
3. 단순 프로토콜은 결과 집합마다 오는 RowDescription을 쓴다. 문장이 여럿인 Query는 모든 결과 집합에서 뽑는다.
4. 확장 프로토콜은 문장의 Describe나 그 문장에 묶인 포털의 Describe에 서버가 RowDescription으로 답했을 때, 그 자리를 Parse한 그 문장에 적어 둔다. 그 뒤의 모든 Execute가 Describe 없이도 쓴다. 이름이 다시 Parse되면 새 문장이고 적어 둔 것을 쓰지 않는다. Describe가 왔을 때 그 이름의 뜻이 하나로 정해지지 않으면(`names.get`의 `others`가 있으면) 적지 않는다. 값의 형식은 그 포털의 Bind가 적은 결과 형식 코드다.
5. 값. NULL은 건너뛴다. 글자 형식은 바이트 그대로다. 바이너리는 `KeyInt2`·`KeyInt4`·`KeyInt8`을 십진수로, `KeyUUID`를 소문자 `8-4-4-4-12`로, `KeyText`를 바이트 그대로 읽는다. 길이가 타입과 맞지 않으면 건너뛴다. `KeyText`의 값에 0x80 이상의 바이트가 있으면 건너뛴다(S2). 1,024바이트를 넘는 값은 건너뛴다. 한 읽기에서 `MaxReadKeys`개를 얻으면 더 뽑지 않는다. 행 수(`Sent`)는 지금처럼 모두 센다.
6. `Statement.KeyParam`이 n이고 그 포털의 Bind에 n번째 값이 있으면 그 값 하나를 복사해 `Read.Param`으로 넘긴다. 다른 바인딩 값은 지금처럼 들고 있지 않는다.
7. 나눠 가져가는 포털(행 수 제한이 있는 Execute)은 Execute마다 그때 보낸 행의 키를 넘긴다.
8. 서버 쪽 탭은 추적하는 읽기가 든 단위가 답을 기다리는 동안에만 RowDescription과 DataRow의 본문을 본다. 그런 단위가 없으면 지금처럼 DataRow의 머리만 센다. `maxInspect`를 넘는 DataRow는 건너뛴다.
9. DataRow마다 `s.mu`를 잡지 않는다. 답 하나(완료나 에러까지)에 많아야 한 번 잡는다.
10. 프록시가 클라이언트에게 넘기는 바이트는 그대로다.

- [ ] **Step 1: 실패하는 테스트** (`keys_test.go`. 테스트 게이트는 자기가 만든 테이블의 OID와 키 컬럼 번호를 `Tracked`로 답한다. 테이블은 `stet_test`에 테스트마다 다른 이름으로 만들고 지운다).
  - `TestKeysFromSimpleQuery`: `select id, title from t where …` 세 행 → `Read.Keys`가 그 세 키, `Tables == [oid]`.
  - `TestKeysFromExtendedQuery`: Parse/Bind/Describe(포털)/Execute/Sync → 키. Parse/Describe(문장)/Sync 뒤 Bind/Execute/Sync → 키. 결과 형식이 바이너리일 때 `int2`, `int4`, `int8`, `uuid`, `text`, `varchar`, `bpchar`, `name` 키 각각 → 글자 형식으로 읽은 것과 같은 글자.
  - `TestKeysFromNamedStatementRunAgain`: 이름 있는 문장을 한 번 Describe한 뒤 Bind/Execute만 세 번 → 세 번 모두 키.
  - `TestNoKeysWithoutDescribe`: 한 번도 Describe되지 않은 문장의 Execute → `Read == nil`.
  - `TestKeysAfterNameParsedAgain`: 이름을 다른 문장(키 컬럼의 자리가 다르다)으로 다시 Parse하고 Describe 없이 실행 → `Read == nil`. 다시 Describe한 뒤에는 새 자리의 키.
  - `TestKeysFromJoinAndAlias`: `select c.id as cid, d.id from t c join u d …` → 두 테이블의 키가 모두. 자기 조인 → 두 컬럼의 키가 모두. `Tables`에는 테이블마다 한 번.
  - `TestNoKeysThroughView`: 그 테이블 위의 뷰를 읽으면 `Keys`도 `Tables`도 비어 있다.
  - `TestNullKeyIsSkipped`: 외부 조인으로 NULL인 키 → 그 행은 건너뛰고 나머지는 얻는다.
  - `TestLargeReadKeepsThousandKeys`: 1,500행 → `len(Keys) == 1000`(앞의 1,000개), `Sent == 1500`.
  - `TestOddKeyValues`: 0x80 이상의 바이트가 든 글자 키와 1,025바이트 키는 건너뛴다. 빈 글자 키와 쉼표·따옴표가 든 키는 그대로 얻는다.
  - `TestKeyOutput`: 타입마다 글자·바이너리 값의 표(음수 정수, 길이가 맞지 않는 바이너리, 대문자가 없는 `uuid` 글자, NULL)가 동작 5의 규칙대로 나온다.
  - `TestNoKeysAfterAWrite`: `BEGIN` / `UPDATE other …` / 추적하는 읽기 → `Read == nil`. `BEGIN` / 읽기 / 읽기 → 둘 다 키. `COMMIT`의 답을 받은 뒤의 읽기 → 키. 답을 받지 않은 `ReadOnly`가 아닌 메시지 뒤에 파이프라인으로 온 읽기 → nil. 한 Sync 아래 쓰기 Execute 다음의 읽기 Execute → nil.
  - `TestKeysFromSeveralStatements`: `select id from t where id = 1; select id from t where id = 2` → 두 키.
  - `TestNoKeysFromCursor`: `DECLARE`/`FETCH`로 읽은 행 → `Read == nil`.
  - `TestKeyParam`: 테스트 게이트가 `KeyParam: 1`을 준 `select title from t where id = $1` → `Keys`가 비고 `Tables == [oid]`, `Param`이 그 값·형식·OID. `select count(*) from t where id = $1` → `Tables`가 비어 있다.
  - `TestKeysOfPortalInParts`: 행 수 제한 2로 Execute 세 번 → 보고마다 그 부분의 키.
  - `TestOversizedRowIsSkipped`: `SetMaxInspect`로 낮춘 상한을 넘는 DataRow → 그 행만 건너뛴다.
  - `TestRowsNotShownWithoutTrackedRead`: 추적하는 읽기가 없으면 서버 쪽 탭이 `'D'`와 `'T'`를 원하지 않는다(내부 테스트).
  - `TestTrackedReadBytesUnchanged`: 같은 읽기를 추적하는 게이트와 추적하지 않는 게이트로 보내, 클라이언트 소켓이 받은 바이트가 같다. 단순·확장 프로토콜 각각.
  - `latency_test.go`에 `TestTrackedReadLatencyBudget`: 100행을 돌려주는 추적하는 읽기에서 프록시가 더하는 시간이 p95로 2ms 이하다(`addedLatencyOf`).
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. 키 컬럼의 자리는 Parse 때 만든 칸의 포인터로 문장과 그 포털이 함께 쓴다. 서버 쪽만 채우고 읽는다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/`(지연 테스트 포함)와 전체 명령이 통과한다. `TestTrackedReadLatencyBudget`이 잰 값을 보고에 적는다.
- [ ] **Step 5:** 커밋 `Proxy: take the keys of the rows a tracked read returns`.

---

### Task 7: 게이트 — 기억 (DB 없음)

스펙 5장의 "기억의 모양", "조회를 기다리는 읽기", "지운 뒤에 온 조회 결과", "상한"과 6장의 비교, 7장의 표시다. 이 타입은 I/O를 하지 않고, 시계를 받아 쓴다.

**Files:** Create `internal/gate/stale_memory.go`, `internal/gate/stale_memory_internal_test.go`.

**Interfaces — Produces:**

```go
// readMemory remembers which version of which rows each login role read
// (spec-v0.2-stale 5 to 7). Safe for concurrent use; it does no I/O.
type readMemory struct{ /* ... */ }

// remember maps each tracked table to how long its entries are kept.
func newReadMemory(remember map[string]time.Duration, maxKeys int, now func() time.Time) *readMemory

// party is who read: a login role and its task (stet.on_behalf_of; "" for none).
type party struct{ role, task string }

// reading notes a read by p that ended at readAt and whose versions are not
// known yet: of the rows keys of table, or, with keys nil, of some row of
// table (a key taken from a WHERE clause).
func (m *readMemory) reading(p party, table string, keys []string, readAt time.Time) *ticket
// learned ends t: versions maps the key of each row found, as the server
// writes it, to its xmin. A key of t that is not in versions is forgotten for
// t's party. Nothing is kept for a row, table or role cleared at or after
// t's readAt, nor over an entry read later.
func (m *readMemory) learned(t *ticket, versions map[string]string)
// dropped ends t with nothing learned.
func (m *readMemory) dropped(t *ticket)

// check compares a write by p to the row key of table, whose xmin now is
// version ("" when the row is not there), at time at. A stale result is
// remembered for the unlock rule of spec 6.
func (m *readMemory) check(p party, table, key, version string, at time.Time) decide.ReadCheck

// writing marks a write by role as on its way: to the row key of table, to
// some row of table (key ""), or to any table (table ""). While a mark
// covers a row, check answers own_write for role.
func (m *readMemory) writing(role, table, key string) *writeMark
// refreshed ends a row mark whose write ran and committed: every entry role
// has for the row, whatever its task, becomes version as read now. With
// version "" they are forgotten.
func (m *readMemory) refreshed(mark *writeMark, version string)
// refreshedRow is refreshed for a row that had no mark of its own (an INSERT
// with a key, whose text the server had not yet written).
func (m *readMemory) refreshedRow(role, table, key, version string)
// forgotten ends mark and forgets what it covers for role: the row, the
// table, or everything.
func (m *readMemory) forgotten(mark *writeMark)
// released ends mark and changes nothing (the write did not run).
func (m *readMemory) released(mark *writeMark)
// forget forgets role's entries: of the row key of table, of table (key ""),
// or all (table "").
func (m *readMemory) forget(role, table, key string)

// counts reports and resets what was thrown away for the limits.
func (m *readMemory) evicted() int64
func (m *readMemory) len() int
```

**규칙.**

- 기억의 자리는 (접속 계정, 업무, 테이블, 키)이고 버전과 읽은 시각을 둔다. 같은 자리는 더 늦게 읽은 것만 덮어쓴다.
- `check`가 비교하는 읽기: 업무 `t`의 쓰기는 같은 계정의 (`t`)와 (없음). 업무 없는 쓰기는 같은 계정의 모든 업무. 그 계정의 그 행이 `stale`로 판정된 적이 있으면, 그 판정보다 뒤에 끝난 그 계정의 모든 읽기를 더한다. 테이블의 `remember`보다 오래된 것은 없는 것으로 본다.
- `check`의 순서: 그 행을 덮는 표시가 있으면 `own_write`. 비교하는 읽기 가운데 끝나지 않은 것(`reading`은 했고 `learned`·`dropped`는 아직)이 있으면 `read_pending`. 키 없이 적은 읽기는 그 계정·업무의 그 테이블 모든 행에 해당한다. 비교할 기억이 없으면 `no_read`. 있으면 가장 늦게 읽은 것의 버전이 `version`과 같으면 `fresh`, 다르면(`version == ""` 포함) `stale`.
- `fresh`와 `stale`에는 `ReadVersion`, `ReadAgeMS`(`at`에서 읽은 시각까지), `ReadOnBehalfOf`(비교한 읽기의 업무, 없으면 nil)를 채운다. `Version`은 `version`이고 빈 글자면 nil이다.
- `refreshed`, `refreshedRow`, `forgotten`, `forget`은 그 범위에 시각을 남긴다. 그 시각 이전에 끝난 읽기의 `learned`는 그 범위에 아무것도 넣지 않는다(S8).
- 상한: 전체 개수가 `maxKeys`를 넘으면 가장 오래 전에 읽은 것부터 버린다. 한 접속 계정의 개수가 `maxKeys/4`를 넘으면 그 계정의 가장 오래된 것부터 버린다. `remember`를 넘긴 것은 넣을 때와 `check`에서 걸러 버린다.
- 다른 접속 계정의 자리는 어떤 메서드로도 읽거나 바꾸지 않는다.

- [ ] **Step 1: 실패하는 테스트** (시계는 테스트가 움직인다).
  - `TestFreshAndStale`: 읽기(`v1`) → `check(v1)`은 `fresh`, `check(v2)`는 `stale`(`ReadVersion == "v1"`, `Version == "v2"`), `check("")`는 `stale`(`Version == nil`). 읽지 않은 행은 `no_read`.
  - `TestNewestComparableRead`: 업무 `a`가 `v1`, 뒤에 업무 `b`가 `v2`를 읽음 → `a`의 `check(v2)`는 `stale`, `b`의 것은 `fresh`, 업무 없는 쓰기의 것은 `fresh`(`ReadOnBehalfOf == "b"`). 업무 없이 `v2`를 읽은 것이 더 있으면 `a`의 것도 `fresh`.
  - `TestPendingRead`: `v1` 기억 뒤에 같은 쪽의 `reading`(끝나지 않음) → `check(v2)`는 `read_pending`. `learned(v2)` 뒤에는 `fresh`. `dropped` 뒤에는 옛 기억으로 `stale`. 다른 업무의 끝나지 않은 읽기는 `a`의 쓰기를 `read_pending`으로 만들지 않는다. 키 없이 적은 읽기는 그 테이블의 모든 행에 해당한다.
  - `TestUnlockAfterStale`: P7의 순서로 `a`가 `stale` → 다시 `check`해도 `stale` → 업무 `b`가 그 행을 다시 읽는 중이면 `read_pending`, 끝나면 `a`의 `check`가 `fresh`.
  - `TestOwnWriteMark`: 행 표시가 있는 동안 그 계정의 모든 업무의 `check`가 `own_write`. `refreshed(v3)` 뒤에는 모든 업무의 자리가 `v3`이고 `check(v3)`이 `fresh`. 테이블 표시는 그 테이블의 모든 행에, 모든 테이블 표시는 모든 행에 해당한다. `released`는 기억을 그대로 둔다. `forgotten`은 범위를 지운다. 표시가 둘이면 하나를 끝내도 `own_write`다.
  - `TestLateLookupDoesNotRevive`: 읽기가 끝남(조회 중) → `forget`(행) → `learned(v1)` → `no_read`. 테이블 범위와 계정 범위의 `forget`, `refreshed`, `forgotten`도 같다. 지운 뒤에 끝난 읽기의 `learned`는 들어간다.
  - `TestRowGoneAtLookup`: `learned`가 돌려주지 않은 키는 그 쪽의 자리에서 지워진다. 다른 업무의 자리는 그대로다.
  - `TestOlderReadDoesNotOverwrite`: 늦게 끝난 읽기의 결과가 먼저 들어간 뒤, 먼저 끝난 읽기의 결과가 와도 덮어쓰지 않는다.
  - `TestRememberExpires`: `remember`가 지난 기억은 `no_read`. 테이블마다 다른 `remember`.
  - `TestLimits`: `maxKeys` 8에서 아홉째를 넣으면 가장 오래된 것이 버려지고 `evicted() == 1`. 한 계정이 셋째를 넣으면 그 계정의 가장 오래된 것이 버려지고 다른 계정의 것은 그대로다.
  - `TestOtherRoleIsUntouched`: 계정 X의 `reading`, `learned`, `writing`, `refreshed`, `forget`(모든 범위)을 어떤 `task`로 불러도 계정 Y의 `check` 결과가 바뀌지 않는다.
  - `TestConcurrentUse`: 여러 고루틴이 모든 메서드를 섞어 부른다(`-race`).
- [ ] **Step 2:** 돌려서 실패를 본다. `go test ./internal/gate/ -run 'Fresh|Newest|Pending|Unlock|OwnWrite|Late|RowGone|Older|Remember|Limits|OtherRole|Concurrent'`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** `go test -race ./internal/gate/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Gate: the memory of row versions read`.

---

### Task 8: 게이트 — 카탈로그, 조회, 읽기 쪽 잇기

스펙 4장의 "추적 대상", 5장의 "조회", 8장의 읽기 집계와 로그다.

**Files:** Create `internal/gate/stale.go`, `internal/gate/stale_test.go`. Modify `internal/gate/gate.go`(`New`, `Close`, `Inspect`, `Tracked`), `internal/gate/reads.go`(`Observe`, 버킷), `internal/gate/export_test.go`.

**Interfaces — Consumes:** Task 1 `config.Stale`, Task 2 `Result.ReadOnly`·`ReadKey`, Task 4 `ReadBucket.Keyed`·`Keys`, Task 5·6 `proxy.ReadKeys`, Task 7 `readMemory`. **Produces:**

```go
// in Gate:
stale *stale // nil when no table has stale

// StaleIdle waits until no lookup of row versions is queued or running, or
// ctx is done. Tests and the lab use it; nothing on a client's path does.
func (g *Gate) StaleIdle(ctx context.Context) error

const (
	staleQueue          = 1024                   // lookups waiting; past it a read is dropped and counted
	staleWorkers        = 2
	staleLookupTimeout  = 500 * time.Millisecond
	staleCatalogTimeout = 5 * time.Second
)
var staleCatalogEvery = time.Minute // a var so tests can shorten it

// tracked is what the catalog says of a table that remembers reads.
type tracked struct {
	oid      uint32
	attr     uint16        // the key column's attribute number
	typ      proxy.KeyType // of the key column's base type
	typeName string        // format_type of the key column, for a cast to it
}

// table is the catalog's entry for the config key table; ok is false when
// the table has no stale or is not tracked.
func (s *stale) table(key string) (t tracked, ok bool)

// lookupVersions reads, in one statement outside any transaction, the key
// text and xmin of the rows of table whose key is one of keys. A key may be
// written as an agent wrote it ('042'): the map's keys are the server's text.
func (s *stale) lookupVersions(ctx context.Context, table string, keys []string) (map[string]string, error)
// refresh queues a lookup of one row's version for a write that ran; then is
// called with the row's key as the server writes it and its xmin, with ok
// false when the row is not there, the lookup failed or the queue was full.
func (s *stale) refresh(table, key string, then func(serverKey, version string, ok bool))
```

**동작.**

1. `New`는 `stale`을 켠 테이블이 있으면 카탈로그를 한 번 읽고(`staleCatalogTimeout`) 작업자를 띄운다. 테이블마다 `to_regclass`로 찾은 OID, 키 컬럼의 번호, 키 타입(도메인이면 그 밑의 타입)을 둔다. 테이블이 없거나, 키 컬럼이 없거나, 키 타입이 여덟 타입이 아니면(S1) 그 테이블은 `Warn` 한 줄을 내고 추적하지 않는다. `New`는 그 때문에 실패하지 않는다. `staleCatalogEvery`마다 다시 읽어 바뀐 것만 로그에 낸다.
2. `Tracked`는 그 카탈로그에서 답한다(잠금 없이 읽는 스냅숏).
3. `Inspect`: `stale`을 켠 테이블이 있으면 분류기가 읽기를 찾는다. 읽는 테이블 가운데 `stale`을 켠 것이 있으면 `Observe`가 선다. `Track`은 거기에 더해 `r.ReadOnly`이고 `r.Cursors`가 없을 때 선다. `ReadOnly`는 `r.ReadOnly`다. `KeyParam`은 `r.ReadKey`가 `$n`이고 그 테이블이 `stale`을 켰을 때 n이다.
4. `Observe`: `stale`을 켠 테이블은 `reads`를 켜지 않아도 `stet.reads`에 센다(`log_above` 없음). 버킷에 `keyed`(그 테이블의 키를 하나 이상 얻은 읽기)와 `keys`를 더한다.
5. `Observe`가 기억으로 넘기는 읽기: 접속 계정을 알고, `Executed`이고 에러가 없고 `Lost`가 아니며, `o.Read != nil`이다. `Read.Keys`를 OID로 테이블에 나누고 겹친 키를 줄인 뒤, 테이블마다 `reading`으로 적고 조회를 대기열에 넣는다. 읽은 시각은 `Observe`가 불린 때의 Stet 시계다.
6. 조건절의 키(S5): `r.ReadKey`가 있고, 그 테이블의 키를 결과에서 얻지 못했고, 그 테이블의 OID가 `Read.Tables`에 있고, `o.Sent >= 1`이면 그 값 하나를 키로 쓴다. 값이 리터럴이면 그 글자, `$n`이면 `Read.Param`이다. 바이너리 값은 `Param.OID`가 0이면 키 컬럼의 타입으로, 아니면 그 OID가 여덟 타입 가운데 하나일 때만 `proxy.KeyOutput`으로 읽는다. NULL이거나 읽지 못하면 쓰지 않는다. 이 읽기는 키 없이(`keys == nil`) 적는다.
7. 대기열이 차 있으면 `dropped`로 끝내고 센다. `Observe`는 기다리지 않고 DB를 쓰지 않는다.
8. 작업자는 조회 하나에 `staleLookupTimeout`을 건다. 조회문은 Global Constraints의 것이고, 키들은 글자 매개변수 하나에 배열 값으로 묶어 보낸다(요소마다 큰따옴표로 감싸고 `\`와 `"`를 막는다). 성공하면 `learned`, 실패하면 `dropped`로 끝내고 센다.
9. 1분에 한 번, 버린 읽기(대기열), 실패한 조회, 상한으로 버린 기억의 수가 하나라도 0이 아니면 `Warn` 한 줄을 낸다.
10. `Close`는 작업자를 멈추고 남은 것을 `dropped`로 끝낸다. 닫힌 뒤의 `Observe`는 기억에 아무것도 하지 않는다.

- [ ] **Step 1: 실패하는 테스트** (`stale_test.go`. 자기 테이블을 만들고, 실제 OID로 `proxy.Outcome`을 꾸며 `Inspect`·`Observe`를 직접 부른다. 기억은 `export_test.go`의 헬퍼로 `check`해 본다).
  - `TestReadsAreRemembered`: 키 셋을 `Observe` → `StaleIdle` 뒤 그 계정·업무의 `check`가 각 행의 지금 `xmin`으로 `fresh`.
  - `TestInspectMarksTrackedReads`: `stale`을 켠 테이블의 `SELECT`는 `Observe`·`Track`·`ReadOnly`. `stale`도 `reads`도 없는 테이블의 `SELECT`는 둘 다 아니다. `DECLARE … CURSOR FOR SELECT`와 `COPY … TO`는 `Track`이 아니다. `select title from t where id = $1`은 `KeyParam == 1`.
  - `TestLookupKeyTypes`: 여덟 타입의 키 각각에서 조회가 맞는 행의 버전을 돌려준다. `'042'`로 넘긴 정수 키는 `42`로 돌아온다.
  - `TestLookupKeepsOddKeysApart`: 글자 키 `a,b`, `a"b`, `a\b`, `{a}`, 빈 글자, `NULL`, 앞뒤 공백이 있는 것, 음수 정수, 대문자로 적은 `uuid` → 각 키가 자기 행의 버전으로만 돌아온다.
  - `TestLookupQuotesNames`: 테이블 `"Deal Notes"`, 키 컬럼 `"Order"`, 설정 키 `crm.contacts`.
  - `TestWhereKeyFallback`: `select title from t where id = $1`(`Keys` 비어 있고 `Tables`에 OID, `Sent == 1`) → 그 행이 기억된다. 바이너리 `int4`·`int8`·`uuid`·`text` 값도 된다. `Sent == 0`이면 기억하지 않는다. 그동안의 `check`는 그 테이블의 어느 행이든 `read_pending`이다.
  - `TestSameNameInAnotherSchemaIsNotRemembered`: 다른 스키마의 같은 이름 테이블을 읽어 `Tables`가 비어 있으면 조건절의 키를 쓰지 않는다.
  - `TestReadsNotRemembered`: `o.Read == nil`, 에러가 난 읽기, `Lost`, 접속 계정을 모르는 세션.
  - `TestUntrackedTables`: 없는 테이블, 없는 키 컬럼, `date` 키, 두 컬럼 키의 설정 → 각각 경고 한 줄, `Tracked`는 거짓, `New`는 성공, 그 테이블의 읽기는 세기만 한다.
  - `TestCatalogIsReadAgain`: 추적하던 테이블을 지우고 다시 만든다 → 주기가 지난 뒤 `Tracked`가 새 OID에 답하고 옛 OID에는 답하지 않는다. 시작할 때 없던 테이블이 생긴 뒤에도 같다. 그 사이의 `Observe`와 쓰기는 실패하지 않는다.
  - `TestSlowLookupDelaysNothing`: 조회가 막혀 있어도(테스트 훅) `Observe`가 1ms 안에 돌아온다. 대기열을 채우면 넘친 읽기가 세어지고 로그에 남는다. 500ms를 넘긴 조회는 `dropped`로 끝난다.
  - `TestReadsCountKeys`: `stet.reads`의 `statements`, `keyed`, `keys`가 보낸 것과 같다. `reads`를 켜지 않은 `stale` 테이블도 센다.
  - `TestObserveAfterClose`: `Close` 뒤의 `Observe`가 패닉하지 않고 기억을 바꾸지 않는다.
- [ ] **Step 2:** 돌려서 실패를 본다. `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/`
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/`와 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Gate: remember the versions of rows agents read`.

---

### Task 9: 게이트 — 쓸 때의 확인, 판정, 자기 쓰기

스펙 6장, 7장, 8장의 판단 기록이다.

**Files:** Create `internal/gate/stale_write.go`, `internal/gate/stale_write_test.go`. Modify `internal/gate/gate.go`(`assemble`, `currentRow`, `input`, `Begin`, `End`, `judge`, `Unseen`, `TxEnded`), `internal/gate/enforce.go`(`act`, `facts`, `recheck`).

**Interfaces — Consumes:** Task 3 `decide.ReadCheck`·`Input.ReadCheck`·`Facts.RowGone`, Task 4 `Decision.ReadCheck`, Task 7 `readMemory`, Task 8 `stale`. **Produces:**

```go
// in pending:
readCheck *decide.ReadCheck // nil when the table has no stale
key       string            // the row's key as the server writes it; "" when not known
mark      *writeMark        // the write's own mark while it is on its way

// Applying tells the gate that Stet is about to run, on its own connection
// and as role, an approved write of table. done is called once it has
// committed or failed. Until then role's writes to table are own_write, and
// then what role remembers of table is forgotten. Safe after Close.
func (g *Gate) Applying(role, table string) (done func())
```

**동작.**

1. **상태 읽기(S11).** 카탈로그가 추적하는 테이블의 `currentRow`는 행이 없어도 한 줄을 돌려주는 모양으로 읽는다. `select to_jsonb(t)::text, t.xmin::text, k.key from (select $1::<키 타입> as key) as k left join <테이블> as t on t.<키> = k.key`. 키 타입은 카탈로그의 `typeName`이고, 셋째 컬럼이 서버가 쓴 키의 글자다. 추적하지 않는 테이블은 지금의 질의 그대로다. 행이 없을 때 `assemble`이 만드는 상태와 `stateVersion`은 지금과 같아야 한다.
2. **비교.** `assemble`이 현재 행을 읽은 뒤 `check`를 부른다(그래서 enforce의 다시 판단도 다시 비교한다). 대상은 `stale`을 켠 테이블의 `Gated` `UPDATE`·`DELETE`다. `not_checked`인 경우: 접속 계정을 모른다, 카탈로그가 그 테이블을 추적하지 않는다, 상태를 읽지 못했다, 키 값이 없다, `INSERT`와 `Measured`·`Unsupported` 쓰기, enforce가 상태를 읽기 전에 정한 쓰기(`unjudged`). `stale`을 켜지 않은 테이블은 `readCheck`가 nil이다.
3. **판정.** `input`이 `ReadCheck`를 넘긴다. 섀도의 `judge`와 enforce의 `verdict`가 같은 규칙을 쓴다. `stale`이면 모델을 부르지 않는다. `judge`는 어느 규칙이 정했든 `d.ReadCheck`를 적는다(S4).
4. **거절.** enforce의 `act`가 `ST003`을 돌려주고 `d.Refused == "ST003"`이다. `facts`의 `RowGone`은 `stale`이고 `Version == nil`일 때 참이다. `recheck`에서 다시 판단한 것이 `stale`이면 이유는 `["stale_read", "rejudged"]`이고 다시 버전을 읽지 않는다.
5. **표시.** `stale`을 켠 테이블에 쓰는, 접속 계정을 아는 쓰기가 서버로 갈 때(섀도는 언제나, enforce는 보내기로 정했을 때) 표시한다. 자기 비교가 끝난 뒤에 한다. 행 표시: `Gated` `UPDATE`·`DELETE`이고 행이 지금 있다. 표시 없음: `Gated` `UPDATE`·`DELETE`인데 행이 지금 없다, 키 값을 주지 않은 `Gated` `INSERT`. 모든 테이블 표시(S9): 이유가 `multi_statement`, `nested_write`, `with`, `truncate`. 테이블 표시: 그 밖의 모든 쓰기(키 값을 준 `INSERT` 포함).
6. **`End`.** 거절된 쓰기는 표시가 없다. 실행되지 않았으면(`Executed`도 `Lost`도 아님) `released`. 트랜잭션 밖에서 실행됐으면: 행 표시의 `UPDATE`는 `refresh`로 그 행의 버전을 조회하고 결과로 `refreshed`를 부른다(`ok`가 거짓이면 `forgotten`). `DELETE`는 `forgotten`. 키 값을 준 `INSERT`는 에이전트가 적은 키 글자로 `refresh`해 `refreshedRow`를 부르고 표시를 `released`로 끝낸다(`ok`가 거짓이면 `forgotten`). 그 밖의 표시는 `forgotten`. 트랜잭션 안에서 실행됐으면 표시를 그 세션의 것으로 적어 둔다. `Lost`이면 `forgotten`(S6).
7. **`TxEnded`.** 그 세션이 적어 둔 표시(트랜잭션 안에서 실행된 쓰기의 것)를 모두 `forgotten`으로 끝낸다. `unseen`이 0보다 크면 그 접속 계정의 기억을 모두 지우고, 그 세션의 남은 `Unseen` 수에서 뺀다. 남은 수가 0이 되면 그 세션의 `Unseen` 표시를 `forgotten`으로 끝낸다.
8. **`Unseen`.** 접속 계정을 알면 그 계정의 모든 테이블에 표시하고(세션마다 하나이고, 그 세션의 남은 `Unseen` 수를 센다) 그 계정의 기억을 모두 지운다. `TxEnded`가 수를 맞춰 끝낸다. 접속 계정을 모르는 세션의 `Unseen`과 그것을 정리하는 `TxEnded`는 아무것도 하지 않는다.
9. **`Applying`.** 테이블 표시를 하고, `done`에서 `forgotten`으로 끝낸다. 그 테이블이 `stale`을 켜지 않았으면 아무것도 하지 않는다.
10. 표시는 어떤 길로도 남지 않는다. 표시한 쓰기는 `End`가, 트랜잭션 안의 것은 `TxEnded`가, `Unseen`은 `TxEnded`가 끝낸다(프록시가 연결이 끊겨도 부른다).

- [ ] **Step 1: 실패하는 테스트** (`stale_write_test.go`. 실제 프록시를 거치는 이 패키지의 기존 헬퍼를 쓴다. 에이전트는 `stet_md5`, 다른 쪽은 `stet_scram`이나 superuser의 직접 연결. 표의 순서는 스펙 11장과 같고, "조회가 끝난 뒤"는 `StaleIdle`이다. 두 모드에서 돈다고 적지 않은 것은 섀도다).
  - `TestStaleWrite` (두 모드): 읽기 → 다른 쪽이 변경 → 쓰기. 섀도: 실행되고 `verdict == "reject"`, `reasons == ["stale_read"]`, `read_check.result == "stale"`. enforce: 실행되지 않고 클라이언트가 `ST003`, 스펙 6장의 MESSAGE·DETAIL·HINT를 받고 `refused == "ST003"`.
  - `TestStaleAcrossConnections`: 연결 A로 읽고 같은 계정의 연결 B로 쓴다 → `stale`.
  - `TestFreshWrite`: 변경 없음 → `fresh`, 판정은 지금과 같다. 다른 행이 바뀐 것은 영향이 없다.
  - `TestWriteWithoutRead`: `no_read`.
  - `TestDeletedRowIsStale` (enforce): 읽은 뒤 다른 쪽이 지움 → `stale`, `version`이 null, 문장은 `the row was deleted after it was read`. 읽은 적 없는 없는 행에 쓰는 것은 `no_read`다.
  - `TestKeyWrittenOtherwise`: `id = 42`를 읽고 `where id = '042'`와 `where id = 42::bigint`로 쓴다 → 읽은 행과 맞는다(`fresh`, 변경 뒤에는 `stale`).
  - `TestWriteWhileLookupPending`: 조회를 막아 두고 다시 읽은 뒤 곧바로 쓴다 → `read_pending`이고 판정되지 않는다.
  - `TestOwnWrite`: 읽기 → 자기 쓰기 → 조회가 끝난 뒤 자기 쓰기 → 둘째가 `fresh`. 조회를 막아 두면 둘째가 `own_write`.
  - `TestOwnWriteFromAnotherTask`: 연결 A(업무 `a`)로 읽기 → 연결 B(업무 `b`)로 그 행에 쓰기 → 연결 A로 쓰기 → `stale`이 아니다.
  - `TestOwnWriteInTransaction`: 읽기 → `BEGIN`, 쓰기 → (커밋 전) 다른 연결의 그 행 쓰기는 `own_write`(서버에서 행 잠금을 기다리므로 다른 고루틴에서 보내고, `COMMIT` 뒤에 기록을 본다) → `COMMIT` → 쓰기는 `no_read`. 트랜잭션 안의 쓰기보다 먼저 끝났고 조회가 늦은 읽기가 기억을 되살리지 않는다.
  - `TestMultiRowWriteForgetsTable`: 읽기 → 자기의 여러 행 `UPDATE` → 그 행에 쓰기 → `no_read`. 여러 행 쓰기가 서버에 가 있는 동안의 쓰기는 `own_write`. `with` 모양의 쓰기는 다른 테이블의 기억도 지운다.
  - `TestInsertWithKey`: 읽기 → 다른 쪽이 그 행을 지움 → 자기가 같은 키로 `INSERT` → 그 행에 쓰기 → `stale`이 아니다. 키 없는 `INSERT`는 다른 행의 기억을 지우지 않는다.
  - `TestLostWriteLeavesNoMark`: 쓰기를 보낸 뒤 답이 오기 전에 연결을 끊는다 → 그 계정의 다음 쓰기가 `own_write`가 아니고, 끊긴 쓰기가 바꾼 행에 쓰는 것이 `stale`이 아니다(`no_read`).
  - `TestOnBehalfOf`: P7(업무 `a` 읽기 → 변경 → 업무 `b` 읽기 → `a` 쓰기)은 `stale`, `read_on_behalf_of == "a"`. M1(둘 다 업무 없음)은 `fresh`.
  - `TestUnlockAfterRejection` (enforce): `stale`로 거절 → 다시 보내면 또 거절 → 같은 계정의 다른 업무 연결로 다시 읽기 → 쓰기가 실행된다.
  - `TestStaleBeforeCumulative` (enforce): 누적 한도를 넘은 쓰기가 낡았으면 `reject`, `stale_read`이고 보류되지 않는다(`stet.held_writes`에 줄이 없다).
  - `TestStaleNeverAsksTheModel` (두 모드): 질문이 있는 테이블에서 `stale`인 쓰기 → 테스트 판단 모델이 불리지 않는다.
  - `TestRowChangedWhileJudged` (enforce): 읽은 기억이 있는 쓰기를 판단하는 동안(`rowVersion` 훅) 행이 바뀐다 → `reject`, `["stale_read", "rejudged"]`, `ST003`. 읽은 기억이 없으면 지금처럼 다시 판단한다.
  - `TestInTransactionComesFirst`: 더러운 트랜잭션 안의 낡은 쓰기 → 판정은 `unsupported`, `in_transaction`이고 `read_check.result == "stale"`이다(섀도). enforce에서는 `not_checked`다.
  - `TestOtherRoleCannotMakeStale`: 계정 X가 그 행을 읽고, 쓰고, 어떤 `stet.on_behalf_of`를 적어도, 계정 Y가 방금 읽은 행에 쓰는 것은 X가 바꾸지 않는 한 `fresh`다. X가 Y의 업무 이름을 적고 읽어도 Y의 낡은 쓰기는 `stale`이다.
  - `TestUnseenForgets` (섀도): 읽기 → `DO` 블록으로 그 행을 고침 → 쓰기 → `no_read`. `DO`가 가 있는 동안의 쓰기는 `own_write`. `Unseen` 둘 뒤에 `TxEnded(…, 1)`이 오면 표시가 남고(`own_write`), 다음 `TxEnded(…, 1)`에서 끝난다.
  - `TestApplying`: 읽기 → `done := g.Applying(role, table)` → 그동안 쓰기는 `own_write` → `done()` → `no_read`.
  - `TestNotChecked`: `INSERT`, 여러 행 쓰기, 접속 계정을 모르는 세션, 추적하지 않는 `stale` 테이블(키 타입 `date`) → `not_checked`. `stale`을 켜지 않은 테이블 → `read_check`가 NULL.
  - `TestRowAbsentStateUnchanged`: 추적하는 테이블에서 없는 행에 쓰는 것의 `state`와 `state_version`이 `stale`을 켜기 전과 같다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/`, 지연 테스트, 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Gate: refuse a write of a row changed since its role read it`.

---

### Task 10: 승인 실행과 `stet` 잇기

**Files:** Modify `internal/apply/apply.go`, `cmd/stet/main.go`. Test `internal/apply/apply_test.go`, `cmd/stet/main_test.go`.

**Interfaces — Consumes:** Task 9 `Gate.Applying`. **Produces:**

```go
// in Applier:
// Applying, when set, is called once a held write's checks have passed, just
// before its statement runs, with the agent's role and the table; the func
// it returns is called once the transaction has committed or failed. Set it
// before Start.
Applying func(role, table string) (done func())
```

- [ ] **Step 1: 실패하는 테스트.**
  - `internal/apply`: `TestApplyingAroundTheStatement`: 적용된 쓰기 → `Applying(역할, 테이블)`이 문장 전에 한 번, `done`이 커밋 뒤에 한 번. 문장이 실패해 `failed`로 적힌 쓰기도 `done`이 불린다. 검사에서 걸린 쓰기(`row_changed` 등)는 `Applying`이 불리지 않는다. `Applying`이 nil이면 지금과 같다.
  - `cmd/stet` (`run`을 거치는 기존 헬퍼): `TestStaleEndToEnd`: `stale`을 켠 설정으로 섀도에서 읽기 → 다른 쪽이 변경 → 쓰기 → `stet.decisions`에 `read_check.result == "stale"`, `reasons == ["stale_read"]`, `executed`. enforce에서 같은 순서 → 클라이언트가 `ST003`.
  - `cmd/stet`: `TestApprovedWriteIsNotStale` (enforce): 읽기 → 누적 한도로 보류된 쓰기 → 검토자가 `stet.approve` → 적용됨 → 그 행에 쓰기의 `read_check.result == "no_read"`.
  - `cmd/stet`: `TestStaleTableNotTrackedStillStarts`: `stale`을 켠 테이블의 키가 `date`다 → 시작하고, 경고가 한 줄 나오고, 그 테이블의 쓰기는 `not_checked`다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다. `openGate`가 실행부를 시작하기 전에 `applier.Applying = g.Applying`을 잇는다. 닫는 순서는 지금 그대로다(게이트가 먼저 닫히므로 `Applying`은 닫힌 뒤에도 안전해야 한다).
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 커밋 `Stet: approved writes clear what their role remembers`.

---

### Task 11: 실험실 — `bench stale-check`

스펙 11장이다.

**Files:** Create `internal/lab/bench/stale.go`, `internal/lab/bench/stale_result.go`, `internal/lab/bench/stale_test.go`. Modify `cmd/stet-lab/bench.go`, `cmd/stet-lab/bench_test.go`, `docs/lab.md`.

**Interfaces — Produces:**

```go
// StaleCheck says what CheckStale runs on.
type StaleCheck struct {
	Admin    Admin  // the server's superuser: it creates and drops the clones, and nothing else
	Names    Names  // the roles, used as they are: none is created, altered or given a membership
	Template string // the benchmark template the clones are made from; it is only read
	Database string // the clones are Database_shadow and Database_enforce; one that exists is refused
	Keep     bool
	Progress func(line string)
}

// StaleRow is one scenario of spec-v0.2-stale 11 in one mode.
type StaleRow struct {
	ID, Mode string   // "P1" ... "N11"; "shadow" or "enforce"
	Want     []string // the read_check results the spec allows
	Got      string   // read_check.result of the scenario's last write
	Verdict  string
	Reasons  []string
	Executed bool
	Code     string // the SQLSTATE the client got; "" when the write ran
	OK       bool
}

type StaleResult struct{ Rows []StaleRow }

func CheckStale(ctx context.Context, c StaleCheck) (StaleResult, error)
func (r StaleResult) Write(w io.Writer) error // a Markdown table, a row per scenario and mode
func (r StaleResult) Failed() int
```

**동작.**

- 모드마다 템플릿의 복제본 하나를 만든다. 그 안에서 Stet을 이 프로세스 안에 띄운다: Stet의 계정(`Names.Gate()`)의 풀, `store.Migrate`, `gate.New`, `proxy.New`(임의의 로컬 포트), enforce에서는 `apply.New`와 `Applying` 잇기. 설정은 코드에서 만든다: `contacts`와 `deals`에 `stale`, 모델은 `record_only`, 질문과 정책 없음, `agents.roles`는 에이전트 계정 넷, `contacts`에 `impact: {max_rows: 100}`(N6의 여러 행 쓰기가 enforce에서도 실행되게), `deals`에 N10을 위한 누적 한도(`impact: {max_rows: 1, cumulative: {max_rows: 1, window: 1m}}`).
- 줄마다 소유자(`Names.Owner()`)가 Stet을 거치지 않고 새로 넣은 자기 행에서 돈다. 줄끼리 행을 나누지 않는다.
- 에이전트의 문장은 Stet을 거쳐 `agent_v`로 보낸다. P1의 "다른 에이전트"는 `agent_i`다. P2·P6의 변경은 소유자가 Stet을 거치지 않고 한다. 연결 A·B는 같은 계정의 두 연결이다. 업무는 그 연결에서 `set stet.on_behalf_of = '…'`로 적는다. P4는 `select title from contacts where id = $1`, P5는 `contacts`와 `deals`를 별칭으로 조인한 읽기다.
- P·M의 줄과 "곧바로"가 없는 N의 줄은 읽기와 자기 쓰기 뒤에 `Gate.StaleIdle`을 기다린다. N5·N8은 기다리지 않는다.
- N10: `deals`의 행을 읽고, 다른 `deals` 행에 한 번 써서 한도를 채우고, 읽은 행에 쓴다(보류된다). 소유자가 복제본 안에서 `stet.approve`를 부른다(검토자 권한은 복제본 안에서 `undo-check`가 한 방식으로 준다). `stet.held_writes`가 `applied`가 될 때까지 기다린 뒤 그 행에 쓴다.
- 줄의 결과는 마지막 쓰기의 `stet.decisions` 줄에서 읽는다. 문장에 줄마다 다른 주석 표지를 넣어 찾고, 줄이 생길 때까지 5초까지 기다린다.
- `OK`: `Got`이 `Want`에 있다. P의 줄은 섀도에서 `Executed`이고 `Reasons`에 `stale_read`, enforce에서 실행되지 않고 `Code == "ST003"`. N5는 실행된다.
- 복제본은 끝나면 지운다(`Keep`이면 둔다). 템플릿과 역할은 건드리지 않는다.
- `stet-lab bench stale-check [-name crm_stale_check] [-keep]`: 표를 표준 출력에 쓰고, `Failed() > 0`이면 0이 아닌 코드로 끝난다.

- [ ] **Step 1: 실패하는 테스트.** `stale_test.go`: `TestCheckStale`가 테스트용 템플릿(이 패키지의 기존 테스트가 템플릿을 만드는 방식)에서 두 모드를 돌려 열아홉 줄(P1~P7, M1, N1~N11) 가운데 그 모드에 해당하는 줄이 모두 `OK`이고, M1이 `fresh`이며, 복제본이 남지 않는다. `TestStaleResultWrite`: 표의 머리와 한 줄의 모양. `cmd/stet-lab`: `bench` 도움말에 `stale-check`가 있다.
- [ ] **Step 2:** 돌려서 실패를 본다.
- [ ] **Step 3:** 구현한다.
- [ ] **Step 4:** 전체 명령이 통과한다.
- [ ] **Step 5:** 테스트 서버의 벤치마크 템플릿에서 실제로 돌린다: `go build -o bin/stet-lab ./cmd/stet-lab && bin/stet-lab bench stale-check`. 템플릿이 없으면 만들지 말고 그 사실을 보고한다. 결과 표를 `docs/lab.md`에 "낡은 쓰기" 절로 더한다(`## 되돌리기` 다음). 그 절에는 켜는 방법(설정 두 줄), 재는 질의(스펙 8장), 돌린 명령과 커밋, 결과 표를 적는다. 기대와 다른 줄이 있으면 그대로 적는다.
- [ ] **Step 6:** 커밋 `Lab: check stale-write detection end to end`.

---

### Task 12: 문서 — 만든 대로의 스펙

**Files:** Modify `docs/spec-v0.2-stale.md`, `docs/spec-v0.1.md`, `docs/spec-v0.2-enforce.md`. Create `internal/gate/stale_memory_bench_test.go`.

- [ ] **Step 1:** 메모리를 잰다. `BenchmarkMemoryPerKey`가 200,000개의 자리(정수 키, 업무 하나)를 채운 뒤 힙이 늘어난 크기를 키 하나당 바이트로 보고한다. `go test -bench MemoryPerKey -run xxx ./internal/gate/`의 값을 적어 둔다.
- [ ] **Step 2:** `docs/spec-v0.2-stale.md`를 만든 대로 고친다. 상태 줄을 "구현 (날짜)"로 바꾼다. "스펙 보정" S1~S11을 해당 장의 본문에 옮긴다(4장: S1·S2·S5, 5장: S8, 6장: S11, 7장: S3·S6·S7·S9·S10, 8장: S4, 12장: 각 보정이 더한 놓침). 13장의 표를 실제로 바뀐 파일에 맞춘다. 14장에 Task 6의 지연과 Step 1의 메모리 값을 적는다. 15장의 "바이너리로 온 지원하지 않는 타입" 항목을 S1에 맞게 고친다. 16장의 "작성자가 정한 것"에 보정을 더하고, "남긴 것"에 ASCII가 아닌 글자 키와 설정에 따라 글자가 달라지는 키 타입을 더한다. 15장의 체크는 하지 않는다(마지막 검토 뒤에 한다).
- [ ] **Step 3:** `docs/spec-v0.1.md`와 `docs/spec-v0.2-enforce.md`에서 `ST003`을 "내지 않는다"고 적은 줄에, 낡은 쓰기 탐지가 이 코드를 낸다는 한 구절과 이 스펙으로 가는 링크를 더한다. 다른 곳은 고치지 않는다.
- [ ] **Step 4:** 소유자의 문서(README.md, docs/concept-v0.2.md, docs/decisions.md)에서 이 단계로 낡아진 줄을 찾아, 고치지 않고 파일과 줄 번호로 보고에 적는다.
- [ ] **Step 5:** 전체 명령이 통과한다.
- [ ] **Step 6:** 커밋 `Docs: stale-write detection as built`.
