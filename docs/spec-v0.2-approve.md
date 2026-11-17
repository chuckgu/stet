# Stet v0.2 2b단계 스펙: 보류 건의 승인과 거부

상태: 구현됨 (2026-11-17). 근거는 D31의 7번, spec-v0.1 7장, spec-v0.2-enforce 9·13·17장이다.

## 1. 목표와 비목표

enforce는 `hold`와 `escalate`로 판정된 쓰기를 막고 `stet.held_writes`에 저장한다. enforce 단계만으로는 그 뒤가 없어, 에이전트에게 보류는 거부와 같았다. 이 단계는 사람이 보류 건을 근거와 함께 보고 승인하거나 거부하는 가장 짧은 길을 만든다.

**목표**

- 검토자가 SQL로 보류 건과 그 근거를 본다(`stet.review`).
- 검토자가 `stet.approve(id)`나 `stet.reject(id)`를 부른다.
- 승인된 쓰기는 Stet이 실행한다. 에이전트가 보낸 문장과 값 그대로, 그 에이전트 계정의 권한으로, 대상 행이 그동안 바뀌지 않았는지 확인한 뒤에 실행한다.
- 승인은 사람이 본 것까지만 유효하다. 행이 바뀌었거나 승인한 것보다 많은 행이 바뀌면 실행하지 않는다.
- enforce 단계가 넘긴 두 가지를 닫는다. 에이전트가 모르는 보류 건이 승인 대상으로 남는 것과, 실행된 쓰기의 보류 줄이 승인되어 두 번 실행되는 것이다.

**비목표**

- 검토 화면, 알림, 담당자 지정, 다른 사람에게 넘기기(D31: 넓이 단계).
- 에이전트가 자기 보류 건의 결과를 아는 길. 에이전트는 enforce 단계에서처럼 `ST001`만 받는다(11장).
- 승인할 때의 재판단. 사람이 정했으므로 모델을 다시 부르지 않는다.
- 판정 `reject`와 `ST003`. enforce 단계에서처럼 쓰지 않는다.
- 승인을 공격으로 쓰는 에이전트를 막는 것(D30: 적어만 둔다. 11장).

**실패로 볼 것.** 승인하지 않은 쓰기가 실행되는 것. 승인한 쓰기가 두 번 실행되는 것. 검토자가 본 것과 다른 쓰기가 실행되는 것. 섀도 모드에서 Stet이 쓰기를 실행하는 것(규칙 6).

## 2. 접근

**승인된 쓰기를 누가 실행하는가.** Stet이 실행한다(2026-11-17에 정함). 에이전트가 다시 보내게 하는 방식은 Stet이 단순해지지만, 승인된 쓰기가 실행될지가 에이전트에게 달린다.

**Stet의 어디가 실행하는가.** 세 가지를 봤다.

| | 동작 | 버린 이유 또는 고른 이유 |
|---|---|---|
| 함수 안에서 | `stet.approve`가 검토자의 세션에서 문장을 바로 실행한다 | 결과를 그 자리에서 본다. 그러나 에이전트 계정의 권한으로 실행하려면 검토자가 그 계정의 구성원이어야 한다(`SECURITY DEFINER` 함수 안에서는 `SET ROLE`을 쓸 수 없다). 검토자에게 에이전트의 권한 전부와 `stet.held_writes`의 쓰기 권한을 줘야 한다. 바인딩 값을 SQL 안에서 다시 묶는 것도 보낸 그대로라고 보장하기 어렵다 |
| CLI로 | `stet approve <id>`가 설정 파일의 계정으로 붙어 실행한다 | 누가 승인했는지가 DB가 인증한 계정이 아니라 설정 파일을 가진 사람이 된다. D31은 함수를 말한다. 나중의 검토 화면이 부를 것도 함수다 |
| **Stet 프로세스가** | 함수는 줄의 상태만 `approved`로 바꾼다. 실행 중인 Stet이 그 줄을 집어 실행하고 결과를 줄에 적는다 | 고른 것. 검토자에게 필요한 권한은 뷰 하나와 함수 둘뿐이다. 바인딩 값은 저장된 텍스트를 프로토콜로 다시 보낸다(텍스트로 온 값은 온 바이트 그대로, 바이너리로 온 값은 보류될 때 서버가 푼 글자다. 한 Bind에 둘이 섞여 와도 같다). Stet의 계정은 이미 에이전트 계정의 구성원이어야 한다(영향 측정). 대가는 결과가 조금 뒤에 줄에 적히는 것이다. 검토자는 뷰를 다시 읽어 확인한다 |

**테스트 서버(PostgreSQL 14)에서 확인한 것.** 이 설계가 기대는 동작이다.

- `SECURITY DEFINER` 함수 안의 `SET LOCAL ROLE`은 거부된다(`cannot set parameter "role" within security-definer function`).
- 새로 만든 함수는 PUBLIC이 실행할 수 있다. 거두지 않으면 누구나 부른다.
- `SET LOCAL ROLE` 아래의 문장이 `set_config('role', ...)`로 역할을 바꾸면, 그 뒤의 `current_user`가 바뀐 역할로 보인다.
- 저장점 뒤의 `SET LOCAL ROLE`은 그 저장점까지 되돌리면 풀린다.
- 타입 없이 온 텍스트 값 `'01/02/2026 10:00'`은 `DateStyle`과 `TimeZone`에 따라 2월 1일 01:00 UTC로도, 1월 2일 10:00 UTC로도 들어간다.
- `REPEATABLE READ` 트랜잭션이 행을 읽은 뒤 다른 연결이 그 행을 바꾸면, 그 트랜잭션의 `UPDATE`는 `40001`로 실패한다.
- `REPEATABLE READ` 트랜잭션의 `SELECT ... FOR UPDATE SKIP LOCKED`는, 스냅숏을 잡은 뒤 다른 연결이 그 행을 바꾸고 커밋했으면 빈 결과가 아니라 `40001`(`could not serialize access due to concurrent update`)로 실패한다.
- 서버는 `TimeZone`, `DateStyle`, `IntervalStyle`을 세션이 시작할 때와 바뀔 때 ParameterStatus로 알린다. 바뀐 것은 그 단위의 ReadyForQuery 직전에야 알린다.
- 서버는 문장의 타입 없는 리터럴을 Parse할 때, 텍스트 바인딩 값을 Bind할 때 그때의 설정으로 읽는다. Execute할 때가 아니다. `Asia/Seoul`에서 준비한 문장의 `'2026-01-01 10:00'::timestamptz`는 `SET timezone = 'UTC'`에 답이 온 뒤에 실행해도 01:00 UTC다. `Asia/Seoul`에서 Bind한 값도 같다.
- `postgres` 형식으로 나온 `interval` 글자(`-1 days +02:03:04`)는 `IntervalStyle`이 `sql_standard`인 세션에서도 같은 값으로 읽힌다.
- `SET LOCAL ROLE`은 그 역할의 구성원이 아니면 `42501`(`permission denied to set role`)로, 역할이 없으면 `22023`(`role ... does not exist`)으로 실패한다.
- `statement_timeout`과 `lock_timeout`이 같은 값이면 잠금을 기다리던 문장은 `55P03`이 아니라 `57014`(`canceling statement due to statement timeout`)로 끝난다. 문장의 타이머가 먼저 시작하기 때문이다.

## 3. 흐름

1. 에이전트의 쓰기가 `hold`나 `escalate`로 판정된다. Stet은 줄을 `unconfirmed`로 넣고 `ST001`을 돌려준다. (4장, enforce 스펙 9장)
2. 그 에러가 에이전트에게 전달된 것을 확인하면 줄을 `pending`으로 바꾼다. 전달되지 않았으면 `undelivered`로 바꾼다. (4장)
3. 검토자가 `stet.review`에서 줄과 근거를 본다. (5장)
4. 검토자가 `stet.reject(id)`를 부르면 줄은 `rejected`가 된다. 끝이다.
5. 검토자가 `stet.approve(id)`를 부르면 줄은 `approved`가 된다. (6장)
6. Stet이 `approved`인 줄을 집어 확인하고 실행한다. 실행했으면 `applied`, 하지 않았으면 `failed`와 이유가 남는다. (7장)
7. `pending`인 채로 `expires_at`이 지난 줄은 승인할 수 없다. (8장)

