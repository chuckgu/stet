# Stet v0.2 4단계 스펙: 되돌리기

상태: 설계 (2026-11-18). 근거는 concept-v0.2 7장의 2단("되돌릴 수 있게 하기")과 9장의 4번, D31, docs/benchmark-hoop.md의 R3다.

## 1. 목표와 비목표

Stet은 에이전트의 쓰기를 실행 전에 판단한다. 판단이 틀렸거나 판단이 보지 못한 쓰기는 그대로 남는다. 벤치마크에서 Stet은 결함 400건 가운데 11건을 놓쳤고, 함수와 뷰를 거친 쓰기(`s5.call`, `s5.view`)는 보지 못했다. 이 단계는 에이전트 계정이 바꾼 행의 이전 값을 남기고, 사람이 그것을 골라 되돌리는 가장 짧은 길을 만든다.

**목표**

- 에이전트 계정이 바꾼 행마다 바뀌기 전과 뒤의 값을 남긴다. 쓰기와 같은 트랜잭션 안에서 DB가 남긴다. 롤백된 쓰기는 남지 않는다.
- 문장이 어떤 길로 왔든 남긴다. Stet이 판단한 쓰기, 함수와 뷰를 거친 쓰기, 연쇄로 바뀐 행, Stet을 거치지 않고 붙은 에이전트 계정의 쓰기, 승인으로 실행된 쓰기가 모두 든다.
- 검토자가 계정, 시간 범위, 테이블로 골라 되돌린다(`stet.undo`). 되돌리기 전에 무엇이 어떻게 될지 본다(`stet.undo_plan`).
- 되돌리기는 다른 쪽의 변경을 지우지 않는다. 그 뒤에 누가 바꾼 행은 건너뛰고 알린다.
- 남긴 변경을 판단 기록에 잇는다. 판단 기록이 없는 변경은 Stet이 판단하지 않은 쓰기다.

**비목표**

- 검토 화면, 되돌리기의 자동 실행, 되돌리기를 다시 되돌리기.
- 업무(`on_behalf_of`)나 세션으로 고르기, 판단 기록의 id로 고르기(14장).
- 읽어 간 것. 되돌릴 수 없다(concept 7장).
- DB 밖으로 나간 것(시퀀스 값, `NOTIFY`, 서버 밖을 부르는 함수)과 캡처하지 않는 테이블.
- "되돌릴 수 있으면 덜 막는다"는 판정 정책(D31). 이 단계는 재료만 만든다.

**실패로 볼 것.** 되돌리기가 다른 쪽이 바꾼 값을 조용히 덮어쓰는 것. 남긴 이전 값이 실제 이전 값과 다른 것. 캡처 때문에 에이전트의 쓰기가 실패하는 것. 변경이 다른 판단 기록에 이어지는 것.

## 2. 접근

**쓰기 전 값을 누가 잡는가.** DB가 잡는다(2026-11-18에 정함).

| | 동작 | 버린 이유 또는 고른 이유 |
|---|---|---|
| **T. DB가 잡는다** | 테이블에 트리거를 달아, 바뀐 행의 이전·이후 값을 쓰기와 같은 트랜잭션에서 Stet의 표에 남긴다 | 고른 것. 정확하다. 롤백된 쓰기는 남지 않고, DB가 만든 키의 `INSERT`, 다중 행, 연쇄, 함수·뷰 경유를 모두 잡는다. 대가는 테이블 소유자가 트리거를 한 번 설치하는 것과, 에이전트의 쓰기마다 기록이 같은 트랜잭션에 더해지는 것이다 |
| P. 프록시가 미리 읽는다 | 쓰기를 보내기 직전에 걸리는 행을 읽어 저장한다 | 읽기와 실행 사이에 바뀐 행은 틀리게 남는다. DB가 만든 키의 `INSERT`는 되돌릴 수 없다(프록시는 응답의 행을 읽지 않는다). 함수·뷰·연쇄는 못 본다 |
| W. 복제 로그를 읽는다 | 논리 복제 슬롯으로 변경을 읽는다 | 서버 설정과 복제 권한이 필요하다. Stet이 멈추면 로그가 쌓인다. 관리형 Postgres에서 설정 변경과 재시작이 든다 |

**트리거의 종류.** 문장 단위 트리거와 전이 테이블(`REFERENCING OLD TABLE / NEW TABLE`)을 쓴다. 행 단위 트리거는 행마다 함수를 부른다. 문장 단위는 문장에 한 번 부르고, 바뀐 행 수를 먼저 세어 상한(4장)을 적용할 수 있다.

**행의 값을 남기는 형식.** 컬럼마다 그 값의 글자(`::text`)를 담은 JSON이다. `to_jsonb(행)`은 쓰지 않는다. `jsonb` 컬럼의 JSON `null`과 SQL `NULL`을 구분하지 못하고, `json` 컬럼의 글자가 바뀐다(아래의 확인).

**복구가 도는 곳.** 검토자의 세션 안의 SQL 함수다. Stet 프로세스가 필요 없다. 검토자가 자기 트랜잭션에서 부르고, 결과를 본 뒤 `COMMIT`하거나 `ROLLBACK`한다. 복구하는 쓰기는 검토자의 권한으로 된다. Stet의 계정에 고객 테이블의 쓰기 권한을 주지 않는다. (승인된 쓰기를 Stet 프로세스가 실행하는 것과 다르다. 그쪽은 에이전트의 권한으로 실행해야 했다.)

**변경을 판단 기록에 잇는 방법.** 프록시는 문장을 고치지 않으므로 판단 기록의 id를 DB에 넘길 길이 없다. 그래서 양쪽이 따로 적은 것으로 잇는다. 트리거는 백엔드 pid와 서버가 그 문장을 받은 시각을 적고, Stet은 판단 기록에 그 연결의 백엔드 pid와 문장이 서버에 있던 시간 창을 적는다(5장). 문장 앞에 주석을 붙이거나 세션 변수를 끼워 넣는 방법은 버렸다. 문장을 고치게 되고, 확장 프로토콜에서는 한 번 준비한 문장이 여러 번 실행된다.

**테스트 서버(PostgreSQL 14)에서 확인한 것.** 이 설계가 기대는 동작이다.

