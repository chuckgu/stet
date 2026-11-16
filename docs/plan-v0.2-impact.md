# 영향 측정 (섀도) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 게이트 대상 테이블에 대한 다중 행 쓰기가 몇 행에 걸리는지를 실행 전에 DB에 물어 기록하고, 테이블별 한도로 결정론적 verdict를 낸다. 섀도 모드에서만 동작한다.

**Architecture:** 분류기(`sqlparse`)가 다중 행 쓰기를 `Measured`로 분류하고 측정 방법을 정한다. 게이트는 `Begin`에서 Stet 자신의 연결로 읽기 전용 트랜잭션을 열어 세거나 추정하고, 프록시는 실행 뒤의 실제 행 수를 넘겨준다. verdict는 `decide`의 결정론 단계가 내고, 결과는 `stet.decisions.impact`에 남는다. 모델은 부르지 않는다.

**Tech Stack:** Go 1.27, `github.com/jackc/pgx/v5` v5.11.0 (`pgconn`, `pgproto3`), `github.com/pganalyze/pg_query_go/v6` v6.2.5, 테스트용 PostgreSQL 14+ (`scripts/testdb.sh`).

**Spec:** [docs/spec-v0.2-impact.md](spec-v0.2-impact.md). 상위 문서는 [docs/concept-v0.2.md](concept-v0.2.md)다. 실행하는 사람은 둘 다 읽는다.

**이 계획에 없는 것:** decisions.md D30이 1번 단계에 더한 두 가지(읽는 문장의 결과 행 수 기록, 접속 계정별 누적 한도)는 스펙에 설계가 아직 없다. 설계가 나온 뒤 별도 계획으로 만든다.

## Global Constraints

- CLAUDE.md의 규칙 일곱 개를 지킨다. 특히 규칙 6: 섀도 모드는 원래 쓰기를 바꾸지 않는다. 문장 바이트를 고치지 않고, 측정이 어떻게 실패해도 쓰기는 그대로 통과한다.
- `impact` 설정이 없는 테이블의 기록은 이 변경 전과 같다. `internal/gate`, `internal/proxy`, `internal/store`의 기존 테스트는 고치지 않고 통과해야 한다. 고치는 기존 테스트는 `classify_test.go`의 Kind 기대값뿐이다.
- 기록되는 reason 문자열은 지금의 검사 순서가 내는 값 그대로다.
- 모델에 보내는 상태(`gate.State`)는 건드리지 않는다.
- 스펙의 값: `cap = max_rows + 1`, 세기의 `statement_timeout`은 200ms, `Begin` 전체 예산은 기존 `stateTimeout`(500ms), 방법 이름은 `count`·`estimate`·`syntax`, reason은 `impact:rows`·`impact:share`·`impact:uncertain`.
- 측정은 `BEGIN READ ONLY`와 `SET LOCAL ROLE <클라이언트 접속 계정>` 안에서 한다. `SET ROLE`이 실패하면 측정 실패다. 역할을 바꾸지 않고 다시 시도하지 않는다.
- 세기는 조건식의 함수 호출이 모두 허용 목록(`now`, `lower`, `upper`, `length`, `date_trunc`, `abs`)에 있을 때만 한다. 이름은 스키마가 없거나 `pg_catalog`여야 한다. 스키마를 붙인 연산자(`OPERATOR(...)`)가 있으면 세지 않는다.
- 테스트: `scripts/testdb.sh start <dir>` 뒤 `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`. 이 변수가 없으면 통합 테스트가 건너뛰어지므로 반드시 설정한다. `internal/proxy/latency_test.go`의 2ms 예산도 통과해야 한다.
- 코드의 주석과 식별자는 영어, 문서는 한국어다(기존 관례).
- `main`에 직접 커밋하지 않는다. 브랜치 `v0.2-impact`에서 작업한다. 커밋 메시지는 저장소의 형식(한국어 한 줄 요약)을 따르고 끝에 `Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>`를 붙인다.

## Review Focus

스펙이 암시하지만 놓치기 쉬운 입력이다. 각 줄의 테스트는 괄호 안의 Task에 들어 있다.

1. **NULL 바인딩 값.** `where company_id = $1`에 NULL이 오면 0행으로 세고 `commit`이다. NULL이 빈 문자열로 바뀌면 안 된다. (Task 6)
2. **한 번도 분석되지 않은 테이블.** `reltuples`가 0 이하면 `share`를 비우고 `max_share`를 적용하지 않는다. 0으로 나누지 않는다. (Task 4, Task 6)
3. **0행에 걸리는 쓰기.** `rows`와 `actual_rows`가 0으로 기록된다. 값이 없는 것과 구분된다. (Task 3, Task 6)
4. **실행이 실패하는 측정 대상 쓰기.** 다중 행 `INSERT`가 PK 충돌로 실패해도 클라이언트가 받는 에러는 같고, 기록에는 측정값이 있고 `actual_rows`는 없다. (Task 7)
5. **동시에 들어오는 측정 대상 쓰기.** 여러 세션이 동시에 측정해도 경쟁이 없다(`-race`). (Task 7)

## File Structure

| 파일 | 책임 |
|---|---|
| `internal/config/config.go` | `impact` 블록 파싱과 검증 (수정) |
| `internal/sqlparse/classify.go` | `Measured` 종류와 결과 타입, 호출 지점 (수정) |
| `internal/sqlparse/measure.go` | 측정 자격 판정, 방법 선택, 세기용 `SELECT` 만들기 (신규) |
| `internal/proxy/session.go` | 실제 행 수와 트랜잭션 상태를 게이트에 전달 (수정) |
| `internal/decide/impact.go` | `Impact` 타입과 영향 verdict (신규) |
| `internal/decide/decide.go` | `Input` 필드 추가, `Verdict`에서 호출 (수정) |
| `internal/store/schema.sql`, `store.go` | `impact` 컬럼 (수정) |
| `internal/gate/impact.go` | DB에 묻는 측정 (신규) |
| `internal/gate/gate.go` | `Begin`·`judge`에 연결 (수정) |
| `internal/gate/export_test.go` | 테스트에서 세기 시간 제한을 바꾸는 훅 (신규) |
| `internal/gate/impact_test.go` | 측정 통합 테스트 (신규) |

---

### Task 0: 브랜치와 문서 커밋