```
unconfirmed ─┬─> pending ─┬─> approved ─┬─> applied
             │            │             └─> failed
             │            ├─> rejected
             │            └─ (expires_at이 지나면 승인 불가. 뷰는 expired로 보여 준다)
             └─> undelivered
```

`applied`, `failed`, `rejected`, `undelivered`는 끝 상태다. 끝 상태의 줄은 다시 승인할 수 없다. 실패한 쓰기를 실행하려면 에이전트가 다시 보내 새로 판단받는다.

## 4. 저장: `stet.held_writes`의 변경

```sql
ALTER TABLE stet.held_writes ALTER COLUMN status SET DEFAULT 'unconfirmed';
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS expires_at    timestamptz;  -- 이때까지 승인할 수 있다
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS settings      jsonb;        -- 보류될 때 그 세션의 TimeZone, DateStyle, IntervalStyle
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS row_absent    boolean;      -- 상태를 읽었을 때 대상 행이 없었다
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS in_transaction boolean;     -- 명시적 트랜잭션이거나 한 Sync 아래 다른 문장과 함께 온 쓰기였다. pending이 될 때 적는다
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS planned_rows  bigint;       -- 보류될 때 잰 행 수
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS rows_exact    boolean;      -- planned_rows가 정확한 값인가
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS resolved_by   text;         -- 승인·거부한 접속 계정(session_user)
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS resolved_at   timestamptz;
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS note          text;         -- 검토자가 적은 말
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS max_rows      bigint;       -- 승인이 허락한 행 수의 상한
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS applied_at    timestamptz;  -- 실행을 끝냈거나 실패로 적은 시각
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS rows_affected bigint;
ALTER TABLE stet.held_writes ADD COLUMN IF NOT EXISTS error         text;         -- failed의 이유
CREATE INDEX IF NOT EXISTS held_writes_approved ON stet.held_writes (resolved_at) WHERE status = 'approved';
```

**전달을 확인한 뒤에만 `pending`이다.** 줄은 `unconfirmed`로 들어가고, 게이트가 그 쓰기의 결과를 받을 때 바꾼다. 바꾸는 것은 `unconfirmed`인 줄뿐이다.

| 게이트가 본 것 | 줄의 상태 |
|---|---|
| 프록시가 거부를 전달했다고 보고했고, 삽입이 성공했다 | `pending` |
| 거부가 전달되지 않았다(`Undelivered`) | `undelivered` |
| 삽입이 실패로 보고됐다(늦게 들어갈 수 있다) | `undelivered` |
| 프록시가 막은 쓰기를 실행됐거나 결과를 모른다고 보고했다(프록시의 결함) | `undelivered`. 에러 로그(`refused write reported as sent`, enforce 스펙 11장)도 남는다 |
| 게이트가 결과를 받지 못했다(Stet이 죽었다) | `unconfirmed`인 채로 남는다 |

- 늦게 들어온 삽입은 `unconfirmed`로 남고, 아무도 승인할 수 없다. enforce 스펙 13장의 "보류 건 삽입의 틈"이 닫힌다.
- 실행됐다고 보고된 쓰기의 줄은 `pending`이 되지 않는다. 승인으로 두 번 실행되는 길이 닫힌다.
- `pending`으로 바꾸는 것은 판단 기록을 넣은 뒤다. 넣기가 실패해도 그 뒤에 바꾼다. 승인할 수 있는 줄에는 대개 근거가 이미 붙어 있다.
- `pending`으로 바꿀 때 `in_transaction`을 함께 적는다. 프록시가 그 쓰기의 결과를 보고할 때 알려 주는 값 그대로다. 명시적 트랜잭션 안에서 왔거나, 한 Sync 아래 다른 Execute와 함께 왔거나, Sync 없이 Query로 끝난 단위(enforce 스펙 4장)에 들었으면 참이다. 검토자에게 뜻은 같다(5장).
- `pending`으로 바꾸지 못하면(DB 오류) 줄은 `unconfirmed`로 남고 에러 로그(`held write not marked pending`)를 남긴다. 에이전트는 id를 들었지만 그 줄은 승인할 수 없다. 막는 쪽으로 틀린다. 에이전트가 다시 보내면 새 줄이 생긴다.
- 이 단계 전에 들어간 줄은 `expires_at`이 없다. 뷰에는 `expired`로 보이고, 승인할 수 없고, 거부할 수는 있다.

**더 저장하는 것**

- `expires_at`은 문장이 들어온 시각에 `held.expire_after`를 더한 값이다(9장).
- `settings`는 그 세션에서 서버가 마지막으로 알린 `TimeZone`, `DateStyle`, `IntervalStyle`이다. 키는 서버가 알리는 이름 그대로다. 텍스트로 온 값(`'2026-11-17 10:00'`, `'01/02/2026'`)은 이 설정에 따라 다르게 읽힌다. Stet의 연결에서 그대로 실행하면 사람이 승인한 것과 다른 값이 들어갈 수 있다. 프록시는 enforce 세션에서 서버의 ParameterStatus로 세 설정을 따라가 게이트에 넘긴다. 섀도 세션은 따라가지 않는다.
- 서버는 문장의 타입 없는 리터럴을 Parse할 때, 텍스트 바인딩 값을 Bind할 때 그때의 설정으로 읽는다(2장). 설정이 바뀐 것은 그 단위의 ReadyForQuery 직전에야 알린다. 그래서 프록시가 아는 값이 쓰기의 값이 읽힌 때의 값이 아닐 수 있다. 프록시는 다음 가운데 하나면 그것을 게이트에 알린다.
  - 아직 ReadyForQuery가 답하지 않은 단위에 `SET`이나 `RESET`이 있다. 쓰기 자신의 메시지와 쓰기와 같은 Sync 아래의 메시지도 든다. Sync 없이 Query로 끝나 ReadyForQuery 없이 답을 받은 단위에 든 것은 다음 ReadyForQuery까지 센다. 어느 설정의 `SET`이든 센다. 단위 안의 Execute가 무엇을 실행할지 모르는 포털을 실행하거나, 그 포털의 이름이 가질 수 있는 뜻(enforce 스펙 6장) 가운데 `SET`이나 `RESET`이 있으면 그 단위에 `SET`이 있는 것으로 본다.
  - 쓰기를 실행하는 포털의 문장이 Parse된 뒤나 그 포털이 Bind된 뒤에, 서버가 세 설정 가운데 하나가 바뀌었다고 알렸다. 그 이름이 가질 수 있는 뜻이 여럿이면 그 모두의 Parse를 본다.
  - 그 Parse나 Bind가 왔을 때 첫째 경우였다.