- 전이 테이블을 쓰는 문장 단위 `AFTER` 트리거는 직접 쓴 문장, 뷰를 거친 `UPDATE`, `SECURITY DEFINER` 함수 안의 `UPDATE`, `INSERT ... ON CONFLICT DO UPDATE`(넣은 행은 INSERT 트리거에, 고친 행은 UPDATE 트리거에), 연쇄 삭제된 자식 테이블의 행에서 모두 불린다. 0행을 바꾼 문장은 아무것도 남기지 않았다(전이 테이블이 비어 있다).
- 연쇄 삭제에서는 부모 테이블의 트리거가 자식 테이블의 것보다 먼저 불린다. 둘의 `statement_timestamp()`와 `current_query()`는 같다(클라이언트가 보낸 그 문장).
- `SECURITY DEFINER` 트리거 함수 안에서 `session_user`는 접속 계정이고 `current_user`는 함수의 소유자다. `SET LOCAL ROLE` 아래에서 `current_setting('role')`은 그 역할이고, 아니면 `none`이다.
- 트리거를 만드는 계정은 함수의 `EXECUTE`와 그 스키마의 `USAGE`가 있어야 한다(없으면 `permission denied for schema`, `permission denied for function`). 쓰는 계정은 둘 다 없어도 트리거가 불린다. 그 계정이 함수를 직접 부르거나 기록을 읽으려 하면 스키마 권한이 없어 거부된다.
- 트리거 함수의 `EXCEPTION WHEN OTHERS` 블록 안에서 기록이 실패하면(표의 제약을 어기게 했다) 에이전트의 `INSERT`는 성공하고 기록만 없다.
- 확장 프로토콜에서 한 Sync 아래 두 Execute의 `statement_timestamp()`는 서로 다르고, 각 문장의 `current_query()`는 그 문장의 글자다. 트랜잭션 id는 같다.
- `to_jsonb(행)`으로 남긴 값은 `jsonb` 컬럼의 `'null'`이 `NULL`로 돌아오고, `json` 컬럼은 공백과 중복 키가 달라진다. `TimeZone`이나 `extra_float_digits`가 다른 세션에서 다시 만들면 글자가 달라 같은 행이 다르게 보인다. 컬럼별 글자로 남기면 `'null'`은 `"null"`, `NULL`은 `null`로 구분된다.
- `GENERATED ALWAYS AS IDENTITY` 컬럼은 `OVERRIDING SYSTEM VALUE`로 값을 줘서 넣을 수 있다. 테이블 소유자가 아니어도 된다. 생성 컬럼(`GENERATED ALWAYS AS (...) STORED`)에는 값을 줄 수 없다.
- 오버헤드(같은 기기, 14개 컬럼의 테이블, 묶음 표 없이 한 표에 남긴 시험용 함수). 키로 한 행을 고치는 `UPDATE`의 평균 지연은 트리거가 없을 때 0.05ms, 트리거가 있고 에이전트 계정이 아닐 때 0.06ms, 캡처할 때 0.20ms였다. 10,000행을 고치는 `UPDATE`는 차례로 약 25ms, 40ms, 300ms였다. 변경 89,168건의 기록은 65MB였다(변경 하나에 약 0.7KB).

## 3. 흐름

1. 운영자가 설정에 되돌리기를 켤 테이블을 적는다(9장). `stet undo-sql`이 설치할 SQL을 출력하고, 테이블 소유자가 그것을 실행한다(4장).
2. 에이전트 계정의 쓰기가 그 테이블의 행을 바꾸면 트리거가 변경 묶음 하나와 행마다의 이전·이후 값을 남긴다. 쓰기와 함께 커밋되거나 함께 버려진다(4장).
3. Stet은 판단 기록에 백엔드 pid와 시간 창을 적는다(5장).
4. 검토자가 `stet.history`에서 변경과 그 판단을 본다(6장).
5. 검토자가 `stet.undo_plan(계정, 언제부터, ...)`으로 무엇이 되돌려지고 무엇이 건너뛰어질지 본다(7장).
6. 검토자가 자기 트랜잭션에서 `stet.undo(...)`를 부르고, 결과를 본 뒤 커밋하거나 롤백한다(7장).

캡처(2)와 되돌리기(5, 6)는 Stet 프로세스가 꺼져 있어도 된다. 판단 기록에 잇는 것(3)과 보존 기간(9장)만 Stet 프로세스가 한다.

## 4. 캡처

**설치.** 테이블마다 트리거 넷이다. `stet undo-sql`(9장)이 출력하고 테이블 소유자가 실행한다. Stet은 고객의 테이블에 아무것도 설치하지 않는다.

```sql
CREATE TRIGGER stet_capture_ins AFTER INSERT ON public.deals
  REFERENCING NEW TABLE AS n
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE TRIGGER stet_capture_upd AFTER UPDATE ON public.deals
  REFERENCING OLD TABLE AS o NEW TABLE AS n
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE TRIGGER stet_capture_del AFTER DELETE ON public.deals
  REFERENCING OLD TABLE AS o
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deals
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
```

- 인자는 설정의 테이블 키와 키 컬럼이다. 판단 기록의 `target_table`과 같은 이름으로 남기려는 것이다.
- 함수와 표는 `schema.sql`이 `stet` 스키마에 만든다. 소유자는 Stet의 계정이다. 트리거를 만들 계정에는 `stet` 스키마의 `USAGE`와 `stet.capture()`의 `EXECUTE`를 줘야 한다. `EXECUTE`는 PUBLIC에서 거둔다. 트리거 함수는 직접 부를 수 없으므로 이 권한으로 할 수 있는 일은 트리거를 만드는 것뿐이다.
- 받는 테이블은 보통 테이블뿐이다. 파티션 테이블과 그 파티션, 상속 자식이 있는 테이블은 받지 않는다(11장). 키 컬럼은 그 테이블의 기본 키이거나 `NOT NULL`인 유일 제약의 컬럼 하나여야 한다. 행을 키로 가리키기 때문이다.

**저장.**

```sql
-- 문장 하나가 한 테이블에 한 일
CREATE TABLE IF NOT EXISTS stet.change_sets (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at            timestamptz NOT NULL,          -- 남긴 때. clock_timestamp()
  stmt_at       timestamptz NOT NULL,          -- 서버가 그 문장의 메시지를 받은 때. statement_timestamp()
  txid          bigint,                        -- txid_current_if_assigned()
  pid           integer NOT NULL,              -- pg_backend_pid()
  login_role    text NOT NULL,                 -- session_user
  agent_role    text NOT NULL,                 -- 이 쓰기의 주인으로 보는 계정 (아래)
  app           text,                          -- application_name. 스스로 적는 값
  held_write    uuid,                          -- 승인으로 실행된 쓰기면 그 보류 줄의 id
  target_table  text NOT NULL,                 -- 설정의 테이블 키 (트리거의 첫 인자)
  relid         oid NOT NULL,
  op            text NOT NULL CHECK (op IN ('insert','update','delete','truncate')),
  query         text,                          -- current_query(). 클라이언트가 보낸 문장
  rows          bigint,                        -- 그 문장이 이 테이블에서 바꾼 행 수. truncate는 NULL
  skipped       text CHECK (skipped IN ('too_many_rows','too_many_statements','truncate','capture_failed')),  -- 값을 남기지 않은 이유
  detail        text                           -- capture_failed의 SQLSTATE와 메시지
);
-- 그 문장이 바꾼 행 하나. 키마다 한 줄이다
CREATE TABLE IF NOT EXISTS stet.changes (
  id      bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  set_id  bigint NOT NULL REFERENCES stet.change_sets (id) ON DELETE CASCADE,
  key     text NOT NULL,                       -- 키 컬럼 값의 글자
  before  jsonb,                               -- 문장 전에 그 키의 행. 없었으면 NULL
  after   jsonb                                -- 문장 뒤에 그 키의 행. 없어졌으면 NULL
);
-- 캡처 함수가 읽는 설정. 한 줄뿐이고 Stet이 시작할 때 설정 파일에서 맞춘다
CREATE TABLE IF NOT EXISTS stet.capture_settings (
  one       boolean PRIMARY KEY DEFAULT true CHECK (one),
  roles     text[],                            -- 캡처할 계정. NULL이면 모든 세션
  max_rows  bigint NOT NULL
);
```

인덱스는 `change_sets (agent_role, at)`, `change_sets (pid, stmt_at)`, `changes (set_id)`다. 세 표의 인덱스와 트리거는 없을 때만 만든다(11장의 잠금).