**Files:** `README.md`, `docs/decisions.md`, `docs/concept-v0.2.md`, `docs/spec-v0.2-impact.md`, `docs/plan-v0.2-impact.md`

- [ ] **Step 1: 브랜치를 만든다**

Run: `git switch -c v0.2-impact`

- [ ] **Step 2: 방향 문서와 스펙, 이 계획을 커밋한다**

```bash
git add README.md docs/decisions.md docs/concept-v0.2.md docs/spec-v0.2-impact.md docs/plan-v0.2-impact.md
git commit -m "v0.2 방향: 상태를 아는 에이전틱 쿼리 통제, 영향 측정 스펙과 계획" \
  -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

- [ ] **Step 3: 시작 상태를 확인한다**

Run: `scripts/testdb.sh start /tmp/stet-pg && STET_TEST_PG=127.0.0.1:55432 go test -race ./...`
Expected: 모든 패키지 `ok`. 이후 Task는 이 상태에서 시작한다.

---

### Task 1: 설정의 `impact` 블록

**Files:**
- Modify: `internal/config/config.go` (`Table`, `rawTable`, `resolveTable`)
- Modify: `stet.example.yaml`
- Test: `internal/config/config_test.go`

**Interfaces:**
- Produces:
  ```go
  // Impact limits how much one multi-row write may touch.
  type Impact struct {
      MaxRows  int     // required, at least 1
      MaxShare float64 // optional; 0 means unset. Applies to update and delete only
  }
  // in Table:
  Impact *Impact // nil: multi-row writes stay unsupported, as before
  ```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
func TestImpactLimits(t *testing.T) {
	base := minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables:\n  contacts:\n    pk: id\n    send_fields: [name]\n"
	parse := func(extra string) *Impact {
		c, err := Parse([]byte(base + extra))
		if err != nil {
			t.Fatal(err)
		}
		return c.Tables["contacts"].Impact
	}
	if got := parse(""); got != nil {
		t.Errorf("no impact block: %+v, want nil", got)
	}
	if got := parse("    impact: {max_rows: 100, max_share: 0.1}\n"); got == nil || *got != (Impact{MaxRows: 100, MaxShare: 0.1}) {
		t.Errorf("got %+v", got)
	}
	if got := parse("    impact: {max_rows: 5}\n"); got == nil || *got != (Impact{MaxRows: 5}) {
		t.Errorf("max_rows only: %+v", got)
	}
}

func TestExampleConfigShowsImpact(t *testing.T) {
	c, err := Load("../../stet.example.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := c.Tables["contacts"].Impact; got == nil || *got != (Impact{MaxRows: 100, MaxShare: 0.1}) {
		t.Errorf("example impact = %+v", got)
	}
}
```

`TestInvalidConfigs`의 표에 다음을 더한다(앞부분은 기존 항목과 같은 `minimal + "model: {adapter: jev, version: jev-1.13.0}\ntables: {contacts: {pk: id, ...}}\n"` 형태).

| 이름 | `impact` 값 | 에러에 들어가야 할 문자열 |
|---|---|---|
| `impact needs max_rows` | `{max_share: 0.1}` | `impact.max_rows` |
| `impact max_rows below 1` | `{max_rows: 0}` | `impact.max_rows` |
| `impact max_share zero` | `{max_rows: 1, max_share: 0}` | `impact.max_share` |
| `impact max_share above 1` | `{max_rows: 1, max_share: 1.5}` | `impact.max_share` |
| `impact unknown key` | `{max_rows: 1, window: 5m}` | `window` |

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/config/ -run 'Impact|InvalidConfigs'`
Expected: FAIL (`Impact` 미정의).

- [ ] **Step 3: 구현한다**

`rawTable`에 포인터 필드로 받는다. 블록이 있는지와 `max_share: 0`을 명시했는지를 구분해야 하기 때문이다.

```go
Impact *struct {
	MaxRows  *int     `yaml:"max_rows"`
	MaxShare *float64 `yaml:"max_share"`
} `yaml:"impact"`
```

에러 형식은 기존 `fail(field, ...)`을 쓴다: `tables.<name>.impact.max_rows: required`, `... must be at least 1`, `tables.<name>.impact.max_share: <v> must be above 0 and at most 1`.

`stet.example.yaml`의 `contacts` 테이블에 `unsupported: reject` 앞에 넣는다.

```yaml
    impact:                  # 없으면 다중 행 쓰기는 unsupported로 기록된다
      max_rows: 100          # 필수. 이보다 많이 걸리면 hold
      max_share: 0.1         # 선택. UPDATE·DELETE가 테이블의 10%를 넘게 걸리면 hold
```

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/config/`
Expected: PASS. `TestLabConfigIsValid`도 그대로 통과한다(lab 설정은 건드리지 않는다).

- [ ] **Step 5: 커밋**