- 그런 쓰기와, 셋 가운데 서버가 알린 적 없는 것이 있는 쓰기는 "보낸 그대로 저장할 수 없는" 것이다. 줄을 넣지 않고 `held_write` 없이 막는다(enforce 스펙 7장). 에러 로그 `held write not stored`가 남는다.
- `SET`은 깨끗한 문장이어서(enforce 스펙 4장) 답을 기다리지 않고 그 뒤에 이어 보낸 쓰기도 판단받는다. 보류되면 위의 규칙으로 저장되지 않는다. `RESET`은 깨끗하지 않아서, 답을 받기 전에 이어 보낸 쓰기는 보류되기 전에 `in_transaction`으로 막힌다.
- 바인딩 값은 텍스트 형식으로 온 것은 온 바이트 그대로, 바이너리 형식으로 온 것은 Stet의 연결에서 서버가 텍스트로 푼 글자로 저장한다. NULL은 NULL이다. 한 Bind에 둘이 섞여 와도 같다(JDBC는 정수를 바이너리로, 날짜를 타입 없는 텍스트로 한 Bind에 보낸다고 알려져 있다). 텍스트 값을 Stet의 연결에서 다시 읽어 저장하면 `'01/02/2026'`이 Stet의 `DateStyle`로 읽힌 글자가 된다. 판단 기록의 `params`도 같은 값이다(enforce 스펙 9장).
- `row_absent`는 단일 행 `UPDATE`·`DELETE`의 상태를 읽었는데 대상 행이 없었다는 표시다. 그때는 `row_version`이 없다. 상태를 읽지 못했으면 둘 다 없다. 검토자가 본 것은 "그런 행이 없다"이다(7장).
- `planned_rows`와 `rows_exact`는 승인이 허락하는 크기의 기준이다(6장).

| 쓰기 | `planned_rows` | `rows_exact` |
|---|---|---|
| 단일 행 `INSERT`·`UPDATE`·`DELETE` | 1 | true |
| 잰 다중 행 쓰기, 방법이 `syntax`이거나 상한에 닿지 않은 `count` | 잰 값 | true |
| 잰 다중 행 쓰기, 상한에 닿은 `count`나 `estimate` | 잰 값 | false |
| 재지 못한 쓰기 | NULL | false |

- Stet의 풀 연결은 `DateStyle`을 `ISO`로, `IntervalStyle`을 `postgres`로 고정한다. 바이너리 값을 텍스트로 풀 때 어느 설정에서나 같은 뜻으로 읽히는 글자가 나오게 하려는 것이다.

**줄의 내용은 바뀌지 않는다.** `BEFORE UPDATE` 트리거(`stet.held_writes_guard`)가 셋을 강제한다. 어기면 `ST004`다. 누가 고치든 같고, Stet의 계정도 마찬가지다.

- 바뀔 수 있는 컬럼은 아홉이다. `status`, `in_transaction`, `resolved_by`, `resolved_at`, `note`, `max_rows`, `applied_at`, `rows_affected`, `error`. 나머지는 한 번 들어가면 바뀌지 않는다. 문장, 값, 값의 타입(`param_types`), 계정, 데이터베이스, 테이블, 대상 키, 행 버전, `row_absent`, 판정, 이유, `expires_at`, `settings`, `planned_rows`, `rows_exact`, `id`, `decision_id`, `created_at`이다. 나중에 더해지는 컬럼도 기본이 고정이다.
- `status`는 3장의 그림에 있는 화살표로만 바뀐다. 끝 상태에서는 바뀌지 않는다. `status`를 그대로 두는 갱신은 이 확인을 지난다.
- 아홉도 저마다 바뀌는 갱신이 정해져 있다. `in_transaction`은 `unconfirmed`에서 `pending`이 되는 갱신에서만 바뀐다. `resolved_by`, `resolved_at`, `note`, `max_rows`는 `pending`에서 `approved`나 `rejected`가 되는 갱신에서만, `applied_at`, `rows_affected`, `error`는 `approved`에서 `applied`나 `failed`가 되는 갱신에서만 바뀐다. 그래서 승인된 줄의 `max_rows`를 넓히거나 `resolved_at`을 늦춰 10분(8장)을 다시 세게 할 수 없다.

승인된 뒤에 문장이 바뀌면 사람이 보지 않은 쓰기가 실행된다. 함수와 Stet의 코드가 지키는 것과 별개로 DB가 직접 막는다(규칙 5와 같은 생각이다). `DELETE`는 막지 않는다. 보존 기간은 이 단계에서 다루지 않는다. 줄을 SQL로 직접 넣는 것도 막지 않는다. 그런 줄 때문에 실행부가 상한과 설정을 다시 확인한다(7장의 `no_row_limit`, `settings_missing`).

**스키마를 올리는 때.** `Store.Migrate`는 `schema.sql`이 데이터베이스가 마지막으로 실행한 것과 다를 때만 그 파일을 실행한다. advisory lock을 잡은 뒤 파일의 SHA-256을 `stet.schema_version`(한 줄짜리 표)의 값과 비교한다. 같으면 DDL을 하나도 돌리지 않고 끝난다. 다르거나 그 표가 없으면 파일을 실행하고 새 값을 적는다. 트랜잭션은 서버의 기본 격리 수준과 상관없이 `READ COMMITTED`다. 다른 Stet의 Migrate를 기다린 뒤에는 그쪽이 커밋한 값을 읽어야 하기 때문이다. 이유와 대가는 11장에 있다.

## 5. 검토자가 보는 것: `stet.review`

```sql
CREATE OR REPLACE VIEW stet.review AS
SELECT h.id,
       CASE WHEN h.status = 'pending' AND (h.expires_at IS NULL OR h.expires_at <= now())
            THEN 'expired' ELSE h.status END AS status,
       h.created_at, h.expires_at,
       h.login_role, d.agent_id, d.on_behalf_of,
       h.target_table, h.target_pk, h.statement, h.params, h.in_transaction,
       h.verdict, h.reasons, h.planned_rows, h.rows_exact,
       d.impact, d.cumulative, d.proposed, d.state, d.answers,
       h.resolved_by, h.resolved_at, h.note, h.max_rows,
       h.applied_at, h.rows_affected, h.error
FROM stet.held_writes h LEFT JOIN stet.decisions d ON d.id = h.decision_id;
```

- 근거는 판단 기록에서 온다. 모델에 보낸 상태(`state`), 모델의 답(`answers`), 영향 측정(`impact`), 누적 값(`cumulative`)이다. 판단 기록의 삽입이 실패했으면 그 칸들이 비어 있다. 그 줄도 승인할 수 있다. 문장과 값과 이유는 줄 자체에 있다.
- `in_transaction`이 참인 줄은 명시적 트랜잭션 안에서 왔거나, 한 Sync 아래 다른 문장과 함께 온 쓰기다. 에이전트가 `BEGIN` 뒤에 보낸 첫 쓰기가 대표적이다. 어느 쪽이든 거부로 그 트랜잭션은 실패했고, 에이전트가 그 트랜잭션에서 함께 하려던 다른 문장은 실행되지 않았다. 승인하면 이 쓰기 하나만 따로 실행되고 바로 커밋된다. 드라이버가 문장마다 `BEGIN`을 붙이는 클라이언트(psycopg, 자동 커밋을 끈 JDBC)의 쓰기는 모두 이렇게 보인다. 그래서 막지 않고 검토자에게 보여 준다.
- `expired`는 뷰를 읽는 트랜잭션이 시작한 시각(`now()`)으로 가린다. 승인할 수 있는지는 `stet.approve`가 그때의 시각(`clock_timestamp()`)으로 다시 본다(6장).
- 뷰는 소유자(Stet의 계정)의 권한으로 읽는다. 검토자에게 `stet.held_writes`와 `stet.decisions`의 권한을 줄 필요가 없다.
- `state`와 `params`에는 업무 데이터가 있다. 누구에게 뷰를 보여 줄지는 운영자가 정한다.

## 6. 승인과 거부

```sql
stet.approve(id uuid, note text DEFAULT NULL, max_rows bigint DEFAULT NULL) RETURNS text  -- 'approved'
stet.reject(id uuid, note text DEFAULT NULL) RETURNS text                                 -- 'rejected'
```

둘 다 `SECURITY DEFINER`이고 소유자는 Stet의 계정이다. `search_path`를 `pg_catalog, pg_temp`로 고정하고 이름을 스키마까지 적는다. 하는 일은 줄을 잠그고(`FOR UPDATE`) 아래의 확인을 차례로 한 뒤, 조건이 맞으면 그 줄의 `status`, `resolved_by`(`session_user`), `resolved_at`, `note`, `max_rows`를 바꾸는 것뿐이다. 문장을 실행하지 않는다.