**`stet.capture()`가 하는 일.** `SECURITY DEFINER`이고 `search_path`를 `pg_catalog, pg_temp`로 고정한다. 값의 글자가 세션 설정에 따라 달라지지 않게 `DateStyle`을 `ISO, MDY`, `IntervalStyle`을 `postgres`, `extra_float_digits`를 `1`, `TimeZone`을 `UTC`, `bytea_output`을 `hex`로 함수 안에서만 고정한다(아래에서 "다섯 설정"). `client_min_messages`도 `warning`으로 고정한다. 함수가 남기는 로그가 클라이언트로 가지 않게 하려는 것이다.

1. `stet.capture_settings`를 읽는다. 줄이 없으면 모든 세션을 캡처하고 상한은 10,000이다.
2. 쓰기의 주인(`agent_role`)을 정한다. 접속 계정(`session_user`)이 함수의 소유자(Stet의 계정)면 그때의 역할(`current_setting('role')`, `none`이면 접속 계정)이고, 아니면 접속 계정이다. Stet의 계정이 `SET LOCAL ROLE`로 에이전트가 되어 실행하는 승인된 쓰기가 그 에이전트의 것으로 남는다.
3. `roles`가 있고 `agent_role`이 거기에 없으면 아무것도 하지 않고 끝낸다.
4. `TRUNCATE`면 묶음 하나를 `skipped = 'truncate'`로 남기고 끝낸다.
5. 바뀐 행을 센다. 0이면 아무것도 남기지 않는다. `max_rows`를 넘으면 묶음 하나를 `skipped = 'too_many_rows'`와 행 수로 남기고 끝낸다.
6. 묶음 하나와 행마다 한 줄을 남긴다.
   - 행의 값은 `jsonb_object(컬럼 이름의 배열, 값::text의 배열)`이다. 컬럼은 그때의 `pg_attribute`에서 읽는다(지워진 컬럼은 뺀다). 값은 모두 JSON 문자열이거나 `null`이다.
   - `INSERT`는 `after`만, `DELETE`는 `before`만 있다.
   - `UPDATE`는 이전 행들과 이후 행들을 **키로** 묶는다(`FULL JOIN`). 키가 그대로면 한 줄에 `before`와 `after`가 있다. 키가 바뀐 행은 두 줄이 된다. 옛 키의 줄은 `before`만, 새 키의 줄은 `after`만 있다. 그래서 줄은 언제나 "그 키의 행이 문장 전에는 무엇이었고 뒤에는 무엇인가"다. 이 두 줄은 되돌리지 않는다(7장의 `key_changed`).
7. 접속 계정이 Stet의 계정이면 `current_setting('stet.held_write', true)`를 `held_write`로 남긴다. 승인된 쓰기를 실행하는 쪽(`internal/apply`)이 문장 앞에 `set_config('stet.held_write', <id>, true)`를 둔다. 다른 계정의 세션에서는 이 설정을 읽지 않는다. 에이전트가 스스로 적을 수 있는 값이기 때문이다.

**캡처는 쓰기를 실패시키지 않는다.** 1부터 7까지 전부가 `EXCEPTION WHEN OTHERS` 블록 안이다. 어디서든 에러가 나면 그 블록이 한 일을 되돌리고, 묶음 하나를 `skipped = 'capture_failed'`와 SQLSTATE로 남긴다. 그것도 실패하면 서버 로그(`RAISE LOG`)에만 남긴다. 클라이언트에게는 아무 메시지도 가지 않는다. 삼키지 않는 것은 시간 초과와 취소뿐이다(11장).

**한 트랜잭션의 상한.** 그 블록은 표에 쓸 때마다 하위 트랜잭션 하나를 만든다. 한 트랜잭션의 하위 트랜잭션이 64개를 넘으면 서버 전체의 스냅숏 확인이 느려진다(PostgreSQL의 알려진 동작). 그래서 한 트랜잭션에서 값을 남기는 것은 캡처가 표에 쓴 처음 31번까지다(0행 쓰기와 캡처하지 않는 세션은 세지 않는다). 32번째는 묶음 하나를 `skipped = 'too_many_statements'`로 남기고, 그 뒤로 그 트랜잭션에서는 아무것도 남기지 않는다. 캡처하지 않는 세션(3번에서 끝나는 것)은 표에 쓰지 않으므로 하위 트랜잭션을 만들지 않는다. 세는 방법은 만드는 쪽이 정한다. 조건은 한 트랜잭션에서 캡처가 만드는 하위 트랜잭션이 32개를 넘지 않는 것이다.

- enforce에서 Stet을 지나는 트랜잭션은 게이트 대상 테이블에 쓰기를 하나만 보낼 수 있다(이미 무언가를 바꾼 트랜잭션의 쓰기는 `ST002`로 막힌다). 그래서 이 상한에 닿는 것은 섀도의 큰 트랜잭션, 함수 안에서 여러 번 쓰는 문장, Stet을 거치지 않은 세션이다.
- 캡처 여부는 Stet의 모드와 상관없다. 트리거가 달려 있으면 섀도에서도 enforce에서도, Stet이 꺼져 있어도 남는다.
- `agents.roles`가 비어 있으면(모든 세션이 게이트 대상) `roles`는 NULL이고 모든 세션의 쓰기를 남긴다.
- 기록은 바뀌지 않는다. 두 표의 `UPDATE`와 `TRUNCATE`는 트리거가 `ST004`로 거부한다. `DELETE`는 막지 않는다(보존 기간). 이 트리거들은 없을 때만 만든다(11장의 잠금).
- 에이전트 계정은 `stet` 스키마의 권한이 없으므로 기록을 읽거나 고칠 수 없다.

## 5. 판단 기록에 잇기

```sql
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS backend_pid integer;      -- 그 세션의 서버 쪽 백엔드 pid
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS sent_after  timestamptz;  -- DB 시계. 문장은 이보다 뒤에 서버로 갔다
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS done_before timestamptz;  -- DB 시계. 문장의 결과는 이보다 앞에 나왔다
CREATE INDEX IF NOT EXISTS decisions_backend_idx ON stet.decisions (backend_pid, sent_after);
```

- 셋 다 줄을 넣을 때만 적는다(규칙 3). 서버로 보내지 않은 쓰기와 pid를 모르는 세션의 쓰기는 NULL이다.
- `backend_pid`는 프록시가 서버의 BackendKeyData에서 읽는다. 지켜보는 세션에서만 읽고, 메시지는 그대로 넘긴다.
- 두 시각은 DB의 시계다. Stet은 자기 시계의 시각을 보내지 않고, 줄을 넣는 순간까지 흐른 시간을 보낸다. `sent_after`는 `clock_timestamp()`에서 "쓰기가 Stet에 들어온 때부터 흐른 시간"을 뺀 값이고, `done_before`는 "게이트가 결과를 받은 때부터 흐른 시간"을 뺀 값이다. 두 기기의 시계가 어긋나도 틀리지 않는다. 남는 오차는 삽입 문장이 서버에 닿는 시간뿐이다.

**잇는 규칙.** 변경 묶음 하나는 다음을 모두 만족하는 판단 기록이 **하나뿐일 때** 그 기록에 이어진다.