```bash
git add internal/config stet.example.yaml
git commit -m "설정: 테이블별 영향 한도 impact (max_rows, max_share)" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

---

### Task 2: 분류기의 `Measured`

**Files:**
- Modify: `internal/sqlparse/classify.go` (`Kind`, `Result`, `Classify`)
- Create: `internal/sqlparse/measure.go`
- Modify: `internal/gate/gate.go` (`judge`의 첫 분기 한 줄)
- Test: `internal/sqlparse/classify_test.go`

**Interfaces:**
- Produces:
  ```go
  const Measured Kind = 3 // after Unsupported; String() returns "measured"

  type Method string
  const (
      MethodCount    Method = "count"
      MethodEstimate Method = "estimate"
      MethodSyntax   Method = "syntax"
  )

  // Measure says how to learn how many rows a multi-row write touches.
  type Measure struct {
      Method   Method
      CountSQL string // MethodCount: "SELECT 1 FROM <relation> [WHERE ...]", no LIMIT
      Rows     int    // MethodSyntax: the number of VALUES lists
  }

  // in Result:
  Measure *Measure // set exactly when Kind is Measured
  // Reason: for Measured, the reason the write had before this change
  ```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`TestClassifyKinds`에서 다음 행의 Kind만 `Unsupported`에서 `Measured`로 바꾼다. reason은 그대로다: `multi row insert`, `insert select`, `range update`, `update all rows`, `pk or other`, `pk inside an or`, `pk in list`, `pk compared to column`, `where current of`, `delete all rows`, `pk equals null`, `subquery in where`. 나머지 행은 바꾸지 않는다. 특히 `update from`, `delete using`, `subquery in values`, `subquery in set`, `with on gated write`, `upsert`는 단일 행이거나 값을 읽을 수 없는 형태라 `Unsupported`로 남는다. 같은 테스트의 불변식 검사에 다음을 더한다.

```go
if (r.Kind == Measured) != (r.Measure != nil) {
	t.Errorf("Measure = %+v for kind %v; want a Measure exactly when Measured", r.Measure, r.Kind)
}
```

새 테스트:

```go
func TestClassifyMeasured(t *testing.T) {
	cases := []struct {
		name, sql string
		reason    string
		want      Measure
	}{
		{"range update", "update contacts set status = 'churned' where company_id = 3", "not_single_row",
			Measure{Method: MethodCount, CountSQL: "SELECT 1 FROM contacts WHERE company_id = 3"}},
		{"no where", "delete from contacts", "not_single_row",
			Measure{Method: MethodCount, CountSQL: "SELECT 1 FROM contacts"}},
		{"only, schema, alias, unused $1", "update only public.contacts as c set title = $1 where c.company_id = $2", "not_single_row",
			Measure{Method: MethodCount, CountSQL: "SELECT 1 FROM ONLY public.contacts c WHERE c.company_id = $2"}},
		{"in list, trailing semicolon", "delete from contacts where id in (1, 2, 3);", "not_single_row",
			Measure{Method: MethodCount, CountSQL: "SELECT 1 FROM contacts WHERE id IN (1, 2, 3)"}},
		{"listed function", "delete from contacts where lower(email) = $1", "not_single_row",
			Measure{Method: MethodCount, CountSQL: "SELECT 1 FROM contacts WHERE lower(email) = $1"}},
		{"unlisted function", "update contacts set status = 'x' where set_config('role', 'none', true) is not null", "not_single_row",
			Measure{Method: MethodEstimate}},
		{"pg_catalog function not on the list", "delete from contacts where trim(name) = 'x'", "not_single_row",
			Measure{Method: MethodEstimate}},
		{"schema-qualified function", "delete from contacts where public.f(id)", "not_single_row",
			Measure{Method: MethodEstimate}},
		{"schema-qualified operator", "delete from contacts where company_id operator(public.=) 3", "not_single_row",
			Measure{Method: MethodEstimate}},
		{"where current of", "update contacts set title = 'x' where current of c", "not_single_row",
			Measure{Method: MethodEstimate}},
		{"join", "update contacts set title = l.title from leads l where contacts.company_id = l.id", "join",
			Measure{Method: MethodEstimate}},
		{"subquery", "update contacts set title = 'x' where company_id in (select id from companies)", "subquery",
			Measure{Method: MethodEstimate}},
		{"with", "with x as (select 1) update contacts set status = 'x' where company_id = 3", "with",
			Measure{Method: MethodEstimate}},
		{"with writing another table", "with d as (delete from other returning id) update contacts set status = 'x' where company_id = 3", "with",
			Measure{Method: MethodEstimate}},
		{"multi row insert", "insert into contacts (name) values ('a'), ('b'), ('c')", "multi_row",
			Measure{Method: MethodSyntax, Rows: 3}},
		{"multi row insert with a subquery", "insert into contacts (company_id) values ((select 1)), (2)", "multi_row",
			Measure{Method: MethodEstimate}},
		{"insert select", "insert into contacts (name) select name from leads", "insert_select",
			Measure{Method: MethodEstimate}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Measured || r.Reason != tc.reason || r.Measure == nil || *r.Measure != tc.want {
				t.Errorf("got %v/%q %+v, want measured/%q %+v", r.Kind, r.Reason, r.Measure, tc.reason, tc.want)
			}
		})
	}
}