시각은 트랜잭션이 시작한 때(`now()`)가 아니라 함수가 도는 때(`clock_timestamp()`)다. `resolved_at`도, `expires_at`과의 비교도 그렇다. 검토자가 오래 열어 둔 트랜잭션에서 승인해도 `resolved_at`은 실제로 승인한 시각이고, 그 사이 만료된 줄은 승인되지 않는다.

거절하는 경우. SQLSTATE는 `ST005`이고, 메시지가 이유를 말한다. 표의 순서대로 확인하고 처음 걸린 것으로 거절한다. 거절하면 줄은 그대로다.

| 경우 | 메시지 |
|---|---|
| 그 id의 줄이 없다 | `held write <id> does not exist` |
| `status`가 `unconfirmed`다 | `held write <id> is unconfirmed: it is not confirmed that the agent was told of it` |
| `status`가 그 밖에 `pending`이 아니다 | `held write <id> is <status>, not pending` |
| `expires_at`이 없거나 지났다 | `held write <id> has expired` |
| `session_user`가 그 줄의 `login_role`이다 | `<session_user> sent this write and cannot approve it` |
| 줄의 이유에 `concurrent_change`가 있다 | `held write <id> was held because its row kept changing while it was judged (concurrent_change): its stored state is stale and it would not run; the agent must send it again` |
| `max_rows`가 0보다 작다 | `max_rows must not be negative` |
| `max_rows`를 주지 않았는데 `rows_exact`가 참이 아니거나 `planned_rows`가 없다 | `held write <id> was not measured exactly (planned_rows: <잰 값이나 unknown>): pass max_rows, the most rows it may change` |

- `concurrent_change`가 있는 줄은 대상 행이 판단하는 동안 계속 바뀌어 보류된 쓰기다(enforce 스펙 5장). 저장된 상태가 이미 낡았으므로 승인해도 실행되지 않는다. 에이전트가 다시 보내야 한다.
- **승인은 크기까지다.** 실행할 때 바뀐 행 수가 상한을 넘으면 실행을 되돌린다(7장). 상한은 `max_rows`를 줬으면 그 값이고, 주지 않았으면 `planned_rows`다. "100행 넘게"나 "약 5,000행"으로만 잰 쓰기, 재지 못한 쓰기, `rows_exact`가 없는 줄(함수를 거치지 않고 넣은 줄)은 검토자가 숫자를 직접 적어야 승인된다. Stet이 그 숫자를 지어내지 않는다. 그래서 함수로 승인한 줄에는 언제나 상한이 있다.
- `reject`는 앞의 세 확인(줄이 없다, `unconfirmed`다, `pending`이 아니다)만 한다. 만료된 줄도, 자기 쓰기도 거부할 수 있다.
- 두 검토자가 같은 줄을 동시에 부르면 뒤의 쪽은 앞의 쪽의 잠금을 기다린다. 앞의 쪽이 커밋하면 뒤의 쪽은 지금 상태를 말하는 `ST005`(`... is approved, not pending`)를 받는다. 하나만 된다.
- 승인을 취소하는 함수는 없다. `approved`가 된 줄은 곧 실행된다.

**권한.** `schema.sql`을 실행할 때마다(4장) 두 함수의 `EXECUTE`를 PUBLIC에서 거둔다. 그 실행은 한 트랜잭션이므로 PUBLIC이 부를 수 있는 틈이 없다. `CREATE OR REPLACE`는 이미 준 권한을 지우지 않으므로 검토자의 권한은 남는다. 누가 승인할 수 있는지는 운영자가 DB 권한으로 정한다. Stet은 역할을 만들지 않는다.

```sql
GRANT USAGE ON SCHEMA stet TO reviewer;
GRANT SELECT ON stet.review TO reviewer;
GRANT EXECUTE ON FUNCTION stet.approve(uuid, text, bigint), stet.reject(uuid, text) TO reviewer;
```

- 에이전트 계정에는 주지 않는다. 에이전트 계정이 `stet` 스키마에 `USAGE`를 가지면 시작할 때 이미 경고한다(enforce 스펙 8장). 스키마 권한이 없으면 뷰도 함수도 쓸 수 없다.
- 검토자는 에이전트 계정의 구성원일 필요가 없고, 대상 테이블의 권한도 필요 없다.
- `stet.held_writes`를 직접 읽고 고칠 수 있는 것은 Stet의 계정과 superuser뿐이다.

## 7. 실행

**언제 도는가.** `mode: enforce`이고 게이트 대상 테이블이 있을 때만 돈다. 섀도에서는 Stet이 어떤 쓰기도 실행하지 않는다(규칙 6). 섀도에서 `stet.approve`를 부르면 줄은 `approved`가 되지만 아무도 집지 않고, 아래의 10분이 지나면 실행되지 않는다.

**집는 방법.** Stet은 1초마다 `approved`인 줄을 풀 연결로 읽는다. 첫 읽기는 시작하자마자다. 승인된 순서(`resolved_at`, 같으면 `id`)로 100줄까지 읽고, 그 순서로 한 번에 하나씩 처리한다. 읽기 하나는 5초를 넘기지 않는다. 줄마다 Stet의 계정으로 새 연결을 열고, 처리가 끝나면 그 연결을 닫는다. 풀의 연결을 쓰지 않는다. 에이전트가 쓴 문장이 그 연결의 설정을 바꿨을 수 있기 때문이다. 새 연결의 시작 설정은 풀과 같다(`lock_timeout` 200ms, `DateStyle` `ISO`, `IntervalStyle` `postgres`).

**한 줄의 처리.** 모두 한 트랜잭션이다. 줄의 상태 변경과 쓰기가 함께 커밋되거나 함께 버려진다. 줄 하나의 처리는 `held.apply_timeout`에 5초를 더한 시간을 넘기지 않는다. 넘으면 연결을 닫는다. 그것은 Stet 자신의 실패다(아래).

1. 트랜잭션을 연다. 줄에 `row_version`이 있거나 `row_absent`이면 `REPEATABLE READ`로, 아니면 `READ COMMITTED`로 연다. 격리 수준은 줄을 읽기 전에 정해야 하므로, 목록 질의가 줄마다 그 둘이 있는지를 함께 돌려준다. 고정 컬럼(4장)이어서 트랜잭션 밖에서 읽어도 같다. 목록은 그 둘이 없다고 했는데 잠근 줄에 있으면 Stet 자신의 실패로 보고 그만둔다.
2. 줄을 잠그고 읽는다(`WHERE id = $1 AND status = 'approved' FOR UPDATE SKIP LOCKED`). 없으면 다른 Stet이 처리 중이거나 이미 끝난 것이다. 그만둔다. `REPEATABLE READ`에서는 다른 Stet이 그 줄을 끝내고 커밋한 직후에 이 질의가 빈 결과 대신 `40001`로 실패한다(2장). 그것도 같은 뜻으로 보고 조용히 그만둔다. 승인한 지 10분이 지났는지도 이 질의에서 `clock_timestamp()`로 잰다.
3. 실행 전 확인. Stet의 계정으로 표의 순서대로 확인한다. 하나라도 걸리면 실행하지 않고 처음 걸린 것을 `failed`로 적는다.

| 확인 | `error` |
|---|---|
| `resolved_at`에서 10분이 넘었거나 `resolved_at`이 없다 | `approval_lapsed` |
| 줄의 `database`가 Stet이 붙은 데이터베이스가 아니다 | `other_database` |
| 줄의 `target_table`이 지금 설정의 게이트 대상이 아니다 | `not_gated` |
| 상한(6장)이 없다. `max_rows`도 `planned_rows`도 없다 | `no_row_limit` |
| `settings`가 없거나, 세 설정 가운데 없거나 빈 것이 있다 | `settings_missing` |
| `row_version`이 있거나 `row_absent`인데 `target_pk`가 없다 | `row_changed` |
| `row_version`이 있고, 대상 행의 지금 `xmin`이 그것과 다르다 | `row_changed` |
| `row_absent`인데 지금은 대상 행이 있다 | `row_changed` |
| 그 계정의 역할 수준 `search_path`(이 데이터베이스용이 있으면 그것, 없으면 전체용)가 이 연결의 `search_path`와 다르다 | `search_path_differs` |