- `backend_pid`가 묶음의 `pid`와 같다.
- `executed`가 참이다.
- 묶음의 `stmt_at`이 `sent_after`에서 10ms를 뺀 때와 `done_before` 사이다. 앞쪽에만 여유를 둔다. 삽입 문장이 서버에 닿는 시간만큼 두 값이 실제보다 늦게 적히기 때문이다.
- `statement`가 묶음의 `query`와 같은 글자다.

- 하나도 없으면 Stet이 판단하지 않은 변경이다. 함수나 뷰를 거친 쓰기, Stet을 거치지 않고 붙은 세션의 쓰기, 판단 기록을 넣지 못한 쓰기다.
- 둘 이상이면 대상 키로 좁힌다. 판단 기록에 `target_pk`가 있고 `target_table`이 묶음의 것과 같으면, 그 키가 묶음의 `key` 가운데 있는 기록만 남긴다. 그래도 둘 이상이면 잇지 않고 후보의 수만 보여 준다. 잘못 잇는 것보다 잇지 않는 쪽이다.
- 후보가 여럿이 되는 것은 같은 연결에서 같은 글자의 문장이 10ms 안에 이어 실행됐을 때다. 한 Sync 아래의 일괄 실행과, 준비한 문장 하나를 값만 바꿔 빠르게 되풀이하는 것이 그렇다. 행이 서로 다르면 대상 키로 갈린다. 같은 행을 되풀이해 고치는 것과 대상 키가 없는 쓰기(DB가 키를 만드는 `INSERT`, 다중 행 쓰기)는 갈리지 않는다.
- 연쇄로 바뀐 자식 테이블의 묶음은 `stmt_at`과 `query`가 부모의 문장과 같으므로 같은 판단 기록에 이어진다.
- 승인으로 실행된 쓰기는 `held_write`로 보류 줄에, 거기서 원래의 판단 기록에 이어진다. 이 규칙을 쓰지 않는다.
- 잇는 것은 저장하지 않는다. 뷰가 읽을 때 계산한다(6장). 판단 기록은 쓰기가 끝난 뒤에 들어가고 묶음은 트랜잭션이 커밋돼야 보이므로, 어느 쪽이 먼저인지 정해져 있지 않다.
- `statement`와 `current_query()`가 같은 글자로 남는지는 단순 프로토콜과 확장 프로토콜 모두에서 테스트로 확인한다. 다르게 남는 경로가 있으면 그 경로의 쓰기는 이어지지 않는다.

## 6. 검토자가 보는 것: `stet.history`

변경마다 한 줄이다. 값을 남기지 않은 묶음(`skipped`)은 `key` 없이 한 줄로 나온다.

| 컬럼 | 뜻 |
|---|---|
| `change_id`, `set_id`, `at` | 변경과 묶음의 id, 남긴 때 |
| `agent_role`, `login_role`, `app` | 누가 |
| `target_table`, `op`, `key`, `before`, `after` | 무엇을. `op`는 문장의 종류다 |
| `rows`, `skipped`, `detail`, `query` | 그 문장이 바꾼 행 수, 값을 남기지 않은 이유, 문장 |
| `held_write` | 승인으로 실행된 쓰기면 그 보류 줄 |
| `decision_id`, `decision_candidates` | 이어진 판단 기록(5장). 하나로 정해지지 않으면 `decision_id`는 NULL이고 후보의 수가 나온다 |
| `verdict`, `reasons` | 그 판단 기록의 판정과 이유 |
| `undo_id`, `undone_at` | 이 변경을 되돌린 마지막 되돌리기(7장) |

뷰는 소유자(Stet의 계정)의 권한으로 읽는다. `before`와 `after`에는 그 테이블의 모든 컬럼이 있다. 누구에게 보여 줄지는 운영자가 정한다.

## 7. 되돌리기

```sql
stet.undo_plan(agent_role text, since timestamptz, until timestamptz DEFAULT NULL, tables text[] DEFAULT NULL)
stet.undo(agent_role text, since timestamptz, until timestamptz DEFAULT NULL, tables text[] DEFAULT NULL, note text DEFAULT NULL)
  -- 둘 다 RETURNS TABLE (target_table text, key text, action text, outcome text, reason text, warning text, change_ids bigint[])
```

**고르기.** `agent_role`이 그 계정이고, `at`이 `since` 이상 `until` 미만이고(`until`이 없으면 지금까지), `tables`가 있으면 `target_table`이 그 안에 있는 묶음이다. `tables`는 설정의 테이블 키다.

**키마다 하나의 결정.** 고른 변경을 테이블과 키로 묶고 남긴 순서로 본다. 그 키에서 고른 첫 변경의 `before`를 B, 마지막 변경의 `after`를 A라 한다. 되돌린다는 것은 그 키의 행을 B로 만드는 것이다.

| 차례로 확인 | 걸리면 |
|---|---|
| 테이블이 없다 | 건너뛴다. `table_gone` |
| 남긴 값의 컬럼 이름들이 지금 테이블의 컬럼들과 다르다 | 건너뛴다. `columns_changed` |
| 고른 변경 가운데 키를 바꾼 `UPDATE`의 줄이 있다(`update` 묶음의 줄인데 `before`나 `after`가 없다) | 건너뛴다. `key_changed`. 옛 키와 새 키의 두 줄을 한 행으로 묶을 근거가 없다. 한쪽만 되돌려지면 행이 사라진다 |
| 고른 변경 사이가 이어지지 않는다(앞 변경의 `after`가 다음 변경의 `before`와 다르다) | 건너뛴다. `interleaved`. 그 사이에 고르지 않은 쪽이 그 행을 바꿨다 |
| 지금 그 키의 행이 A와 다르다(A가 없는데 행이 있다, A가 있는데 행이 없다도 든다) | 건너뛴다. `changed_since`. 그 뒤에 누가 그 행을 바꿨다 |
| B와 A가 같다(둘 다 없는 것도 든다) | 건너뛴다. `no_net_change` |

걸리지 않으면 `action`이 정해진다. B가 없으면 `delete`(에이전트가 만든 행을 지운다), A가 없으면 `insert`(에이전트가 지운 행을 다시 넣는다), 둘 다 있으면 `update`다.

- 비교는 남길 때와 같은 방법으로 만든 글자끼리 한다. 지금 행의 값도 4장의 다섯 설정을 고정한 함수 안에서 만든다.
- 다른 쪽이 바꾼 것인지는 값으로만 안다. 캡처하지 않는 계정(사람, 애플리케이션)의 변경도 그래서 잡힌다. 누가 값을 바꿨다가 정확히 같은 값으로 돌려놓은 것은 구분하지 못한다. 그때는 되돌려도 잃는 것이 없다.
- 고른 범위에 값을 남기지 않은 묶음(`skipped`)이 있으면 `key` 없이 한 줄로 나온다. `outcome`은 `not_captured`이고 `reason`은 그 `skipped`다. 그 문장이 바꾼 행은 되돌려지지 않는다.

**`stet.undo_plan`**은 위의 확인만 하고 아무것도 바꾸지 않는다. 잠그지 않는다. `outcome`은 `would_restore`, `skipped`, `not_captured`다.

**`stet.undo`**는 키마다 다음을 한다.