// Shapes that must not become Measured, with the reason they keep.
func TestClassifyStaysUnsupported(t *testing.T) {
	cases := map[string]struct{ sql, reason string }{
		"multi row upsert":               {"insert into contacts (email) values ('a'), ('b') on conflict (email) do nothing", "multi_row"},
		"multi assign over many rows":    {"update contacts set (title, status) = ('x', 'y') where company_id = 3", "multi_assign"},
		"indirection over many rows":     {"update contacts set tags[1] = 'x' where company_id = 3", "indirection"},
		"single row with subquery":       {"update contacts set title = (select title from leads limit 1) where id = 1", "subquery"},
		"single row with join":           {"update contacts set title = l.title from leads l where contacts.id = 1", "join"},
		"single row insert with a with":  {"with x as (select 1) insert into contacts (name) values ('a')", "with"},
		"cte also writes the gated table": {"with d as (delete from contacts where company_id = 9 returning id) update contacts set status = 'x' where company_id = 3", "with"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r, err := Classify(tc.sql, gated)
			if err != nil {
				t.Fatal(err)
			}
			if r.Kind != Unsupported || r.Reason != tc.reason || r.Measure != nil {
				t.Errorf("got %v/%q %+v, want unsupported/%q", r.Kind, r.Reason, r.Measure, tc.reason)
			}
		})
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/sqlparse/`
Expected: FAIL (`Measured` 미정의).

- [ ] **Step 3: 구현한다**

`classifyInsert`·`classifyUpdate`·`classifyDelete`는 고치지 않는다. reason이 지금과 같아야 하기 때문이다. `Classify`의 단일 문장 경로에서 `classifyStmt` 다음에 `measure.go`의 함수를 부른다.

```go
// measured upgrades an Unsupported result to Measured when the only thing
// wrong with the write is that it may touch many rows. The reason is kept,
// so a table without impact limits records what it recorded before.
func measured(n *pg.Node, m matchFunc, r Result) Result
```

`measured`가 `Measured`를 돌려주는 조건(스펙 3장). 하나라도 어긋나면 `r`을 그대로 돌려준다.

1. `r.Kind == Unsupported`이고 reason이 `not_single_row`, `join`, `subquery`, `with`, `multi_row`, `insert_select` 중 하나다.
2. 문장 트리 전체에서 게이트 대상 테이블에 쓰는 문장이 정확히 하나다. `firstGatedWrite`와 같은 걷기로 센다.
3. `INSERT`: `OnConflictClause`가 없고 대상 컬럼에 indirection이 없다. `UPDATE`: `MultiAssignRef`와 indirection이 없다.
4. 단일 행이 아니다. `UPDATE`·`DELETE`: `pkEquality`가 실패한다. `INSERT`: `VALUES` 목록이 둘 이상이거나 `VALUES`가 아닌 `SELECT`다.

방법:

- `UPDATE`·`DELETE`이고 `FromClause`·`UsingClause`·`WithClause`가 없고 `hasSubLink`가 거짓이며, `WHERE`에 `CurrentOfExpr`가 없고, `WHERE`의 모든 `FuncCall` 이름이 허용 목록에 있고(이름이 한 부분이거나 `pg_catalog.<이름>`), 이름이 두 부분 이상인 `A_Expr` 연산자가 없으면 `MethodCount`.
- `INSERT`이고 `VALUES` 목록이 둘 이상이며 `WithClause`가 없고 `hasSubLink`가 거짓이면 `MethodSyntax`, `Rows`는 목록 수.
- 그 밖은 `MethodEstimate`.

`CountSQL`은 문자열을 자르지 않고 만든다. `TargetList`가 상수 1, `FromClause`가 문장의 `RangeVar`, `WhereClause`가 문장의 `WhereClause`인 `SelectStmt`를 조립해 기존 `deparse`와 같은 방식으로 `pg.Deparse`에 넘긴다. 위 테스트의 기대 문자열은 이 방식의 실제 출력이다.

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/sqlparse/`
Expected: PASS.

- [ ] **Step 5: 게이트가 `Measured`를 지금의 unsupported처럼 다루게 한다**

분류만 바뀌고 기록은 그대로여야 한다. `internal/gate/gate.go`의 `judge`에서 첫 분기를 넓힌다. Task 6이 이 줄을 다시 고친다.

```go
case p.res.Kind == sqlparse.Unsupported, p.res.Kind == sqlparse.Measured:
	in.Unsupported = p.res.Reason
```

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/ ./internal/proxy/`
Expected: PASS. 기존 테스트를 고치지 않는다. `TestShadowUnsupportedIsRecordedWithoutModel`이 다중 행 `INSERT`를 여전히 `unsupported`/`multi_row`로 본다.

- [ ] **Step 6: 커밋**

```bash
git add internal/sqlparse internal/gate/gate.go
git commit -m "분류기: 다중 행 쓰기를 measured로 분류하고 측정 방법을 정한다" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

---

### Task 3: 프록시가 실제 행 수와 트랜잭션 상태를 넘긴다

**Files:**
- Modify: `internal/proxy/session.go` (`SessionInfo`, `Outcome`, `unit`, `begin`, `fromServer`, `finish`)
- Test: `internal/proxy/gate_test.go`

**Interfaces:**
- Produces:
  ```go
  // in SessionInfo:
  // TxOpen means an explicit transaction was already open when the statement
  // arrived (the last ReadyForQuery status was not idle).
  TxOpen bool

  // in Outcome:
  // Rows is the row count from the statement's CommandComplete tag; nil when
  // it did not complete or the tag carries no count.
  Rows *int64
  ```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
func TestGateSeesRowCounts(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	simple := func(sql string) {
		t.Helper()
		if _, err := conn.PgConn().Exec(ctx, sql).ReadAll(); err != nil {
			t.Fatal(err)
		}
	}
	simple("insert into " + table + " values (1, 'a'), (2, 'a'), (3, 'b')")              // simple query: 3
	if _, err := conn.Exec(ctx, "update "+table+" set v = $1 where v = $2", "c", "a"); err != nil { // extended: 2
		t.Fatal(err)
	}
	simple("delete from " + table + " where v = 'nobody'")                                // 0, not nil
	b := &pgx.Batch{}                                                                     // two Executes, one Sync
	b.Queue("update "+table+" set v = 'd' where v = $1", "c")                            // 2
	b.Queue("delete from "+table+" where id = $1", 3)                                    // 1
	if err := conn.SendBatch(ctx, b).Close(); err != nil {
		t.Fatal(err)
	}
	conn.Exec(ctx, "insert into "+table+" values ($1, 'dup')", 1)                         // fails: nil

	_, os := g.outcomes(t, 6)
	want := []*int64{ptr(int64(3)), ptr(int64(2)), ptr(int64(0)), ptr(int64(2)), ptr(int64(1)), nil}
	for i, o := range os {
		if (o.Rows == nil) != (want[i] == nil) || (o.Rows != nil && *o.Rows != *want[i]) {
			t.Errorf("write %d: rows %v, want %v", i, deref(o.Rows), deref(want[i]))
		}
	}
}

func TestGateSeesOpenTransaction(t *testing.T) {
	table := watchedTable(t)
	g := newRecordingGate(watches(table))
	addr, _ := startProxy(t, upstream(t), g)
	conn := connect(t, addr, "stet_scram", nil)
	ctx := testCtx(t)

	mustExec(t, conn, "insert into "+table+" values (1, 'autocommit')")
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "insert into "+table+" values (2, 'in tx')"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	mustExec(t, conn, "insert into "+table+" values (3, 'autocommit again')")

	bs, _ := g.outcomes(t, 3)
	for i, want := range []bool{false, true, false} {
		if bs[i].info.TxOpen != want {
			t.Errorf("write %d: TxOpen = %v, want %v", i, bs[i].info.TxOpen, want)
		}
	}
}
```

`ptr`과 `deref`(nil이면 `"nil"`, 아니면 값을 돌려주는 출력용 헬퍼)가 이 패키지의 테스트에 없으면 `gate_test.go`에 더한다.

- [ ] **Step 2: 실패를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test ./internal/proxy/ -run 'RowCounts|OpenTransaction'`
Expected: FAIL (필드 미정의).

- [ ] **Step 3: 구현한다**

- `begin`: 게이트를 부르기 전에 `s.mu`를 잡고 `s.status != 'I'`를 읽어 `info.TxOpen`에 넣은 복사본을 넘긴다. `s.info` 자체는 바꾸지 않는다.
- `fromServer`의 `'C'`: 그 단위에 감시 중인 쓰기가 하나라도 있을 때만 태그를 해석해 완료 순번별로 기억한다. 감시 중인 쓰기가 없는 단위는 지금처럼 순번만 센다.
- `finish`: 실행된 쓰기에 그 순번의 행 수를 넣는다. Query 단위(`idx 0`)는 완료가 정확히 하나일 때 그 값을 쓴다.
- 태그 해석: 본문은 NUL로 끝나는 문자열이고, 마지막 공백 뒤가 정수면 그 값이다(`INSERT 0 5` → 5, `UPDATE 0` → 0, `BEGIN` → 없음).

```go
// tagRows reads the row count from a CommandComplete body.
func tagRows(body []byte) (int64, bool)
```

- [ ] **Step 4: 통과를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/proxy/`
Expected: PASS. `TestPassthroughLatencyBudget`를 포함한다.

- [ ] **Step 5: 커밋**

```bash
git add internal/proxy
git commit -m "프록시: 실제 행 수와 트랜잭션 상태를 게이트에 넘긴다" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

---

### Task 4: 영향 verdict

**Files:**
- Create: `internal/decide/impact.go`
- Modify: `internal/decide/decide.go` (`Input`, `Verdict`)
- Test: `internal/decide/decide_test.go`

**Interfaces:**
- Consumes: `config.Impact` (Task 1)
- Produces:
  ```go
  // Impact is what the database said about a multi-row write. It is stored
  // as stet.decisions.impact.
  type Impact struct {
      Method     string    `json:"method,omitempty"` // count | estimate | syntax; empty when measuring failed
      Rows       *int64    `json:"rows,omitempty"`   // the number the verdict used
      Capped     bool      `json:"capped,omitempty"`
      Cap        int       `json:"cap,omitempty"`
      Estimate   *int64    `json:"estimate,omitempty"`
      TableRows  *int64    `json:"table_rows,omitempty"`
      Share      *float64  `json:"share,omitempty"`
      Cascades   []Cascade `json:"cascades,omitempty"`
      ActualRows *int64    `json:"actual_rows,omitempty"`
      MeasureMS  int       `json:"measure_ms"`
      Error      string    `json:"error,omitempty"`
  }
  type Cascade struct {
      Table    string `json:"table"`
      OnDelete string `json:"on_delete"` // cascade | set_null | set_default
  }

  // added to Input:
  Op          string         // insert | update | delete; the share limit skips inserts
  ShapeReason string         // the reason a measured write had before; used when measuring failed
  Impact      *Impact        // set for a measured write on a table with impact limits
  ImpactLimit *config.Impact
  ```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
func TestVerdictImpact(t *testing.T) {
	n := func(v int64) *int64 { return &v }
	f := func(v float64) *float64 { return &v }
	lim := &config.Impact{MaxRows: 100, MaxShare: 0.5}
	cases := []struct {
		name    string
		in      Input
		verdict string
		reasons []string
	}{
		{"counted, within limits", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(99), TableRows: n(1000), Share: f(0.099)}}, Commit, nil},
		{"zero rows", Input{Op: "delete", ImpactLimit: lim, Impact: &Impact{Method: "count", Rows: n(0)}}, Commit, nil},
		{"capped", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true, Cap: 101}}, Hold, []string{"impact:rows"}},
		{"estimate over the row limit", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "estimate", Rows: n(150)}}, Hold, []string{"impact:rows"}},
		{"share over the limit", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(40), TableRows: n(50), Share: f(0.8)}}, Hold, []string{"impact:share"}},
		{"rows and share both over: rows wins", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(101), Capped: true, Share: f(0.9)}}, Hold, []string{"impact:rows"}},
		{"share does not apply to inserts", Input{Op: "insert", ImpactLimit: lim,
			Impact: &Impact{Method: "syntax", Rows: n(40), Share: f(0.8)}}, Commit, nil},
		{"table never analyzed: no share", Input{Op: "delete", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(40)}}, Commit, nil},
		{"no share limit configured", Input{Op: "delete", ImpactLimit: &config.Impact{MaxRows: 100},
			Impact: &Impact{Method: "count", Rows: n(40), Share: f(0.8)}}, Commit, nil},
		{"estimate only, within limits", Input{Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "estimate", Rows: n(50)}}, Escalate, []string{"impact:uncertain"}},
		{"measuring failed", Input{Op: "update", ShapeReason: "not_single_row", ImpactLimit: lim,
			Impact: &Impact{Error: "set role: permission denied"}}, Unsupported, []string{"not_single_row"}},
		{"unsupported wins over impact", Input{Unsupported: "in_transaction", Op: "update", ImpactLimit: lim,
			Impact: &Impact{Method: "count", Rows: n(500), Capped: true}}, Unsupported, []string{"in_transaction"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v, reasons := Verdict(tc.in)
			if v != tc.verdict || !reflect.DeepEqual(reasons, tc.reasons) {
				t.Errorf("got %s %v, want %s %v", v, reasons, tc.verdict, tc.reasons)
			}
		})
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `go test ./internal/decide/ -run TestVerdictImpact`
Expected: FAIL (`Impact` 미정의).