- `no_row_limit`과 `settings_missing`은 함수와 게이트를 거친 줄에는 생기지 않는다. 줄을 SQL로 직접 넣거나 고친 경우다. 상한 없이 실행하지 않고, 빠진 설정을 Stet의 것으로 채워 실행하지 않는다. 키가 없는 행 버전은 확인할 수 없으므로 바뀐 것으로 본다.

4. 저장점을 두고, `SET LOCAL ROLE`로 줄의 `login_role`이 된다. 줄의 `settings` 셋을 `set_config(<이름>, <값>, true)`로 맞춘다. `statement_timeout`과 `lock_timeout`을 `held.apply_timeout`으로 둔다. 이 단계가 서버 에러로 실패하면 문장의 실패처럼 저장점까지 되돌리고 `failed`(`SQLSTATE <코드>: <서버의 메시지>`)로 적는다. Stet의 계정이 그 역할의 구성원이 아니면 `42501`, 역할이 지워졌으면 `22023`이다(2장).
5. 줄의 문장을 확장 프로토콜로 보낸다. 값은 저장된 텍스트 그대로(NULL은 NULL, 빈 글자는 빈 글자), 타입은 `param_types` 그대로다. 결과 행은 버린다. 이어서 같은 역할로 `SET CONSTRAINTS ALL IMMEDIATE`를 보낸다. 지연된 제약이 여기서 검사되고, 어기면 문장의 실패로 적힌다. 커밋에서 실패하면 Stet 자신의 실패가 되어 줄이 `approved`로 남기 때문이다.
6. 실행 뒤 확인. 걸리면 저장점까지 되돌리고 `failed`로 적는다.

| 확인 | `error` |
|---|---|
| 문장이나 5단계의 `SET CONSTRAINTS`가 실패했다 | `SQLSTATE <코드>: <서버의 메시지>`. `40001`이면 `row_changed` |
| `current_user`가 줄의 `login_role`이 아니다 | `role_changed` |
| 바뀐 행 수가 상한(6장)을 넘는다 | `rows_exceeded: <바뀐 수> rows, approved up to <상한>` |

7. `RESET ROLE` 뒤에 줄을 `applied`로 바꾸고 `applied_at`, `rows_affected`를 적는다. 커밋한다. `applied_at`은 `applied`로 적을 때도 `failed`로 적을 때도 `clock_timestamp()`다. 트랜잭션이 시작한 시각이 아니라 실제로 끝낸 시각이다.

- **행 버전.** 단일 행 `UPDATE`·`DELETE`의 줄에는 보류될 때의 `xmin`이 있다. 3단계에서 지금 값을 Stet의 계정으로 읽어 비교한다. `REPEATABLE READ`이므로, 읽은 뒤에 다른 트랜잭션이 그 행을 바꾸고 커밋하면 서버가 문장을 `40001`로 거부한다. 그래서 이 확인에는 틈이 없다. 프록시가 `commit`을 보낼 때의 확인(enforce 스펙 5장)에는 틈이 있었다. 행이 사라졌으면 그대로 실행한다. 0행에 걸릴 뿐이다. 없던 행이 생긴 것은 바뀐 것이다(enforce 스펙 5장과 같다). 읽은 뒤에 생긴 행은 이 트랜잭션에 보이지 않으므로 문장은 0행에 걸린다.
- **`search_path`.** 문장의 스키마 없는 이름은 이 연결의 `search_path`로 풀린다. 검토자가 본 상태도 Stet이 같은 `search_path`로 읽은 것이다. 에이전트 계정의 역할 수준 설정이 다르면 에이전트가 뜻한 테이블이 다른 것일 수 있으므로 실행하지 않는다. 시작할 때의 확인이 이미 같은 것을 경고한다(enforce 스펙 8장).
- `row_version`도 `row_absent`도 없는 줄(`INSERT`, 다중 행 쓰기, 상태를 읽지 못해 보류된 쓰기)은 이 확인을 하지 않는다. 행 수의 상한만 본다.
- **바뀐 행 수**는 서버가 답한 그 문장의 행 수다. 판단 기록의 `rows_affected`와 같은 뜻이다. 연쇄 삭제로 지워진 다른 테이블의 행은 세지 않는다.
- **`failed`는 끝이다.** 다시 시도하지 않는다. 시간 초과(`57014`)도 같다. 잠금을 기다리다 넘긴 것도 `statement_timeout`과 `lock_timeout`이 같은 값이어서 `57014`로 끝난다(2장). 사람이 본 때와 상황이 달라졌을 수 있기 때문이다.
- **Stet 자신의 실패.** 처리하다가 Stet 자신의 질의가 실패하면(연결이 끊겼다, 행 버전을 읽다가 Stet의 `lock_timeout`을 넘겼다, 줄을 고치지 못했다, 커밋이 실패했다, 줄 하나의 상한을 넘겼다) 트랜잭션을 버린다. 줄은 `approved`로 남는다. 쓰기와 상태 변경이 한 트랜잭션이므로 반영된 것이 없고, 두 번 실행되지 않는다. 그 Stet은 그 줄을 30초 동안 다시 집지 않고 다음 줄로 간다. 1초마다 다시 집으면 상한을 넘긴 문장이 승인이 만료될 때까지 되풀이해 돌고, 트랜잭션과 함께 되돌려지지 않는 일(시퀀스 등)이 그때마다 남기 때문이다.
- Stet이 끝날 때는 처리 중인 줄 하나가 끝나기를 기다린다. 상한은 줄 하나의 상한(`held.apply_timeout`에 5초를 더한 값)이다. 넘으면 연결을 닫는다. 서버가 그 트랜잭션을 버리고, 줄은 `approved`로 남는다. 끄기 시작한 뒤에는 새 줄을 집지 않는다. 실행부는 프록시와 게이트 다음, 풀을 닫기 전에 멈춘다.
- Stet이 여럿이어도 한 줄은 한 번만 실행된다(2단계의 잠금).
- 모델을 부르지 않는다. 이 트랜잭션은 모델을 기다리지 않는다(규칙 1).

**기록과 누적 한도**

- 실행 결과는 그 줄에 남는다. `stet.decisions`에는 새 줄을 넣지 않고, 원래의 판단 기록도 고치지 않는다(규칙 3). 원래 기록은 "막았다"(`executed = false`, `refused = ST001`)는 사실 그대로다.
- 승인으로 실행된 행은 그 계정의 누적 한도에 세지 않는다. 누적 한도는 "사람이 보지 않고 바꿀 수 있는 양"이다. 세면, 한도를 넘어 보류된 큰 쓰기 하나를 승인한 뒤 그 계정의 쓰기가 창이 지날 때까지 전부 보류된다.
- 로그 한 줄을 남긴다. 실행하면 info(`held write applied`), `failed`로 적으면 warn(`held write failed`)이다. id, 계정, 테이블, 행 수나 이유가 들어간다. Stet 자신의 실패는 warn(`held write not applied; it stays approved`)이고 에러와 다시 집을 때까지의 시간이 들어간다. 목록을 읽지 못하면 그때마다 warn(`approved held writes not listed`)이다.

## 8. 만료