1. 그 키의 행을 잠그고(`FOR UPDATE`) 위의 확인을 다시 한다.
2. 그 행을 가리키는 다른 행이 있는지 본다(아래의 `referenced`).
3. `action`의 문장을 실행한다. `update`와 `insert`는 B의 값을 준다. 값은 글자를 그 컬럼의 타입으로 읽는다. 생성 컬럼에는 값을 주지 않는다. `GENERATED ALWAYS AS IDENTITY` 컬럼은 `insert`에서만 준다(`OVERRIDING SYSTEM VALUE`). `UPDATE`로는 값을 줄 수 없는 컬럼이다.
4. 그 키의 행을 다시 읽어 B와 비교한다. 같으면 `restored`, 다르면 `restored_differs`다(고객의 트리거나 기본값이 값을 바꿨다. 값을 줄 수 없는 컬럼이 B와 달랐다. `delete`인데 행이 남아 있다).
5. 문장이 서버 에러로 실패하면 그 키만 되돌리고(저장점) `failed`로 적는다. `reason`은 `SQLSTATE <코드>: <메시지>`다.

- **되돌리기는 연쇄를 일으키지 않는다(`referenced`).** 에이전트가 만든 행을 지우면, 그 행을 가리키는 외래 키가 `ON DELETE CASCADE`·`SET NULL`·`SET DEFAULT`일 때 다른 쪽이 그 뒤에 만든 자식 행까지 지워지거나 바뀐다. `update`가 참조되는 컬럼의 값을 바꿀 때 `ON UPDATE`의 연쇄도 같다. 그래서 복구 문장이 없애거나 바꾸는 값을 그런 외래 키로 가리키는 행이 하나라도 있으면 그 키를 건너뛴다. `reason`은 `referenced: <테이블들>`이다. 그 자식 행이 같은 되돌리기에서 먼저 지워지면(에이전트가 부모와 자식을 함께 만들었다) 아래의 "다시 해 보기"에서 된다. 잠근 뒤에 보므로 그 사이에 새 자식이 생기지 않는다(자식을 넣는 쪽이 부모 행의 잠금을 기다린다). 연쇄가 아닌 외래 키(`NO ACTION`, `RESTRICT`)는 서버가 `23503`으로 막는다. 자식 테이블을 읽을 권한이 검토자에게 없으면 그 키는 `failed`(`42501`)다.
- **순서와 다시 해 보기.** 순서가 맞아야 되는 것이 있다. 지워진 부모는 자식보다 먼저 넣어야 하고, 유일 제약의 값을 서로 맞바꾼 두 행은 한쪽을 먼저 돌려야 한다. 순서를 미리 계산하지 않는다. 외래 키 위반(`23503`), 유일 제약 위반(`23505`), `referenced`로 되지 않은 키는 나머지를 처리한 뒤 다시 해 본다. 한 바퀴에서 하나도 더 되지 않으면 그만두고, 남은 것은 그 이유로 `failed`나 `skipped`다.
- **에러를 내지 않는다.** 건너뛴 것과 실패한 것은 결과의 줄로 돌려준다. 검토자는 결과를 보고 커밋하거나 롤백한다. 에러를 내는 것은 인자가 틀렸을 때뿐이다(아래).
- **복구 문장은 부른 세션의 설정으로 돈다.** `search_path`, `TimeZone` 같은 설정을 함수가 바꾸지 않는다. 고객의 트리거가 평소와 같은 환경에서 돌게 하려는 것이다. Stet 자신의 읽기와 비교는 `search_path`와 다섯 설정을 고정한 도우미 함수 안에서 하고, 조립한 복구 문장은 이름을 스키마까지 적고 연산자를 `OPERATOR(pg_catalog.=)`로 적는다.
- **경고(`warning`).** 되돌리기는 하되 검토자가 알아야 하는 것이다.
  - `children_not_captured: <테이블들>`. `insert`로 되돌리는 테이블을 가리키는 외래 키가 `ON DELETE CASCADE`·`SET NULL`·`SET DEFAULT`인데 그 자식 테이블에 캡처 트리거가 없다. 함께 지워진 자식 행은 남지 않았으므로 돌아오지 않는다.
  - `not_selected: <테이블들>`. 같은 문장이 다른 테이블에서도 행을 바꿨는데(연쇄 등) `tables`로 고르지 않아 그 변경은 되돌리지 않는다.
- 같은 것을 두 번 되돌리면 두 번째는 모두 `changed_since`로 건너뛴다. 지금 행이 A가 아니기 때문이다.

**거절하는 경우.** SQLSTATE는 `ST006`이다. `agent_role`이나 `since`가 NULL이다. `until`이 `since`보다 앞이거나 같다. `session_user`가 `agent_role`이다(자기 쓰기를 되돌릴 수 없다).

**기록.** `stet.undo`는 부를 때마다 `stet.undos`에 한 줄(id, 때, `session_user`, 고른 조건, 검토자가 적은 `note`, `restored`·`skipped`·`failed`의 수)을, `stet.undo_items`에 결과의 줄마다 한 줄을 남긴다. 검토자의 트랜잭션 안이므로, 롤백하면 이 기록도 사라진다. 일어나지 않은 되돌리기는 남지 않는다.

**권한.** `stet.undo`와 `stet.undo_plan`은 부른 계정의 권한으로 돈다(`SECURITY INVOKER`). 기록을 읽고 `stet.undos`에 적는 것만 `SECURITY DEFINER` 도우미 함수가 한다. 도우미까지 모든 함수의 `EXECUTE`를 PUBLIC에서 거둔다. 테이블 소유자도 `stet` 스키마의 `USAGE`를 가지므로(4장), 열어 두면 도우미로 남긴 값을 읽을 수 있다. 누가 되돌릴 수 있는지는 운영자가 정한다. 아래는 예시이고, 검토자에게는 도우미 함수의 `EXECUTE`도 줘야 한다. 정확한 문장은 `schema.sql`의 주석과 문서에 적는다.

```sql
GRANT USAGE ON SCHEMA stet TO reviewer;
GRANT SELECT ON stet.history TO reviewer;
GRANT EXECUTE ON FUNCTION stet.undo_plan(text, timestamptz, timestamptz, text[]),
                          stet.undo(text, timestamptz, timestamptz, text[], text) TO reviewer;
```

- 검토자는 되돌릴 테이블에 `SELECT`, `INSERT`, `UPDATE`, `DELETE`가 있어야 한다. 없는 테이블의 키는 `failed`(`42501`)다.
- 도우미 함수의 이름과 나눔은 만드는 쪽이 정한다. 조건은 셋이다. 검토자가 `stet.change_sets`와 `stet.changes`를 직접 읽을 권한 없이 되돌릴 수 있어야 한다. 도우미 함수로 남의 이름(`session_user`가 아닌 계정)의 되돌리기 기록을 만들 수 없어야 한다. `stet.undo_plan`만 받은 계정은 아무것도 바꾸거나 적을 수 없어야 한다.

## 8. 모드와 규칙