- [ ] **Step 3: 구현한다**

`Verdict`에서 `in.Unsupported` 검사 바로 다음에 `in.Impact != nil`이면 `impact.go`의 함수 결과를 돌려준다. 순서는 스펙 5장의 표 3~7번이다: 측정 실패(`Method == ""`) → `Unsupported`와 `ShapeReason`, 행 수 초과 또는 `Capped` → `impact:rows`, `Op`가 `update`·`delete`이고 `MaxShare > 0`이고 `Share`가 있고 그 값이 `MaxShare`보다 크면 → `impact:share`, 방법이 `estimate` → `Escalate`와 `impact:uncertain`, 나머지 → `Commit`과 nil.

- [ ] **Step 4: 통과를 확인한다**

Run: `go test ./internal/decide/`
Expected: PASS. 기존 `TestVerdict`를 포함한다.

- [ ] **Step 5: 커밋**

```bash
git add internal/decide
git commit -m "판정: 영향 한도에 따른 결정론적 verdict" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

---

### Task 5: `impact` 컬럼

**Files:**
- Modify: `internal/store/schema.sql`, `internal/store/store.go` (`Decision`, `Insert`)
- Test: `internal/store/store_test.go`

**Interfaces:**
- Produces: `Decision.Impact any` — nil이면 NULL, 아니면 JSON으로 저장된다. `internal/store`는 `decide`를 import하지 않는다.

- [ ] **Step 1: 실패하는 테스트를 쓴다**

```go
func TestInsertRecordsImpact(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	d.Impact = map[string]any{"method": "count", "rows": 99, "capped": false}
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var method string
	var rows int
	err := pool.QueryRow(context.Background(),
		`select impact->>'method', (impact->>'rows')::int from stet.decisions where id = $1`, d.ID).Scan(&method, &rows)
	if err != nil {
		t.Fatal(err)
	}
	if method != "count" || rows != 99 {
		t.Errorf("impact = %s %d", method, rows)
	}
}