- `pending`인 줄은 `expires_at`까지만 승인할 수 있다. 기본은 들어온 때부터 24시간이다.
- 만료는 저장하는 상태가 아니다. `stet.approve`가 `clock_timestamp()`로 시각을 비교하고, 뷰가 `now()`로 비교해 `expired`로 보여 준다. Stet이 꺼져 있어도 만료된다.
- 검토가 밀리거나 볼 사람이 없으면 쓰기는 실행되지 않는다. 다른 사람에게 넘기지 않는다.
- 승인된 줄은 10분 안에 실행되지 않으면 실행하지 않는다(`approval_lapsed`). 실행부가 줄을 집을 때 `resolved_at`과 `clock_timestamp()`를 비교한다. Stet이 꺼져 있던 사이의 승인이 한참 뒤에 실행되는 것을 막는다. 10분은 설정이 아니다.

## 9. 설정

```yaml
held:
  expire_after: 24h      # 보류 건을 승인할 수 있는 기간. 1m 이상 30d 이하. 기본 24h
  apply_timeout: 30s     # 승인된 쓰기 하나를 실행하는 시간의 상한. 1s 이상 10m 이하. 기본 30s
```

섀도에서도 검증한다. 섀도에서는 쓰이지 않는다.

## 10. 시작할 때의 확인

enforce에서 게이트 대상 테이블이 있으면 둘을 더 경고한다. 시작은 한다. 섀도에서는 경고하지 않는다.

- Stet의 계정이 superuser다. 승인된 문장은 Stet의 연결에서 실행된다. `SET LOCAL ROLE`은 문장이 스스로 되돌릴 수 있다(11장). 그때 문장이 얻는 권한이 Stet 계정의 것이다. 고칠 방법(`stet` 스키마와 에이전트 계정의 구성원 자격만 가진 계정으로 붙는다)을 말한다.
- Stet의 계정이 어떤 에이전트 계정의 구성원이 아니다. 그 계정의 보류 건은 승인해도 `failed`(`42501`)가 되고, 다중 행 쓰기는 재지 못한다. 고칠 문장(`GRANT <에이전트> TO <Stet>`)을 말한다. `SET ROLE`로 될 수 있으면 구성원으로 센다(NOINHERIT 구성원 포함). superuser는 모든 역할의 구성원이므로 이 경고가 나오지 않는다. 없는 계정은 이 경고 대신 enforce 스펙 8장의 "계정이 없다"는 경고만 나온다.
- 확인하지 못한 것은 "확인하지 못했다"고 경고한다.

## 11. 한계

- **`SET LOCAL ROLE`은 보안 경계가 아니다.** 승인된 문장은 에이전트가 쓴 것이고, Stet의 연결에서 돈다. 문장 안의 함수(`set_config('role', ...)`)가 역할을 Stet의 계정이 될 수 있는 다른 역할로 바꿀 수 있다. Stet은 실행 뒤에 `current_user`를 확인해 다르면 되돌리고, 연결을 버린다. 되돌릴 수 없는 일(시퀀스, 서버 밖으로 나가는 함수)과, 바꿨다가 되돌려 놓는 문장은 잡지 못한다. 검토자는 문장을 읽고 승인하므로, 그런 함수가 든 문장을 승인하지 않는 것이 첫 방어다. Stet의 계정에는 `stet` 스키마와 에이전트 계정의 구성원 자격만 주기를 권한다. 설계 대상은 실수하는 에이전트다(D30).
- 승인도 공격면이다(D31). 보류를 쏟아내 검토자를 지치게 할 수 있다. 같은 쓰기를 다시 보내면 줄이 하나씩 더 생기고, 이 단계는 그 수를 묶지 않는다.
- 문장은 승인된 때 실행된다. `now()`나 `current_date`는 그때의 값이다. 그 사이 다른 행이 바뀐 것은 확인하지 않는다. 중복으로 보류된 `INSERT`를 승인하면, 그 사이 같은 사람이 또 들어왔어도 실행된다.
- `row_version`이 없는 쓰기는 행이 바뀌었는지 확인하지 않는다. 다중 행 쓰기는 행 수만 본다. 같은 수의 다른 행이 걸려도 실행된다.
- 세션 설정은 셋만 옮긴다. 에이전트가 세션 안에서 바꾼 `search_path`, `SET ROLE`, 그 밖의 설정(`lc_monetary` 등)은 옮기지 않는다. enforce 스펙 13장의 한계와 같다.
- `SET`으로 세 설정 가운데 하나를 바꾸기 전에 준비한 문장(Parse)이나 묶은 포털(Bind)로 보낸 쓰기는, 보류되면 `held_write` 없이 막힌다(4장). 클라이언트가 그 문장을 다시 준비할 때까지 그렇다. `SET`에 답이 오기 전에 준비한 문장도 같다. 문장을 한 번 준비해 두고 오래 쓰는 드라이버가 세션 중간에 `SET TimeZone`을 보내면, 그 뒤로 그 문장으로 보낸 쓰기의 보류 건은 승인할 수 없다.
- 서버 설정을 다시 읽게 하면(`ALTER SYSTEM`과 `pg_reload_conf()`) 세 설정 가운데 하나가 ReadyForQuery와 다음 쓰기 사이에 바뀔 수 있다. 프록시는 그 단위의 ReadyForQuery에서야 그것을 안다. 그 사이에 보류된 쓰기는 옛 값으로 저장된다.
- 바이너리 값과 텍스트 값이 한 Bind에 섞인 쓰기는, 바이너리 값을 풀려고 모든 값을 Stet의 연결에서 한 번 읽는다. 저장하는 것은 텍스트 값의 온 바이트지만(4장), 이 읽기에서 텍스트 값도 Stet 연결의 설정으로 읽힌다. 에이전트 세션에서는 읽히지만 Stet 연결의 설정으로는 읽히지 않는 값(`DateStyle`이 `DMY`인 세션의 `'13/01/2026'`)이 있으면 바인딩 값을 읽지 못한 것이 된다. 그 쓰기는 보류되면 `held_write` 없이 막힌다.
- 실행하는 동안 `session_user`는 Stet의 계정이다. 권한 확인과 행 수준 보안은 에이전트 계정(`current_user`)으로 되지만, `session_user`를 읽는 기본값, 트리거, 정책은 Stet을 본다.
- 명시적 트랜잭션 안에서나 한 Sync 아래 다른 문장과 함께 보류된 줄을 승인하면 그 쓰기 하나만 실행된다. 에이전트가 같은 트랜잭션에서 하려던 나머지는 실행되지 않았다. 검토자에게는 `in_transaction`으로 보인다.
- 에이전트는 결과를 모른다. `ST001`을 받은 뒤 승인됐는지, 실행됐는지 알 길이 없다. 승인되어 실행된 쓰기를 에이전트가 다시 보내면 새로 판단받는다.
- 결과는 검토자에게 바로 가지 않는다. 검토자가 뷰를 다시 읽어야 `applied`인지 `failed`인지 안다. 대개 1~2초 안이다.
- 승인으로 실행된 쓰기는 `stet.decisions`에 "실행됨"으로 남지 않는다. 쓰기 스트림을 재생하거나 최근 변경을 조립할 때 그 쓰기는 보이지 않는다. `stet.held_writes`를 함께 읽어야 한다.
- Stet이 죽은 사이에 보류된 줄은 `unconfirmed`로 남아 승인할 수 없다. 에이전트는 id를 들었을 수 있다.
- 한 번에 하나씩 실행한다. 오래 걸리는 쓰기 하나가 뒤의 승인을 `held.apply_timeout`까지 세워 둔다.
- 한 번에 읽는 것은 승인된 순서로 100줄이다. 그 100줄이 모두 Stet 자신의 실패로 30초를 기다리는 줄이면, 그 뒤의 승인은 그동안 실행되지 않는다. 그 상태가 이어지면 10분이 지나 `approval_lapsed`가 된다.
- 실행부 안에서 panic이 나면 Stet 프로세스가 끝난다. 처리하던 트랜잭션은 서버가 버리고 줄은 `approved`로 남는다. 다시 시작한 Stet은 그 줄을 먼저 집는다.
- `stet.held_writes`의 보존 기간이 없다. 줄은 지우기 전까지 남는다.
- **스키마는 바뀌었을 때만 올린다(4장).** 스키마의 DDL은 Stet의 표에 강한 잠금을 잡고, 시작하는 Stet은 잠금을 200ms만 기다린다(`lock_timeout`). 파일을 실행할 때마다 올리면 검토자의 열린 트랜잭션(커밋하지 않은 `stet.approve`, `stet.review`의 읽기)이나 쓰기를 실행하고 있는 다른 Stet 때문에 Stet이 시작하지 못한다. 대가는 둘이다. `stet.schema_version`이 지금 파일의 값인 동안에는 누가 손으로 고친 스키마를 Stet이 되돌리지 않는다. 지운 인덱스나 트리거도, 손으로 PUBLIC에 준 두 함수의 `EXECUTE`도 그대로 남는다. 또 파일이 바뀐 뒤 처음 시작하는 Stet은 여전히 잠금을 잡는다. 그때 그런 트랜잭션이 열려 있으면 시작하지 못하고, 다시 시작해야 한다.