- **섀도(규칙 6).** Stet 프로세스의 동작은 바뀌지 않는다. 캡처는 테이블 소유자가 트리거를 설치해서 켜는 것이고, 켜면 에이전트의 쓰기에 시간이 더해진다(2장의 숫자). 결과는 바꾸지 않는다. 캡처가 실패해도 쓰기는 성공하고, 한 문장의 상한(`max_rows`)이 캡처의 시간을 묶는다. 남는 차이는 11장에 적는다.
- **규칙 1.** 캡처는 에이전트의 트랜잭션 안에서 돌지만 모델을 부르지 않는다. 되돌리기도 모델을 부르지 않는다.
- **규칙 3.** `stet.decisions`의 새 컬럼 셋은 줄을 넣을 때만 적는다.
- **규칙 5.** 남긴 값과 `query`, `app`은 에이전트가 쓴 데이터다. 모델에 보내지 않고, 판정에 쓰지 않는다.
- 승인된 쓰기를 실행하는 쪽은 실행 전에 `stet.held_write`를 적는 것만 달라진다.

## 9. 설정, 명령, 시작할 때의 확인

```yaml
undo:
  tables:                  # 되돌리기를 켤 테이블
    contacts:              # tables의 키. 키 컬럼은 거기서 가져온다
    deals:
    public.deal_notes:     # 게이트 대상이 아닌 테이블(연쇄로 바뀌는 자식 등). 키 컬럼을 적는다
      pk: id
  max_rows: 10000          # 한 문장이 이보다 많은 행을 바꾸면 값을 남기지 않는다. 1 이상 1,000,000 이하. 기본 10000
  keep: 30d                # 변경 기록을 두는 기간. 1h 이상. 기본 30d
```

- `undo` 절이 없으면 이 단계의 어떤 것도 돌지 않는다. `schema.sql`의 표와 함수는 만들어진다.
- `tables`의 키에 있는 테이블에 `pk`를 적었는데 그쪽과 다르면 설정 오류다. 없는 키인데 `pk`가 없어도 설정 오류다.

**`stet undo-sql -config stet.yaml [-drop]`.** Stet의 계정으로 붙어 테이블을 확인하고 SQL을 표준 출력에 쓴다. 아무것도 실행하지 않는다.

- 출력은 두 묶음이다. 첫째는 Stet의 계정이 실행할 것(테이블 소유자마다 `GRANT USAGE ON SCHEMA stet`, `GRANT EXECUTE ON FUNCTION stet.capture()`), 둘째는 테이블 소유자가 실행할 것(4장의 트리거)이다. 둘째는 한 트랜잭션이고 `lock_timeout`을 5초로 둔다. 이미 있는 트리거는 지우고 다시 만든다.
- 테이블을 찾지 못하거나, 받지 않는 종류이거나(4장), 키 컬럼이 유일하지 않으면 그 이유를 말하고 0이 아닌 코드로 끝난다. SQL을 출력하지 않는다.
- 캡처하지 않는 자식 테이블(7장의 `children_not_captured`가 될 것)은 주석으로 알린다.
- 트리거를 만드는 동안 그 테이블에 쓰기를 막는 잠금이 잠깐 걸린다. 열린 트랜잭션이 그 테이블에 쓰고 있으면 5초를 기다리다 실패하고, 아무것도 바뀌지 않는다.
- `-drop`은 트리거를 지우는 SQL을 출력한다.

**Stet이 시작할 때**(`undo.tables`가 있으면, 두 모드 모두)

- `stet.capture_settings`를 설정 파일의 `agents.roles`와 `undo.max_rows`로 맞춘다.
- 다음을 경고한다. 시작은 한다. 테이블을 찾지 못했다. 트리거 넷 가운데 없거나 꺼져 있거나 인자가 설정과 다른 것이 있다(`stet undo-sql`을 실행하라고 말한다). 캡처하지 않는 자식 테이블이 있다. 확인하지 못한 것은 "확인하지 못했다"고 경고한다.
- 한 시간마다 `keep`보다 오래된 묶음을 지운다. 한 번에 10,000묶음씩이다. 행의 줄은 연쇄로 지워진다. `stet.undos`와 `stet.undo_items`는 지우지 않는다.

## 10. 수용 기준에 쓰는 끝에서 끝까지의 확인

랩의 벤치마크 스트림(spec-v0.2-benchmark의 V와 X)을 캡처를 켠 데이터베이스에 프록시 없이 보낸다(`none` arm). 모델을 부르지 않으므로 비용이 없다. 그런 다음 에이전트 계정마다 `stet.undo`를 부르고, 캡처한 테이블의 내용을 스트림 전의 덤프와 비교한다.

## 11. 한계