func TestInsertWithoutImpactLeavesItNull(t *testing.T) {
	s, pool := testStore(t)
	d := sample(uniqueTable(t))
	if err := s.Insert(context.Background(), d); err != nil {
		t.Fatal(err)
	}
	var isNull bool
	if err := pool.QueryRow(context.Background(),
		`select impact is null from stet.decisions where id = $1`, d.ID).Scan(&isNull); err != nil {
		t.Fatal(err)
	}
	if !isNull {
		t.Error("impact is set for a write that was not measured")
	}
}
```

- [ ] **Step 2: 실패를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test ./internal/store/ -run Impact`
Expected: FAIL (`Impact` 필드 미정의).

- [ ] **Step 3: 구현한다**

`schema.sql`의 인덱스 정의 앞에 넣는다. 이 파일은 시작할 때마다 실행된다.

```sql
-- 다중 행 쓰기의 영향 측정 (docs/spec-v0.2-impact.md 6장). 측정하지 않은 쓰기는 NULL.
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS impact jsonb;
```

`Insert`는 기존 jsonb 컬럼과 같은 방식(`jsonArg`, `$n::jsonb`)으로 넣는다.

- [ ] **Step 4: 통과를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/store/`
Expected: PASS. `TestMigrateIsIdempotentAndConcurrent`와 `TestDecisionsAreAppendOnly`를 포함한다.

- [ ] **Step 5: 커밋**

```bash
git add internal/store
git commit -m "기록: stet.decisions에 impact 컬럼" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

---

### Task 6: 게이트의 측정과 기록

**Files:**
- Create: `internal/gate/impact.go`, `internal/gate/impact_test.go`
- Modify: `internal/gate/gate.go` (`pending`, `Begin`, `judge`)

**Interfaces:**
- Consumes: `sqlparse.Measured`, `sqlparse.Measure` (Task 2). `proxy.SessionInfo.TxOpen`, `proxy.Outcome.Rows` (Task 3). `decide.Impact`, `decide.Cascade`, `Input`의 새 필드 (Task 4). `config.Impact` (Task 1). `store.Decision.Impact` (Task 5).
- Produces:
  ```go
  // measure asks the database what a measured write would touch. It never
  // returns nil: a failure is reported in Impact.Error.
  func (g *Gate) measure(ctx context.Context, p *pending, lim *config.Impact) *decide.Impact

  var countTimeout = 200 * time.Millisecond // a var so tests can shorten it (Task 7)
  ```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

`impact_test.go`(패키지 `gate_test`)에 헬퍼를 둔다. 기존 `row` 구조체와 `decisions`는 고치지 않는다.

```go
// impactYAML is tableYAML plus impact limits: at most 3 rows, half the table.
const impactYAML = tableYAML + "    impact: {max_rows: 3, max_share: 0.5}\n"

// looseYAML has a row limit no test reaches. Tests whose number comes from
// the planner's estimate use it, so a rough estimate cannot flip the verdict.
const looseYAML = tableYAML + "    impact: {max_rows: 100}\n"

type impactRow struct {
	Verdict   string
	Reasons   []string
	Executed  bool
	ExecError *string
	Impact    map[string]any // nil when the column is NULL
}

// impacts waits for n decisions on the env's table, oldest first.
func (e *env) impacts(t *testing.T, n int) []impactRow

// seedMany adds ids 3..7: three more rows at company 3, two at company 9.
func (e *env) seedMany(t *testing.T) {
	t.Helper()
	e.seed(t) // ids 1 and 2, company 3
	mustExec(t, e.admin, `insert into `+e.table+` (id, name, company_id, status) values
		(3, 'Park', 3, 'active'), (4, 'Choi', 3, 'active'), (5, 'Jung', 3, 'active'),
		(6, 'Han', 9, 'active'), (7, 'Yoon', 9, 'active')`)
}

func num(m map[string]any, k string) (float64, bool) // JSON numbers decode as float64
```

테스트와 단언:

| 테스트 | 준비와 쓰기 | 단언 |
|---|---|---|
| `TestImpactCountsBeforeTheWrite` | `seedMany`. `update T set status = 'x' where company_id = 9` | `commit`, reasons 없음. `method=count`, `rows=2`, `capped` 없음, `cap=4`, `actual_rows=2`, `estimate` 있음. 행이 실제로 바뀌었다 |
| `TestImpactHoldsOverTheRowLimit` | `seedMany`. `update T set status = 'x' where company_id = 3` | `hold`, `[impact:rows]`. `rows=4`, `capped=true`, `actual_rows=5`. 행이 실제로 바뀌었다(섀도) |
| `TestImpactHoldsOverTheShareLimit` | `seed` 뒤 `analyze T`. `delete from T where company_id = 3` (2행 중 2행) | `hold`, `[impact:share]`. `table_rows=2`, `share=1` |
| `TestImpactSkipsShareForAnUnanalyzedTable` | `seed`만(분석 안 함). 같은 `delete` | `commit`. `table_rows`와 `share` 키가 없다 |
| `TestImpactBindsParameters` | `seedMany`. `conn.Exec(ctx, "update T set title = $1 where company_id = $2", "CTO", 9)` | `commit`, `rows=2`. `$1`이 세기 질의에서 안 쓰여도 동작한다 |
| `TestImpactCountsNullParameterAsNoRows` | `seedMany`. `conn.Exec(ctx, "update T set status = 'x' where company_id = $1", nil)` | `commit`, `rows=0`, `actual_rows=0` (두 키 모두 있다) |
| `TestImpactEstimateOnlyEscalates` | `looseYAML`로 `setup`. `seedMany`, `analyze T`. `update T set status = 'x' where company_id in (select 9)` | `escalate`, `[impact:uncertain]`. `method=estimate`, `cap` 키가 없다, `actual_rows=2` |
| `TestImpactReadsInsertedRowsFromTheStatement` | `insert into T (id, name) values (10, 'a'), (11, 'b')` 그리고 `values (20,'a'),(21,'b'),(22,'c'),(23,'d')` | 첫째 `commit`, `method=syntax`, `rows=2`, `actual_rows=2`. 둘째 `hold`, `[impact:rows]`, `rows=4` |
| `TestImpactRecordsCascades` | `T_child (id int primary key, parent int references T(id) on delete cascade)`를 만든다(정리 때 먼저 지운다). `seedMany`. `delete from T where company_id = 9` | `cascades`가 `[{table: T_child, on_delete: cascade}]` |
| `TestImpactIsNotMeasuredInsideATransaction` | `seedMany`. `tx.Exec("update T set status = 'x' where company_id = 9")` 뒤 커밋 | `unsupported`, `[in_transaction]`. `Impact == nil` |
| `TestWithoutImpactLimitsNothingChanges` | 기본 `tableYAML`로 `setup`. `seedMany`. 같은 범위 `update` | `unsupported`, `[not_single_row]`, `Impact == nil`, 모델 호출 0회 |