## 12. 패키지별 변경

| 패키지 | 바뀌는 것 |
|---|---|
| `internal/store` | `schema.sql`: 4장의 컬럼과 트리거, `stet.schema_version`, 5장의 뷰, 6장의 두 함수와 권한 거두기. `Migrate`는 파일이 바뀌었을 때만 실행한다(4장). 보류 줄을 `unconfirmed`로 넣기, `pending`·`undelivered`로 바꾸기(`MarkDelivered`, `MarkUndelivered`), 승인된 줄의 목록, 집기, 결과 적기(`ApprovedWrites`, `ClaimApproved`, `MarkApplied`, `MarkFailed`) |
| `internal/sqlparse` | 메시지에 `SET`이나 `RESET`이 있는지(`Result.Sets`) |
| `internal/proxy` | enforce 세션에서 `TimeZone`, `DateStyle`, `IntervalStyle`의 ParameterStatus를 따라가 `SessionInfo`로 게이트에 넘긴다. 그 값이 쓰기의 값이 읽힌 때의 것이 아닐 수 있으면 알린다(`SetPending`, 4장). 그 밖의 동작은 바뀌지 않는다 |
| `internal/gate` | 보류할 때 `expires_at`, `settings`, `row_absent`, `planned_rows`, `rows_exact`를 채운다. 설정을 모르는 쓰기는 저장하지 않는다. 결과를 받을 때 줄을 `pending`이나 `undelivered`로 바꾼다(4장의 표). 텍스트 바인딩 값을 온 바이트 그대로 둔다 |
| `internal/apply` (새 패키지) | 7장. 한 줄의 처리(`Apply`), 1초마다 집는 루프와 멈추기(`Start`, `Close`), 역할 수준 `search_path` 읽기(`RoleSearchPath`, `SchemaNames`. 시작할 때의 확인도 쓴다) |
| `internal/config` | `held.expire_after`, `held.apply_timeout` |
| `cmd/stet` | enforce에서 게이트 대상 테이블이 있으면 실행부를 켜고, 끌 때 프록시와 게이트 다음, 풀을 닫기 전에 멈춘다. 풀 연결의 `DateStyle`·`IntervalStyle` 고정. 10장의 두 경고 |
| 문서 | `stet.example.yaml`, spec-v0.2-enforce 7·8·9·13·17장과 spec-v0.1 8장의 해당 문장 |

판정 규칙과 모델 인터페이스는 바뀌지 않는다(규칙 7). 프록시의 지연 예산(2ms)을 그대로 확인한다.

## 13. 수용 기준