- **테이블 소유자가 설치해야 한다.** 트리거가 없는 테이블은 지금처럼 판단만 받고 되돌릴 수 없다. 스키마를 다시 만들 때 트리거가 빠질 수 있다(트리거를 덤프하지 않는 도구). 시작할 때의 경고로만 안다.
- **에이전트의 쓰기에 시간이 더해진다.** 2장의 숫자다. `statement_timeout`에 가깝게 끝나던 문장은 넘을 수 있다. 그때 문장은 실패한다(시간 초과와 취소는 캡처가 삼키지 않는다). 에이전트 계정이 아닌 세션도 전이 테이블을 만드는 비용은 낸다.
- **기록이 커진다.** 변경 하나에 행 전체가 두 번 든다. 큰 글자나 바이너리 컬럼도 그대로다. 컬럼을 빼는 설정은 없다.
- **한 트랜잭션에서 32번째부터의 캡처는 남지 않는다**(4장). 섀도에서 에이전트가 한 트랜잭션에 쓰기를 많이 넣으면 뒤쪽은 되돌릴 수 없고 표지만 남는다. 에이전트가 세는 값을 스스로 되돌려 이 상한을 피할 수는 있다. 그때 느려지는 것은 서버이고, 같은 일을 `SAVEPOINT`로도 할 수 있다.
- **`max_rows`를 넘는 문장은 되돌릴 수 없다.** 가장 큰 쓰기가 되돌려지지 않는다. enforce에서는 영향 한도가 그런 쓰기를 먼저 보류한다. 섀도에서는 그대로 실행되고 표지만 남는다. `TRUNCATE`도 표지만 남는다.
- **캡처하지 않는 것.** 트리거가 불리지 않는 변경이다. 테이블 소유자나 superuser가 트리거를 끄고 한 쓰기, `session_replication_role = replica`인 세션, 파티션에 직접 쓴 것. 에이전트 계정이 테이블의 소유자면 트리거를 지울 수 있다(시작할 때의 기존 경고).
- **값의 형식.** 값은 타입의 글자 출력이다. 글자로 나갔다 들어오면 달라지는 타입은 그대로 돌아오지 않는다. `money`는 `lc_monetary`에 따라 글자가 달라진다. 확장 타입은 확인하지 않았다.
- **키가 하나인 테이블만.** 설정의 `pk`가 컬럼 하나다. 키가 여러 컬럼인 테이블은 되돌리기를 켤 수 없다.
- **키를 바꾼 `UPDATE`는 되돌리지 않는다**(`key_changed`). 남기기는 한다. 검토자가 `stet.history`의 값을 보고 손으로 고친다.
- **가리키는 행이 있으면 되돌리지 않는다**(`referenced`). 에이전트가 만든 행에 다른 쪽이 자식 행을 붙였으면 그 행은 남는다. 유일 제약의 값을 셋 이상의 행이 돌려 가며 바꾼 것처럼 한 행씩으로는 풀리지 않는 것은 `failed`로 남는다.
- **테이블의 컬럼이 바뀌면** 그 전의 변경은 되돌릴 수 없다(`columns_changed`). 타입만 바뀐 것은 알아채지 못하고, 값이 새 타입으로 읽히지 않으면 `failed`가 된다.
- **되돌리기는 지금의 규칙으로 실행된다.** 복구 문장에서 고객의 트리거, 제약, 행 수준 보안이 돈다. 트리거가 값을 바꾸면 `restored_differs`로 알린다. 트리거가 다른 테이블에 한 일은 보지 않는다.
- **에이전트의 쓰기가 일으킨 다른 일은 되돌리지 않는다.** 고객의 트리거가 캡처하지 않는 테이블에 쓴 것, 시퀀스, 알림이다.
- **잇기는 근사다.** 하나로 정해질 때만 잇는다(5장). 같은 행을 빠르게 되풀이해 고치는 쓰기와, 한 번에 보낸 같은 글자의 `INSERT`들은 이어지지 않는다. 섀도에서 더 흔하다. enforce에서는 문장 사이에 판단하는 시간이 든다. 틀리게 이어질 수 있는 경우가 하나 남는다. 판단 기록을 넣지 못한 쓰기가, 같은 연결에서 같은 글자로 10ms 안에 앞서 실행됐고 그 변경이 남지 않은(롤백됐거나 0행이었던) 쓰기의 판단 기록에 이어지는 것이다. Stet과 서버 사이에 연결을 나눠 쓰는 풀러(PgBouncer의 트랜잭션 모드)가 있으면 pid가 맞지 않아 이어지지 않는다.
- **`stet.held_write`는 승인된 문장이 스스로 바꿀 수 있다.** 그 문장은 에이전트가 쓴 것이고 Stet의 연결에서 돈다. 다른 보류 줄의 id를 적으면 그 변경은 다른 보류 줄에 이어져 보인다. 되돌리기의 정확성과는 상관없다.
- **스키마의 잠금.** `stet.change_sets`, `stet.changes`, `stet.capture_settings`에는 캡처한 에이전트의 트랜잭션이 끝날 때까지 약한 잠금이 걸린다. `schema.sql`이 이 세 표에 강한 잠금을 잡는 문장을 실행하면, 열린 에이전트 트랜잭션을 기다리는 동안 그 뒤의 모든 캡처가 줄을 선다. 그동안의 캡처는 에이전트의 `lock_timeout`에 걸려 `capture_failed`가 되거나, 그 설정이 없으면 쓰기가 기다린다. 그래서 `schema.sql`은 이 세 표가 이미 있으면 그 표에 잠금을 잡는 문장을 실행하지 않아야 한다. 트리거와 인덱스는 없는지 먼저 보고 만든다(`CREATE INDEX IF NOT EXISTS`도 있는지 보기 전에 잠금을 잡는다). 나중에 컬럼을 더할 때는 이 한계를 다시 본다.
- **설정은 마지막에 시작한 Stet의 것이다.** 한 데이터베이스에 `agents.roles`나 `undo.max_rows`가 다른 Stet이 여럿이면 `stet.capture_settings`는 마지막에 시작한 쪽의 값이다.
- **되돌리기 기록은 검토자가 도우미 함수를 직접 불러 자기 이름으로 지어낼 수 있다.** 남의 이름으로는 못 한다.
- 되돌리기를 다시 되돌릴 수 없다. 되돌리기의 쓰기는 검토자의 것이어서 남지 않는다(`roles`가 NULL일 때만 남는다).

## 12. 패키지별 변경

| 패키지 | 바뀌는 것 |
|---|---|
| `internal/store` | `schema.sql`: 4장의 표 셋과 `stet.capture()`, 기록을 지키는 트리거, 5장의 컬럼과 인덱스, 6장의 뷰, 7장의 함수들과 `stet.undos`·`stet.undo_items`, 권한 거두기. `Decision`의 새 필드 셋과 삽입. 설정 맞추기, 오래된 묶음 지우기 |
| `internal/proxy` | 지켜보는 세션에서 BackendKeyData의 pid를 읽어 `SessionInfo`로 게이트에 넘긴다. 그 밖의 동작은 바뀌지 않는다 |
| `internal/gate` | 판단 기록에 pid와, 들어온 때와 결과를 받은 때부터 흐른 시간을 넘긴다 |
| `internal/apply` | 승인된 문장 앞에 `stet.held_write`를 적는다 |
| `internal/config` | `undo` 절 |
| `cmd/stet` | `undo-sql` 명령. 시작할 때 설정 맞추기, 9장의 경고, 지우기 루프와 멈추기 |
| `internal/lab`, `cmd/stet-lab` | 10장의 확인을 돌리는 데 필요한 만큼 |
| 문서 | `stet.example.yaml`, docs/lab.md, spec-v0.1 8장의 해당 문장 |

판정 규칙과 모델 인터페이스는 바뀌지 않는다(규칙 7). 프록시의 지연 예산(2ms)을 그대로 확인한다.

## 13. 수용 기준