모든 테스트는 `setup(t, calm(), impactYAML)`로 시작하고(마지막 것만 예외), 모델이 한 번도 불리지 않았음을 `d.callCount() == 0`으로 확인한다.

- [ ] **Step 2: 실패를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/ -run 'Impact|WithoutImpactLimits'`
Expected: FAIL.

- [ ] **Step 3: `gate.go`를 연결한다**

- `pending`에 `oids []uint32`(클라이언트가 Parse에 준 타입)와 `impact *decide.Impact`를 더한다. `Begin`에서 `ps`의 OID를 담는다.
- `Begin`: 바인딩 값을 텍스트로 바꾼 뒤, `p.res.Kind == sqlparse.Measured`이고 `p.stateErr == nil`이고 `!s.TxOpen`이고 그 테이블의 `Impact`가 nil이 아니면 `p.impact = g.measure(ctx, p, lim)`. 그 밖의 `Measured`는 아무것도 하지 않는다.
- `judge`의 분기 순서:

```go
switch {
case p.res.Kind == sqlparse.Unsupported,
	p.res.Kind == sqlparse.Measured && t.Impact == nil:
	in.Unsupported = p.res.Reason // shape first, as before
case o.InTransaction:
	in.Unsupported = "in_transaction"
case p.res.Kind == sqlparse.Measured:
	if p.impact == nil { // skipped in Begin but the write turned out not to be in a transaction
		p.impact = &decide.Impact{Error: "not measured"}
	}
	p.impact.ActualRows = o.Rows
	in.Op, in.ShapeReason, in.Impact, in.ImpactLimit = p.res.Op, p.res.Reason, p.impact, t.Impact
	d.Impact = p.impact
case p.stateErr != nil:
	// unchanged from here
```

`End`의 `recent.add`는 그대로다. `Measured` 쓰기는 `p.pk`가 nil이라 들어가지 않는다.

- [ ] **Step 4: `impact.go`의 `measure`를 구현한다**

`MethodSyntax`는 DB에 묻지 않는다: `Method: "syntax"`, `Rows`는 `Measure.Rows`.

그 밖에는 풀에서 연결 하나를 얻어 `PgConn`으로 다음 순서를 따른다. 세기를 맨 뒤에 두는 이유: 시간 제한에 걸리면 트랜잭션이 중단되어 그 뒤에는 아무것도 실행할 수 없다.

1. 바인딩 값이 있으면 원래 문장을 준비해 서버가 추론한 매개변수 타입을 얻는다(`pg.Prepare(ctx, "", p.sql, p.oids)`의 `ParamOIDs`). 이후 질의에 값은 `p.params`의 텍스트로, 타입은 이 OID로 준다. NULL 값은 nil 바이트로 넘긴다.
2. 한 번의 `Exec`로 `begin read only; set local role <pgx.Identifier{p.session.User}.Sanitize()>; set local statement_timeout = '<countTimeout의 ms>ms'`. 실패하면 `Error: "set role: " + err`로 끝낸다.
3. 테이블 크기: `select reltuples::bigint from pg_class where oid = $1::regclass`에 설정의 테이블 키를 준다. 0보다 클 때만 `TableRows`에 넣는다.
4. `DELETE`일 때 외래 키 연쇄:

```sql
select c.conrelid::regclass::text, c.confdeltype
from pg_constraint c
where c.contype = 'f' and c.confrelid = $1::regclass and c.confdeltype in ('c', 'n', 'd')
order by 1
```

   `c`는 `cascade`, `n`은 `set_null`, `d`는 `set_default`로 적는다.
5. 추정: `EXPLAIN (FORMAT JSON) ` 뒤에 원래 문장을 붙여 실행한다. 최상위 노드가 `ModifyTable`이 아니면 실패다. 그 자식 중 `Parent Relationship`이 `InitPlan`·`SubPlan`이 아닌 것의 `Plan Rows`를 더해 반올림한 값이 `Estimate`다. 추정이 실패하면 `Error`에 원인을 넣고 끝낸다.
6. 방법이 `count`면: `select count(*) from (<CountSQL> limit <cap>) s`, `cap = lim.MaxRows + 1`. 결과가 `cap` 이상이면 `Capped: true`이고 `Rows`는 `cap`이다. SQLSTATE `57014`(시간 제한)로 실패하면 방법을 `estimate`로 바꾸고 `Rows`는 추정값이다. 다른 에러는 `Error`에 넣는다.
7. 방법이 `estimate`면 `Rows`는 추정값이다.
8. 어떤 경로로 끝나든 새 context(1초)로 `rollback`을 보낸다. `ctx`는 이미 만료됐을 수 있다.
9. `Op`가 `insert`가 아니고 `TableRows`가 있으면 `Share = Rows ÷ TableRows`. `Cap`은 세기를 시도했을 때만 넣는다. `MeasureMS`는 전체 시간이다.
10. `ctx`가 만료되어 끝났으면 `Error: "timeout"`.

- [ ] **Step 5: 통과를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race ./internal/gate/`
Expected: PASS. Task 2의 Step 5에서 실패하던 `TestShadowUnsupportedIsRecordedWithoutModel`을 포함해 기존 테스트가 고치지 않고 통과한다.

- [ ] **Step 6: 커밋**

```bash
git add internal/gate
git commit -m "게이트: 다중 행 쓰기의 영향을 실행 전에 DB에 물어 기록한다 (섀도)" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```

---

### Task 7: 권한, 실패 경로, 원래 동작 불변

**Files:**
- Create: `internal/gate/export_test.go`
- Modify: `internal/gate/impact.go` (역할별 경고 한 번), `internal/gate/impact_test.go`
- Modify: `docs/spec-v0.2-impact.md` (상태 줄, 11장 체크)

**Interfaces:**
- Consumes: `measure`, `countTimeout` (Task 6)
- Produces (테스트 전용):
  ```go
  // in export_test.go, package gate
  func SetCountTimeout(d time.Duration) (restore func())
  ```

- [ ] **Step 1: 실패하는 테스트를 쓴다**

헬퍼:

```go
// connectAs opens a client connection through the proxy as another role.
func (e *env) connectAs(t *testing.T, user, password, agent string) *pgx.Conn

// superuser connects straight to the test database as postgres
// (password postgres-pw, set by scripts/testdb.sh).
func superuser(t *testing.T) *pgx.Conn
```

| 테스트 | 준비와 쓰기 | 단언 |
|---|---|---|
| `TestImpactCountsWhatTheClientsRoleCanSee` | `superuser`로 `grant stet_md5 to stet_scram`. `seedMany`. `grant all on T to stet_md5; alter table T enable row level security; create policy p on T for all to stet_md5 using (company_id = 9)`. `stet_md5`(비밀번호 `md5-pw`)로 접속해 `update T set status = 'x' where status = 'active'` | `commit`, `rows=2`, `actual_rows=2`. 소유자 권한으로 셌다면 `capped`와 `hold`가 나온다 |
| `TestImpactFailsWhenTheRoleCannotBeAssumed` | `seedMany`. `grant all on T to stet_trust`. `stet_trust`(비밀번호 없음)로 접속해 `update T set status = 'x' where company_id = 9` | `unsupported`, `[not_single_row]`. `impact.error`에 `set role`이 들어 있다. `method` 키가 없다. 쓰기는 실행됐다(`executed`, 2행이 바뀜) |
| `TestImpactNeverCountsThroughAnUnlistedFunction` | `looseYAML`. `seedMany`, `analyze T`. `update T set status = 'x' where set_config('stet.marker', 'ran', false) is not null and company_id = 9` | `method=estimate`, `cap` 키가 없다(세기를 시도하지 않았다). `escalate` |
| `TestImpactFallsBackToTheEstimateWhenCountingIsSlow` | `looseYAML`. `defer gate.SetCountTimeout(time.Millisecond)()`. `insert into T (id, company_id) select g, 1 from generate_series(100, 200100) g`, `analyze T`. `update T set status = 'x' where company_id = 424242` | `method=estimate`, `cap=101`(세기를 시도했다), `error` 키가 없다. `escalate`, `[impact:uncertain]`. `actual_rows=0` |
| `TestImpactFailureKeepsTheOldReason` | `seedMany`. `update T set status = 'x' where current of nocursor` | 클라이언트가 받은 SQLSTATE가 `34000`. `unsupported`, `[not_single_row]`. `impact.error`가 비어 있지 않고 `method` 키가 없다. `executed=false` |
| `TestImpactIsRecordedWhenTheWriteFails` | `seed`. `insert into T (id, name) values (1, 'dup'), (50, 'new')` | 클라이언트가 받은 SQLSTATE가 `23505`. `commit`, `method=syntax`, `rows=2`, `executed=false`, `exec_error`에 `23505`, `actual_rows` 키가 없다 |
| `TestImpactMeasuresConcurrentWrites` | `seedMany`. 고루틴 8개가 각자 연결로 `update T set title = $1 where company_id = 9` | 기록 8건, 모두 `method=count`, `rows=2`. `-race`에서 경쟁이 없다 |
| `TestWritesBehaveTheSameWithImpactOn` | `T`와 구조·내용이 같은 `T_twin`을 만든다. 아래 문장을 `T`에는 프록시로, `T_twin`에는 직접 보낸다 | 문장마다 CommandTag와 SQLSTATE가 같다. 끝난 뒤 두 테이블의 `select * order by id` 결과가 같다 |

`TestWritesBehaveTheSameWithImpactOn`의 문장(`%s`가 테이블 이름):

```sql
update %s set status = 'x' where company_id = 3
delete from %s where company_id = 424242
insert into %s (id, name) values (1, 'dup'), (60, 'new')
update %s set title = 'y' where company_id in (select 9)
insert into %s (id, name) select id + 100, name from %s where company_id = 9
delete from %s
```

- [ ] **Step 2: 실패를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test ./internal/gate/ -run 'Impact|WritesBehaveTheSame'`
Expected: `SetCountTimeout`이 없어 컴파일 실패.

- [ ] **Step 3: 구현한다**

- `export_test.go`에 `SetCountTimeout`을 둔다.
- `measure`에서 `SET ROLE` 실패는 역할마다 한 번만 `Warn`으로 남긴다(`Gate`에 `sync.Map`). 메시지에 필요한 조치(`GRANT <role> TO <Stet 계정>`)를 적는다.
- Task 6의 구현이 위 테스트를 통과하지 못하는 곳을 고친다.

- [ ] **Step 4: 전체를 확인한다**

Run: `STET_TEST_PG=127.0.0.1:55432 go test -race ./...`
Expected: 모든 패키지 `ok`. `internal/proxy`의 `TestPassthroughLatencyBudget`와 `TestWatchedPassthrough`를 포함한다.

Run: `go build -o bin/stet ./cmd/stet && go vet ./...`
Expected: 출력 없음.

- [ ] **Step 5: 스펙의 상태를 고친다**

`docs/spec-v0.2-impact.md`의 상태 줄을 `상태: 구현됨 (섀도)`로 바꾸고, 11장의 항목 가운데 테스트로 확인된 것을 `[x]`로 바꾼다. 확인하지 못한 항목이 있으면 그대로 두고 이유를 한 줄 적는다.

- [ ] **Step 6: 커밋**

```bash
git add internal/gate docs/spec-v0.2-impact.md
git commit -m "영향 측정: 클라이언트 역할로 측정, 실패 경로, 원래 동작 불변 확인" -m "Co-Authored-By: Claude Fable 5.2 <noreply@anthropic.com>"
```