- [x] 보류된 쓰기의 줄은 `unconfirmed`로 들어가고, 거부가 전달되면 `pending`이 된다. 전달되지 않았거나 삽입이 실패로 보고됐거나 프록시가 실행됐다고 보고했으면 `undelivered`가 된다. (`TestInsertHeldWrite`, `TestMarkDelivered`, `TestMarkUndelivered`, `TestHeldWriteCarriesWhatApprovalNeeds`, `TestEnforceMarksAHiddenHoldUndelivered`, `TestEnforceMarksAnUnconfirmedHeldWriteUndelivered`, `TestHeldWriteTheProxyReportsAsSentIsUndelivered`)
- [x] `stet.review`가 줄과 판단 기록의 근거를 함께 보여 준다. 판단 기록이 없는 줄도 나온다. `expires_at`이 지난 `pending`은 `expired`로 보인다. (`TestReviewShowsAHeldWriteWithItsDecision`, `TestRowsHeldBeforeThisStageCannotBeApproved`)
- [x] 권한을 받은 검토자가 `stet.approve`를 부르면 줄이 `approved`가 되고 `resolved_by`가 그 접속 계정이다. 권한을 받지 않은 계정은 뷰도 함수도 쓰지 못한다. 스키마를 다시 올려도 검토자의 권한이 남고 PUBLIC에는 없다. (`TestApproveMarksTheRow`, `TestOnlyGrantedRolesReview`. 스키마를 다시 올리는 것은 `stet.schema_version`의 값을 바꿔 `schema.sql`이 다시 돌게 해서 확인했다. PUBLIC이 쓸 수 없는 것은 `stet` 스키마의 `USAGE`만 받은 계정이 두 함수에서 `42501`을 받는 것으로 확인했다)
- [x] 6장의 표의 줄마다 `ST005`로 거절된다. (`TestApproveAndRejectRefusals`. 표의 순서도 확인한다. 트랜잭션이 시작한 뒤 만료된 줄은 `TestApproveAndRejectReadTheClock`)
- [x] 한 Sync 아래에서 `SET TimeZone` 뒤에 온 쓰기가 보류되면 줄이 생기지 않고, 에러에 `held_write`가 없다. (`TestSetPendingUntilReadyForQuery`, `TestGateSeesSessionSettings`, `TestHeldWriteBehindASetUnderOneSyncIsNotStored`)
- [x] `BEGIN` 뒤의 첫 쓰기로 보류된 줄은 `in_transaction`이 참이고, 승인하면 그 쓰기만 실행된다. (`TestFirstWriteAfterBeginIsHeldInTransaction`, `TestApprovingTheFirstWriteOfATransactionRunsOnlyIt`)
- [x] 승인된 단일 행 `UPDATE`가 실행되고 줄이 `applied`가 된다. 값은 에이전트가 보낸 그대로다. 텍스트 값과 바이너리로 온 값, NULL, 타입을 준 값과 주지 않은 값 모두에서 확인한다. (`TestHeldWriteKeepsTextValuesAsSentAmongBinaryOnes`, `TestHeldWriteKeepsBinaryAndNullParamsAsText`, `TestApplyRunsAnApprovedUpdate`, `TestApplySendsValuesAsStored`, `TestApplySendsAnEmptyValueAsEmpty`, `TestApprovedWriteRunsAsTheAgentSentIt`. 마지막 것은 프록시, 게이트, 함수, 실행부를 모두 거친다. 한 Bind에 타입 없는 텍스트 날짜, 타입 없는 NULL, 타입을 준 바이너리 정수를 보낸다)
- [x] 실행은 에이전트 계정의 권한으로 된다. 그 계정에 권한이 없는 테이블에 쓰는 줄은 `failed`(`42501`)가 되고, 행 수준 보안이 그 계정에 적용된다. (`TestApplyRunsWithTheAgentsRights`. Stet이 그 계정이 될 수 없을 때는 `TestApplyFailsWhenStetCannotBecomeTheRole`)
- [x] 에이전트 세션의 `TimeZone`과 `DateStyle`이 Stet의 것과 다를 때, 승인된 쓰기가 에이전트 세션에서 실행했을 때와 같은 값을 넣는다. (`TestApplyReadsValuesUnderTheAgentsSettings`, `TestApprovedWriteRunsAsTheAgentSentIt`. 둘 다 `Asia/Seoul`과 `ISO, DMY`에서 `'01/02/2026 10:00'`이 2월 1일 01:00 UTC로 들어가는 것을 본다)
- [x] 보류된 뒤 대상 행이 바뀌었으면 `failed`(`row_changed`)이고 행은 그대로다. 실행 트랜잭션이 연 뒤 문장을 보내기 전에 다른 연결이 그 행을 바꿔도 같다. 행이 사라졌으면 `applied`이고 0행이다. 보류될 때 없던 행이 생겼으면 `failed`(`row_changed`)다. 에이전트 계정의 역할 수준 `search_path`가 다르면 `failed`(`search_path_differs`)다. (`TestApplyChecksTheRowVersion`, `TestApplyRefusesAnotherSearchPath`, `TestApplyRefusesARowVersionWithoutItsKey`)
- [x] 바뀐 행 수가 상한을 넘으면 `failed`(`rows_exceeded`)이고 테이블은 그대로다. `max_rows`를 준 승인은 그 값까지 실행된다. (`TestApplyKeepsToTheApprovedRows`. 상한이 없는 줄의 `no_row_limit`도 본다)
- [x] 문장이 역할을 바꾸면 `failed`(`role_changed`)이고 바뀐 것이 없다. 그 뒤의 줄은 정상으로 실행된다(연결을 버렸다). (`TestApplyUndoesAWriteThatChangesRole`)
- [x] 문장이 서버에서 실패하면 `failed`이고 `error`에 SQLSTATE와 메시지가 있다. 시간 상한을 넘어도 같다. (`TestApplyRecordsTheServersError`. 잠금 대기가 `57014`로 끝나는 것과 지연된 제약도 본다)
- [x] 거부된 줄, 만료된 줄, `undelivered`와 `unconfirmed`인 줄은 실행되지 않는다. 승인하고 10분이 지난 줄은 `failed`(`approval_lapsed`)다. (`TestApplyLeavesOtherStatusesAlone`, `TestLoopPicksUpApprovalsMadeWhileItWasDown`)
- [x] 두 실행부가 같은 줄을 동시에 집어도 쓰기는 한 번만 실행된다. 실행부의 질의가 중간에 실패하면 줄은 `approved`로 남고 쓰기는 반영되지 않는다. (`TestTwoAppliersRunAWriteOnce`, `TestApplyLeavesTheRowApprovedWhenItsOwnStepFails`, `TestApplyLeavesTheRowApprovedWhenItsBoundPasses`, `TestLoopGoesOnAfterAFailure`. 7장 2단계에서 다른 Stet이 막 끝낸 줄을 `40001`로 받는 경로는 한 문장 안의 경주라 테스트가 없다)
- [x] 섀도 모드에서는 `approved`인 줄이 있어도 아무것도 실행되지 않는다. (`TestShadowModeRunsNoApprovedWrite`)
- [x] 승인으로 실행된 행은 누적 한도에 들지 않는다. (`TestAppliedRowsDoNotCountTowardTheCumulativeLimit`)
- [x] `stet.held_writes`의 문장이나 값을 고치는 `UPDATE`, 그림에 없는 상태 변경은 `ST004`로 거부된다. (`TestHeldWritesContentIsFixed`, `TestHeldWritesStatusFollowsTheArrows`, `TestHeldWritesResultsMoveWithTheirArrow`)
- [x] Stet을 끌 때 처리 중인 줄이 끝나거나 버려지고, 반쯤 반영된 것이 없다. (`TestCloseWaitsForTheWriteBeingApplied`, `TestCloseAbandonsAWritePastItsBound`, `TestRunStopsTheApplierBeforeThePool`. 마지막 것은 끌 때 처리 중이던 줄이 `applied`로 끝나고 에러 로그가 없는 것을 본다. 실행부와 풀을 닫는 순서는 밖에서 보이지 않아 코드로만 확인했다)
- [x] 10장의 두 경고가 나온다. (`TestCheckWarnsWhenStetIsASuperuser`, `TestCheckWarnsWhenStetIsNotAMemberOfAnAgentRole`, `TestCheckInShadowSaysNothingOfApprovals`)
- [x] 프록시의 2ms 지연 예산을 지킨다. `go test -race ./...`가 통과한다. (`TestPassthroughLatencyBudget`, `TestObservedReadLatencyBudget`, `TestUnwatchedSessionLatencyBudget`, `TestEnforcedSessionLatencyBudget`, `TestEnforcedSessionSettingsLatencyBudget`. 마지막 확인에서 프록시가 더한 p95는 차례로 33.9µs, 41.8µs, 28.5µs, 28.1µs, 29.7µs였다. `STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...`가 모든 패키지에서 `ok`)

마무리 확인(2026-11-17)에서 빌드한 바이너리(`bin/stet proxy`)를 enforce로 띄우고 `psql`로 끝에서 끝까지 확인했다. 5행 테이블에 `impact: {max_rows: 3}`를 두고 에이전트가 모든 행을 고치는 `UPDATE`를 보내면 `ST001`과 `held_write`를 받는다. 검토자의 `stet.review`에는 `pending`, `planned_rows` 4, `rows_exact` false로 보인다. `max_rows` 없는 승인은 `ST005`로 거절되고, `max_rows` 5를 준 승인은 1초 안에 `applied`(5행)가 된다. 거부한 줄은 실행되지 않는다. 누적 한도로 보류된 단일 행 `UPDATE`는 다른 연결이 그 행을 바꾼 뒤 승인하면 `failed`(`row_changed`)가 된다. 10장의 두 경고가 나오고, 섀도로 띄운 Stet은 `approved`인 줄을 그대로 둔다. `SIGTERM`으로 끄면 에러 없이 끝난다.

## 14. 정한 것과 남긴 것

**정한 것**

- 승인된 쓰기는 Stet 프로세스가 실행한다. 함수는 표시만 한다.
- 실행은 에이전트 계정의 권한으로, 에이전트 세션의 날짜·시간 설정으로 한다.
- 누가 승인하는지는 DB 권한이 정한다. 검토자에게 필요한 것은 뷰 하나와 함수 둘이다.
- 승인은 사람이 본 것까지다. 행 버전이 같아야 하고, 바뀌는 행 수가 승인한 수를 넘지 않아야 한다.
- 에이전트에게 전달된 것이 확인된 보류 건만 승인할 수 있다.
- `failed`와 만료는 실행하지 않는 쪽이다. 다시 시도하지 않는다. Stet 자신의 실패로 처리하지 못한 줄만 30초 뒤에 다시 집는다.
- 승인으로 실행된 행은 누적 한도에 세지 않는다.
- 승인·거부 함수와 뷰는 오픈소스 코어에 둔다(v0.1 스펙대로).
- 스키마는 `schema.sql`이 바뀌었을 때만 올린다. 검토자와 다른 Stet의 열린 트랜잭션이 시작하는 Stet을 막지 않게 하려는 것이다.

**남긴 것**

- 에이전트가 자기 보류 건의 결과를 아는 길.
- 검토 화면, 알림, 담당자와 넘기기, 사람에게 올리는 양의 목표(D31의 열린 질문).
- 보류 건의 수를 묶는 것, 같은 쓰기의 보류를 하나로 합치는 것.
- 승인 결과를 판단 기록의 라벨로 잇는 것. 승인이 곧 "잘못 막았다"는 뜻은 아니어서 자동으로 붙이지 않았다.
- 승인으로 실행된 쓰기를 최근 변경과 재생에 넣는 것.
- 여러 줄을 한 번에 승인하는 것, 승인 취소.
- `stet.held_writes`의 보존 기간.
- Stet의 연결이 아니라 에이전트로 인증된 연결에서 실행하는 것. 그래야 역할이 진짜 경계가 된다.