- [ ] 에이전트 계정이 직접 쓴 `INSERT`·`UPDATE`·`DELETE`, 뷰를 거친 쓰기, `SECURITY DEFINER` 함수 안의 쓰기, `ON CONFLICT DO UPDATE`, 연쇄 삭제된 자식 행이 모두 이전·이후 값과 함께 남는다. 롤백된 쓰기와 0행 쓰기는 남지 않는다.
- [ ] 에이전트 계정이 아닌 세션의 쓰기는 남지 않는다. `roles`가 NULL이면 남는다. Stet의 계정이 `SET LOCAL ROLE`로 실행한 쓰기는 그 에이전트의 것으로, `held_write`와 함께 남는다. 에이전트 세션이 `stet.held_write`를 적어도 남지 않는다.
- [ ] 캡처가 실패해도 에이전트의 쓰기는 성공하고 `capture_failed` 묶음이 남는다. 설정을 읽다가 실패해도 같다. 클라이언트는 `client_min_messages`가 `debug5`여도 아무 메시지도 받지 않는다. `max_rows`를 넘는 문장과 `TRUNCATE`는 표지만 남는다.
- [ ] 한 트랜잭션에서 행을 바꾸는 쓰기 40개를 캡처하면 31번까지 값이 남고, 32번째에 `too_many_statements` 묶음이 남고, 그 뒤는 남지 않는다. 그 트랜잭션이 만든 하위 트랜잭션은 32개를 넘지 않는다. 캡처하지 않는 세션은 하위 트랜잭션을 만들지 않는다.
- [ ] 남긴 값으로 행을 다시 만들면 원래 행과 같다. 정수, `numeric`, 실수(`NaN`, 무한대, `0.30000000000000004`), 글자(줄바꿈, 따옴표, 역슬래시), `timestamptz`(무한대 포함), `date`, `interval`, `uuid`, `bytea`, 배열(NULL 원소, 다차원), `jsonb`와 `json`(JSON `null`과 SQL `NULL`의 구분, 중복 키), NULL에서 확인한다. 세션의 `TimeZone`, `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output`이 달라도 같은 글자가 남는다.
- [ ] 키가 바뀐 `UPDATE`는 옛 키와 새 키의 두 줄로 남는다. 되돌리기는 두 키를 `key_changed`로 건너뛰고 행은 그대로다.
- [ ] 에이전트 계정은 기록을 읽거나 고칠 수 없다. 두 표의 `UPDATE`와 `TRUNCATE`는 `ST004`다. 트리거를 만드는 계정은 권한을 받기 전에는 만들 수 없다.
- [ ] 프록시를 지난 쓰기의 변경이 그 판단 기록에 이어진다. 단순 프로토콜과 확장 프로토콜, 명시적 트랜잭션 안의 쓰기, 연쇄로 바뀐 자식에서 확인한다. 함수를 거친 쓰기는 이어지지 않는다. 한 Sync 아래 같은 글자로 서로 다른 행을 고친 쓰기 둘은 각자의 판단 기록에 이어진다. 같은 행을 고친 둘은 맞게 이어지거나 후보 2로 나온다. 다른 판단 기록에 이어지지 않는다.
- [ ] `stet.undo`가 에이전트의 `INSERT`를 지우고, `DELETE`를 다시 넣고, `UPDATE`를 이전 값으로 돌린다. 한 키를 여러 번 바꿨으면 처음의 값으로 돌아간다. `GENERATED ALWAYS AS IDENTITY` 컬럼과 생성 컬럼이 있는 테이블에서도 셋 다 된다.
- [ ] 그 뒤에 다른 계정이 바꾼 행은 `changed_since`로, 고른 변경 사이에 다른 계정이 바꾼 행은 `interleaved`로 건너뛰고, 그 행은 그대로다. 되돌리는 동안 다른 연결이 그 행을 바꾸고 있어도 덮어쓰지 않는다.
- [ ] 지워진 부모와 자식을 함께 되돌리면 둘 다 돌아온다(외래 키 순서). 자식 테이블에 트리거가 없으면 부모만 돌아오고 `children_not_captured`가 나온다. `tables`로 부모만 고르면 부모만 돌아오고 `not_selected`가 나온다. 유일 제약의 값을 맞바꾼 두 행이 둘 다 돌아온다.
- [ ] 에이전트가 만든 행에 다른 계정이 연쇄 외래 키의 자식 행을 붙였으면 그 행은 `referenced`로 건너뛰고, 부모도 자식도 그대로다. 에이전트가 부모와 자식을 함께 만들었으면 둘 다 지워진다.
- [ ] 복구 문장이 실패한 키는 `failed`이고 다른 키는 되돌려진다. 고객의 트리거가 값을 바꾸면 `restored_differs`다. 고객의 트리거 함수가 스키마 없는 이름을 써도 부른 세션의 `search_path`로 돈다.
- [ ] `stet.undo_plan`은 아무것도 바꾸지 않고, 바로 이어 부른 `stet.undo`와 같은 키와 `action`을 말한다. 검토자가 롤백하면 테이블도 `stet.undos`도 그대로다. 두 번 되돌리면 두 번째는 아무것도 바꾸지 않는다.
- [ ] 권한을 받지 않은 계정은 뷰도 함수도 쓰지 못한다. 테이블에 쓰기 권한이 없는 검토자의 키는 `failed`(`42501`)다. 7장의 거절이 `ST006`으로 나온다. 부른 세션의 `search_path`에 함수나 연산자를 가로채는 객체가 있어도 Stet 자신의 비교가 그것을 쓰지 않는다.
- [ ] `stet undo-sql`의 출력을 실행하면 트리거 넷이 생기고, `-drop`의 출력을 실행하면 없어진다. 받지 않는 테이블과 유일하지 않은 키는 거절한다. 9장의 경고가 나온다.
- [ ] `keep`보다 오래된 묶음이 지워진다. 열린 트랜잭션이 캡처를 하고 있는 동안 `schema.sql`을 다시 실행해도(`stet.schema_version`의 값을 바꿔 다시 돌게 한다) 기다리지 않고 끝난다.
- [ ] 10장의 확인: 스트림을 보낸 뒤 되돌리면 캡처한 테이블의 내용이 스트림 전과 같다. 같지 않은 행이 있으면 그 수와 이유를 문서에 적는다. 벤치마크에서 Stet이 막지 못한 경로(`s5.call`, `s5.view`)의 쓰기가 되돌려지는지도 적는다.
- [ ] 오버헤드를 재서 문서에 적는다. 키로 한 행을 고치는 쓰기에 캡처가 더하는 시간이 p95로 1ms를 넘지 않는다.
- [ ] 프록시의 2ms 지연 예산을 지킨다. `go test -race ./...`가 통과한다.

## 14. 정한 것과 남긴 것

**정한 것**

- 이전 값은 DB의 트리거가 남긴다. 테이블 소유자가 설치한다. Stet은 SQL을 출력만 한다.
- 캡처는 테이블마다 켜는 선택이다. 켜지 않은 테이블은 지금과 같다.
- 캡처는 에이전트의 쓰기를 실패시키지 않는다.
- 되돌리기는 검토자의 트랜잭션 안에서, 검토자의 권한으로 한다. 계정, 시간 범위, 테이블로 고른다.
- 다른 쪽이 바꾼 행은 되돌리지 않고 알린다. 판단은 값의 비교로만 한다.
- 변경은 하나로 정해질 때만 판단 기록에 잇는다.

**작성자가 정한 것**(검토할 때 봐 주기를 바라는 것)

- 한 문장의 상한(`max_rows`, 기본 10,000)을 둔다. 넘으면 값을 남기지 않는다. 틀리면 큰 쓰기를 되돌리지 못하는 것이 대가다.
- `TRUNCATE`용 트리거를 하나 더 둬서 테이블마다 넷이다. 표지만 남긴다.
- 게이트 대상이 아닌 테이블도 캡처할 수 있게 한다(`undo.tables`의 `pk`). 연쇄로 지워지는 자식 테이블 때문이다.
- 보존 기간(`keep`, 기본 30일)을 이 단계에 넣는다. 행 전체를 남기는 표가 끝없이 자라기 때문이다.
- 승인으로 실행된 쓰기도 그 에이전트의 것으로 고른다. 사람이 승인한 쓰기도 함께 되돌려진다.
- 판단 기록에 잇는 것을 이 단계에 넣는다. 프록시가 바뀌는 것은 pid를 읽는 것 하나다.
- 한 트랜잭션에서 값을 남기는 것은 캡처 31번까지다. 서버 전체가 느려지는 것을 피하려는 것이다. 대가는 섀도의 큰 트랜잭션에서 뒤쪽 쓰기를 되돌리지 못하는 것이다.
- 되돌리기는 연쇄를 일으키지 않는다. 가리키는 행이 있으면 건너뛴다. 대가는 그런 행을 손으로 정리해야 하는 것이다.
- 키를 바꾼 `UPDATE`는 남기기만 하고 되돌리지 않는다.

**남긴 것**

- 판단 기록의 id, 세션, 업무(`on_behalf_of`)로 고르기. 잇기가 근사인 동안에는 값을 남긴 표의 것(계정, 시간, 테이블)으로만 고른다.
- 같은 연결의 문장 순서로 잇기를 정확하게 만드는 것.
- "작고 되돌릴 수 있으면 통과시킨다"는 판정 정책(D31).
- 되돌리기를 다시 되돌리기, 검토 화면, 되돌리기의 승인 절차.
- 키가 여러 컬럼인 테이블, 파티션 테이블, 남기지 않을 컬럼의 설정.
- 키를 바꾼 `UPDATE`의 되돌리기, 가리키는 행까지 함께 되돌리기.
- `stet.undos`, `stet.undo_items`의 보존 기간.
