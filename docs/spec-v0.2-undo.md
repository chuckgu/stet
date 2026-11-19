# Stet v0.2 4단계 스펙: 되돌리기

상태: 구현 (2026-11-19). 근거는 concept-v0.2 7장의 2단("되돌릴 수 있게 하기")과 9장의 4번, D31, docs/benchmark-hoop.md의 R3다.

## 1. 목표와 비목표

Stet은 에이전트의 쓰기를 실행 전에 판단한다. 판단이 틀렸거나 판단이 보지 못한 쓰기는 그대로 남는다. 벤치마크에서 Stet은 결함 400건 가운데 11건을 놓쳤고, 함수와 뷰를 거친 쓰기(`s5.call`, `s5.view`)는 보지 못했다. 이 단계는 에이전트 계정이 바꾼 행의 이전 값을 남기고, 사람이 그것을 골라 되돌리는 가장 짧은 길을 만든다.

**목표**

- 에이전트 계정이 바꾼 행마다 바뀌기 전과 뒤의 값을 남긴다. 쓰기와 같은 트랜잭션 안에서 DB가 남긴다. 롤백된 쓰기는 남지 않는다.
- 문장이 어떤 길로 왔든 남긴다. Stet이 판단한 쓰기, 함수와 뷰를 거친 쓰기, 연쇄로 바뀐 행, Stet을 거치지 않고 붙은 에이전트 계정의 쓰기, 승인으로 실행된 쓰기가 모두 든다. 승인된 문장은 Stet의 연결에서 돌므로 남긴 뒤의 기록을 건드릴 수 있다. 그것은 11장의 한계다.
- 검토자가 계정, 시간 범위, 테이블로 골라 되돌린다(`stet.undo`). 되돌리기 전에 무엇이 어떻게 될지 본다(`stet.undo_plan`).
- 되돌리기는 다른 쪽의 변경을 지우지 않는다. 그 뒤에 누가 바꾼 행은 건너뛰고 알린다.
- 남긴 변경을 판단 기록에 잇는다. 판단 기록이 없는 변경은 Stet이 판단하지 않은 쓰기다.

**비목표**

- 검토 화면, 되돌리기의 자동 실행, 되돌리기를 다시 되돌리기.
- 업무(`on_behalf_of`)나 세션으로 고르기, 판단 기록의 id로 고르기(14장).
- 읽어 간 것. 되돌릴 수 없다(concept 7장).
- DB 밖으로 나간 것(시퀀스 값, `NOTIFY`, 서버 밖을 부르는 함수)과 캡처하지 않는 테이블.
- "되돌릴 수 있으면 덜 막는다"는 판정 정책(D31). 이 단계는 재료만 만든다.

**실패로 볼 것.** 되돌리기가 다른 쪽이 바꾼 값을 조용히 덮어쓰는 것. 남긴 이전 값이 실제 이전 값과 다른 것. 캡처 때문에 에이전트의 쓰기가 실패하는 것(4장에 적은 예외 말고). 변경이 다른 판단 기록에 이어지는 것.

## 2. 접근

**쓰기 전 값을 누가 잡는가.** DB가 잡는다(2026-11-18에 정함).

| | 동작 | 버린 이유 또는 고른 이유 |
|---|---|---|
| **T. DB가 잡는다** | 테이블에 트리거를 달아, 바뀐 행의 이전·이후 값을 쓰기와 같은 트랜잭션에서 Stet의 표에 남긴다 | 고른 것. 정확하다. 롤백된 쓰기는 남지 않고, DB가 만든 키의 `INSERT`, 다중 행, 연쇄, 함수·뷰 경유를 모두 잡는다. 대가는 테이블 소유자가 트리거를 한 번 설치하는 것과, 에이전트의 쓰기마다 기록이 같은 트랜잭션에 더해지는 것이다 |
| P. 프록시가 미리 읽는다 | 쓰기를 보내기 직전에 걸리는 행을 읽어 저장한다 | 읽기와 실행 사이에 바뀐 행은 틀리게 남는다. DB가 만든 키의 `INSERT`는 되돌릴 수 없다(프록시는 응답의 행을 읽지 않는다). 함수·뷰·연쇄는 못 본다 |
| W. 복제 로그를 읽는다 | 논리 복제 슬롯으로 변경을 읽는다 | 서버 설정과 복제 권한이 필요하다. Stet이 멈추면 로그가 쌓인다. 관리형 Postgres에서 설정 변경과 재시작이 든다 |

**트리거의 종류.** 문장 단위 트리거와 전이 테이블(`REFERENCING OLD TABLE / NEW TABLE`)을 쓴다. 행 단위 트리거는 행마다 함수를 부른다. 문장 단위는 문장에 한 번 부르고, 바뀐 행 수를 먼저 세어 상한(4장)을 적용할 수 있다.

**행의 값을 남기는 형식.** 컬럼마다 그 값을 타입의 출력 함수로 만든 글자를 담은 JSON이다(4장). `to_jsonb(행)`은 쓰지 않는다. `jsonb` 컬럼의 JSON `null`과 SQL `NULL`을 구분하지 못하고, `json` 컬럼의 글자가 바뀐다(아래의 확인). `::text`도 쓰지 않는다. 타입에서 `text`로 가는 캐스트 함수를 테이블 소유자가 만들 수 있고, 그 함수가 캡처 안에서 Stet의 계정으로 돌기 때문이다.

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

**만든 함수로 잰 것**(PostgreSQL 14.18, 같은 기기. 재는 방법은 docs/lab.md의 "되돌리기"). 하나를 빼면 지금의 함수로 잰 것이다.

- 키로 한 행을 고치는 `UPDATE`(14개 컬럼, 1,000행의 테이블, 세 종류를 번갈아 2,000번씩, 세 번 쟀다). 트리거가 없을 때 평균 89~90µs, p95 96~97µs. 트리거가 있고 캡처하지 않는 세션일 때 평균 100~101µs, p95 107~109µs. 캡처할 때 평균 379~384µs, p95 503~514µs. 캡처가 더하는 시간은 p95로 0.41~0.42ms다. 클라이언트를 `go test -race`로 돌렸으므로 위의 시험용 함수의 숫자와 바로 견주지 않는다.
- `stet.undo_plan`은 한 테이블의 5,000키에 약 0.55초, `stet.undo`는 한 문장이 바꾼 5,000키를 모두 되돌리는 데 2.75~2.86초였다. 문장 1,600개가 4,000키를 키마다 여덟 번씩 바꾼 것(변경 32,000건)을 되돌리는 데는 2.6초였고, 그 줄들을 다시 기록하는 데만은 0.4초였다.
- `stet.history`에서 판단 기록 50,100건이 있는 백엔드의 최근 묶음 100개를 읽는 데 0.43~0.51ms였다. 이것만은 뷰가 `stet.undone_changes`를 붙이기(6장의 `undo_id`, `undone_at`) 전에 잰 것이다.
- 오래된 묶음 지우기(9장)는 줄이 하나씩인 묶음 10,000개에 약 20ms였다.

## 3. 흐름

1. 운영자가 설정에 되돌리기를 켤 테이블을 적고(9장) Stet을 한 번 시작한다. Stet이 `stet` 스키마를 만든다. `stet undo-sql`이 설치할 SQL을 출력하고, 테이블 소유자가 그것을 실행한다(4장). `stet undo-sql -grant <검토자>`가 검토자에게 줄 권한을 출력하고, Stet의 계정이 그것을 실행한다(7장).
2. 에이전트 계정의 쓰기가 그 테이블의 행을 바꾸면 트리거가 변경 묶음 하나와 행마다의 이전·이후 값을 남긴다. 쓰기와 함께 커밋되거나 함께 버려진다(4장).
3. Stet은 판단 기록에 백엔드 pid와 시간 창을 적는다(5장).
4. 검토자가 `stet.history`에서 변경과 그 판단을 본다(6장).
5. 검토자가 READ COMMITTED 트랜잭션에서 `stet.undo_plan(계정, 언제부터, ...)`으로 무엇이 되돌려지고 무엇이 건너뛰어질지 본다(7장).
6. 검토자가 같은 트랜잭션에서 `stet.undo(...)`를 부르고, 결과를 본 뒤 커밋하거나 롤백한다(7장).

캡처(2)와 되돌리기(5, 6)는 Stet 프로세스가 꺼져 있어도 된다. 판단 기록에 잇는 것(3), 캡처 설정 맞추기와 보존 기간(9장)만 Stet 프로세스가 한다.

## 4. 캡처

**설치.** 테이블마다 트리거 넷이다. `stet undo-sql`(9장)이 출력하고 테이블 소유자가 실행한다. Stet은 고객의 테이블에 아무것도 설치하지 않는다. `CREATE OR REPLACE TRIGGER`를 쓰므로 PostgreSQL 14 이상이 필요하다.

```sql
CREATE OR REPLACE TRIGGER stet_capture_ins AFTER INSERT ON public.deals
  REFERENCING NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_upd AFTER UPDATE ON public.deals
  REFERENCING OLD TABLE AS stet_old NEW TABLE AS stet_new
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_del AFTER DELETE ON public.deals
  REFERENCING OLD TABLE AS stet_old
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
CREATE OR REPLACE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deals
  FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
```

- 인자는 설정의 테이블 키와 키 컬럼이다. 판단 기록의 `target_table`과 같은 이름으로 남기려는 것이다. 키 컬럼의 이름은 묶음의 `key_column`에 남고, 되돌리기는 행을 이 컬럼으로 찾는다. 그래서 트리거가 지워진 테이블의 변경도 되돌릴 수 있다.
- 전이 테이블의 이름은 `stet_old`와 `stet_new`다. 세션의 임시 릴레이션과 우연히 겹치지 않게 긴 이름을 쓴다(아래의 "설치의 확인").
- 함수와 표는 `schema.sql`이 `stet` 스키마에 만든다. 소유자는 Stet의 계정이다. 트리거를 만들 계정에는 `stet` 스키마의 `USAGE`와 `stet.capture()`의 `EXECUTE`를 줘야 한다. `EXECUTE`는 PUBLIC에서 거둔다. 트리거 함수는 직접 부를 수 없으므로 이 권한으로 할 수 있는 일은 트리거를 만드는 것뿐이다.
- 받는 테이블은 보통 테이블뿐이다. 파티션 테이블과 그 파티션, 상속 자식인 테이블과 상속 자식이 있는 테이블, 임시 테이블, 시스템 테이블은 받지 않는다(11장의 "받지 않는 테이블"). 상속 자식인 테이블은 부모를 거친 쓰기가 자식의 문장 트리거를 부르지 않고, 상속 자식이 있는 테이블은 그 테이블에 쓴 문장이 자식의 행도 바꾸는데 그 테이블의 트리거가 그것을 남기지 않기 때문이다. 한 테이블을 두 키로 적은 것도 받지 않는다. 트리거는 키를 하나만 넘긴다.
- 키 컬럼은 그 테이블의 기본 키이거나, `NOT NULL`이고 그 컬럼 하나에만 걸린 유일 제약이나 유일 인덱스가 있는 컬럼이어야 한다. 부분 인덱스, 식 인덱스, 미룰 수 있는(`DEFERRABLE`) 제약은 안 된다. 미룰 수 있는 기본 키도 안 된다. 행을 키로 가리키는데, 확인을 미루면 한 문장이 한 키의 행 둘을 남길 수 있기 때문이다.

**저장.**

```sql
-- 문장 하나가 한 테이블에 한 일
CREATE TABLE IF NOT EXISTS stet.change_sets (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at            timestamptz NOT NULL,          -- 남긴 때. clock_timestamp()
  stmt_at       timestamptz NOT NULL,          -- 서버가 그 문장의 메시지를 받은 때. statement_timestamp()
  txid          bigint,                        -- txid_current(). 묶음을 쓰는 INSERT가 어차피 트랜잭션 id를 받는다
  pid           integer NOT NULL,              -- pg_backend_pid()
  login_role    text NOT NULL,                 -- session_user
  agent_role    text NOT NULL,                 -- 이 쓰기의 주인으로 보는 계정 (아래)
  app           text,                          -- application_name. 스스로 적는 값
  held_write    uuid,                          -- 승인으로 실행된 쓰기면 그 보류 줄의 id
  target_table  text NOT NULL,                 -- 설정의 테이블 키 (트리거의 첫 인자)
  key_column    text NOT NULL,                 -- 키 컬럼의 이름 (트리거의 둘째 인자). 되돌리기가 행을 이 컬럼으로 찾는다
  relid         oid NOT NULL,
  op            text NOT NULL CHECK (op IN ('insert','update','delete','truncate')),
  query         text,                          -- current_query(). 클라이언트가 보낸 문장
  rows          bigint,                        -- 그 문장이 이 테이블에서 바꾼 행 수. truncate는 NULL
  skipped       text CHECK (skipped IN ('too_many_rows','too_many_statements','truncate','capture_failed')),  -- 값을 남기지 않은 이유
  detail        text                           -- capture_failed의 SQLSTATE와 메시지. 실패한 32번째 캡처의 too_many_statements에도
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
  roles     text[],                            -- 캡처할 계정. NULL이면 모든 세션. Stet의 계정이 접속한 세션은 거르지 않는다
  max_rows  bigint NOT NULL                    -- 한 문장이 이보다 많은 행을 바꾸면 값을 남기지 않는다
);
```

인덱스는 `change_sets (agent_role, at)`, `change_sets (pid, stmt_at)`, `change_sets (txid)`, `changes (set_id)`다. 세 표의 인덱스와 트리거는 없을 때만 만든다(11장의 잠금).

**`stet.capture()`가 하는 일.** `SECURITY DEFINER`이고 함수 안에서만 일곱 설정을 고정한다. `search_path`는 `pg_catalog, pg_temp`다. 값의 글자가 세션 설정에 따라 달라지지 않게 `DateStyle`을 `ISO, MDY`, `IntervalStyle`을 `postgres`, `extra_float_digits`를 `1`, `TimeZone`을 `UTC`, `bytea_output`을 `hex`로 둔다(아래에서 "다섯 설정"). `client_min_messages`는 `warning`이다. 함수가 남기는 로그가 클라이언트로 가지 않게 하려는 것이다.

1. `stet.capture_settings`를 읽는다. 줄이 없으면 모든 세션을 캡처하고 상한은 10,000이다.
2. 쓰기의 주인(`agent_role`)을 정한다. 접속 계정(`session_user`)이 함수의 소유자(Stet의 계정)면 그때의 역할(`current_setting('role')`, `none`이면 접속 계정)이고, 아니면 접속 계정이다. Stet의 계정이 `SET LOCAL ROLE`로 에이전트가 되어 실행하는 승인된 쓰기가 그 에이전트의 것으로 남는다.
3. `roles`가 있고 `agent_role`이 거기에 없으면 아무것도 하지 않고 끝낸다. 접속 계정이 Stet의 계정인 세션은 `roles`로 거르지 않는다. 승인된 문장은 그 세션에서 에이전트의 역할로 도는데, 그 문장이 부른 함수는 역할을 `none`으로 돌려 쓰고 다시 맞춰 둘 수 있다. `roles`로 거르면 그 쓰기는 묶음도 표지도 없이 커밋된다. 그래서 Stet의 세션이 캡처하는 테이블에 쓴 것은 모두 남는다. 그때의 역할로, 역할이 `none`이면 Stet의 계정으로 남는다. Stet 자신은 승인된 쓰기 말고는 고객의 테이블에 쓰지 않는다. 그다음 한 트랜잭션의 상한을 보고 설치를 확인한다(둘 다 아래).
4. `TRUNCATE`면 묶음 하나를 `skipped = 'truncate'`로 남기고 끝낸다.
5. 바뀐 행을 센다. 0이면 아무것도 남기지 않는다. `max_rows`를 넘으면 묶음 하나를 `skipped = 'too_many_rows'`와 행 수로 남기고 끝낸다.
6. 묶음 하나와 행마다 한 줄을 남긴다.
   - 행의 값은 `jsonb_object(컬럼 이름의 배열, 값의 글자의 배열)`이다. 컬럼은 그때의 `pg_attribute`에서 읽는다(지워진 컬럼은 뺀다). 값은 모두 JSON 문자열이거나 `null`이다.
   - 값의 글자는 타입의 출력 함수로 만든다(`format('%s', 값)`). NULL은 `num_nulls`로 가려 `null`로 둔다(복합 타입의 `IS NULL`은 모든 필드가 NULL인 값에도 참이다). `::text`로 만들지 않는다. 그 타입에서 `text`로 가는 캐스트가 있으면 `::text`는 그 함수를 부르는데, 캐스트는 타입의 소유자가 만든 함수일 수 있고 여기서는 Stet의 계정으로 돈다. 출력 함수는 C로만 만들 수 있다(superuser). 그래서 글자가 `::text`와 다른 타입이 있다. `boolean`은 `t`와 `f`이고, `char(n)`은 뒤의 공백을 지키고, `inet`은 `/32`를 붙이지 않는다. 되돌리기는 이 글자를 같은 타입의 입력 함수로 읽는다(7장).
   - `INSERT`는 `after`만, `DELETE`는 `before`만 있다.
   - `UPDATE`는 이전 행들과 이후 행들을 **키로** 묶는다(`FULL JOIN`). 키가 그대로면 한 줄에 `before`와 `after`가 있다. 키가 바뀐 행은 두 줄이 된다. 옛 키의 줄은 `before`만, 새 키의 줄은 `after`만 있다. 그래서 줄은 언제나 "그 키의 행이 문장 전에는 무엇이었고 뒤에는 무엇인가"다. 키를 바꾼 `UPDATE`의 줄은 되돌리지 않는다(7장의 `key_changed`).
   - 한 문장이 한 행을 여러 번 바꿀 수 있다. 문장이 지운 행들을 한 자식 행의 외래 키 여럿이 가리키면(`owner_id`와 `created_by`가 모두 `users`를 `ON DELETE SET NULL`로 가리킨다) 연쇄가 그 행을 외래 키마다 한 번씩 고치고, 전이 테이블에는 그 행의 판마다 한 줄이 있다. 그 키의 줄은 그래도 하나다. `before`는 문장 전의 행(첫 이전 값), `after`는 문장 뒤의 행(마지막 이후 값)이고, 사이의 판은 남기지 않는다. 판들의 키가 같을 때의 이야기다(키가 판마다 바뀌는 것은 이 항목의 끝). 한 키의 이전 값들에서 이후 값들을 하나씩 지우고 이후 값들에서 이전 값들을 하나씩 지워 남는 것이 그 둘이다. 바뀌었다가 같은 값으로 돌아온 키는 남는 것이 없어 줄이 없고, 모든 키가 그러면 묶음도 남기지 않는다. 한쪽에 둘 이상이 남으면 한 행의 판들이 아니다(키 컬럼이 유일하지 않다). 그때는 값을 남기지 않고 `capture_failed` 표지(`21000`)를 남긴다. 한 행만 바꾼 문장은 이 길을 지나지 않는다. 한 문장 안에서 키가 판마다 바뀌는 사슬(키 1의 행이 2가 되고 다시 3이 된다)도 이 길을 지나지 않는다. 어느 키도 한쪽에 두 번 나오지 않기 때문이다. 보통 길대로 키 1의 줄은 `before`만, 키 3의 줄은 `after`만 있고, 사이의 판은 가운데 키 2의 줄에 `before`와 `after`로 남는다. 키를 바꾼 `UPDATE`의 묶음이므로 되돌리기는 그 묶음의 키를 모두 `key_changed`로 건너뛴다(7장).
   - 한 문장이 한 행을 고친 뒤 지울 수도 있다. 그 행을 가리키는 외래 키 하나는 `ON DELETE SET NULL`이고 다른 하나는 `ON DELETE CASCADE`인데, 같은 문장이 두 부모를 다 지우는 경우다. 문장 단위 트리거는 쓰기의 종류마다 따로 불리므로, 그 행은 연쇄의 `UPDATE`가 남긴 `update` 묶음과 `DELETE`가 남긴 `delete` 묶음 둘에 남는다. 두 묶음이 남은 순서에 따라 되돌리기는 그 키의 변경이 이어지지 않는다고 읽고 `interleaved`로 건너뛸 수 있다(11장).
   - 묶음의 `rows`는 남긴 줄로 센다. `INSERT`와 `UPDATE`는 `after`가 있는 줄의 수, `DELETE`는 줄의 수다. 여러 번 바뀐 행도 한 번이고, 줄이 없는 키는 세지 않는다. 값을 남기지 않은 묶음의 `rows`는 전이 테이블의 줄 수다(`TRUNCATE`는 NULL). 5의 상한(`max_rows`)도 전이 테이블의 줄 수로 본다. 세 번 바뀐 행은 셋으로 센다.
7. 접속 계정이 Stet의 계정이면 `current_setting('stet.held_write', true)`를 `held_write`로 남긴다. uuid의 모양이 아니면 NULL로 남긴다(11장). 승인된 쓰기를 실행하는 쪽(`internal/apply`)이 문장 앞에 `set_config('stet.held_write', <id>, true)`를 둔다. 다른 계정의 세션에서는 이 설정을 읽지 않는다. 에이전트가 스스로 적을 수 있는 값이기 때문이다.

2와 7은 1보다 먼저 한다. 설정을 읽다가 실패해도 표지에 맞는 주인과 보류 줄이 남게 하려는 것이다.

**설치의 확인.** 캡처하는 세션에서(3 뒤) 함수는 전이 테이블을 읽기 전에 자기가 어떻게 불렸는지 본다. 다음 가운데 하나면 에러이고, 아래의 블록이 그것을 `capture_failed` 표지로 남긴다. 0행 문장에서도 남는다.

- `AFTER ... FOR EACH STATEMENT` 트리거가 아니다(`39P01`).
- 전이 테이블의 이름이 위의 SQL과 다르다. `INSERT`는 `NEW TABLE AS stet_new`, `DELETE`는 `OLD TABLE AS stet_old`, `UPDATE`는 둘 다다(`39P01`).
- 인자가 정확히 둘이 아니거나, 둘째가 그 테이블의 컬럼이 아니다(`22023`). 컬럼이 아닌 이름은 행을 받는 함수로 읽히기 때문이다.
- 세션의 임시 스키마에 `stet_old`나 `stet_new`라는 릴레이션이 있다(`42P07`). 그 이름의 전이 테이블이 없는 트리거에서는 그 이름이 세션의 임시 릴레이션으로 읽히고, 세션은 그런 뷰로 자기 코드를 Stet의 계정으로 돌리고 없던 행을 남기게 할 수 있다. 전이 테이블의 이름 확인은 지금의 카탈로그를 읽지만 PostgreSQL은 문장이 시작할 때 읽어 둔 트리거를 부른다. 테이블 소유자는 문장 도중에 불릴 트리거를 바꿔 위의 확인을 지날 수 있으므로 이 확인이 따로 있다.

**캡처는 쓰기를 실패시키지 않는다. 예외가 있다.** 1부터 7까지와 설치의 확인이 한 `EXCEPTION WHEN OTHERS` 블록 안이다. 블록 안에서 에러가 나면 블록이 한 일(묶음, 줄)이 되돌려지고, 묶음 하나를 `skipped = 'capture_failed'`와 `detail`의 SQLSTATE, 메시지로 남긴다. 32번째 캡처가 실패했으면 그 표지는 `too_many_statements`이고 `detail`에 그 실패가 남는다. 표지를 쓰는 것도 실패하면 서버 로그(`RAISE LOG`)에만 남긴다. 클라이언트에게는 아무 메시지도 가지 않는다.

쓰기를 실패시키는 것은 값도 표지도 남길 수 없고, 그 원인을 세션이 스스로 만들 수 있을 때다. 그런 에러를 삼키면 에이전트가 쓰기를 기록도 표지도 없이 커밋시킬 수 있다. 실패해 다시 보내지는 쓰기는 안전하고, 기록 없이 커밋되는 쓰기는 안전하지 않다.

- 시간 초과와 취소(`57014`). `WHEN OTHERS`가 잡지 않는다.
- 직렬화 실패(`40001`). 두 블록 모두 되돌려 보낸다. SERIALIZABLE에서는 캡처가 Stet의 표를 읽고 쓰는 것이 세션 사이에 충돌하고, 표지를 쓰는 것도 같은 충돌에 걸린다(11장).
- 자원과 한도(SQLSTATE 부류 `53`과 `54`. 깊은 함수 호출 끝에서 캡처만 스택이 모자라는 `54001`, 디스크가 찼다 등). 아무것도 남길 수 없을 때, 곧 표지를 쓰는 블록이 실패했거나 한 트랜잭션의 묶음을 세는 것이 실패했을 때만 되돌려 보낸다. 값을 남기는 블록만 실패하면 표지가 남고 쓰기는 성공한다.

그 밖의 에러는 삼킨다. `lock_timeout`(`55P03`)과 교착(`40P01`)도 그렇다. 캡처는 Stet의 표의 잠금만 기다리고 에이전트는 그 표를 잠글 수 없으므로, 에이전트는 캡처가 낀 교착을 만들 수 없다. 묶음을 세는 것 자체가 그런 에러로 실패하면(Stet의 계정이나 superuser가 `stet.change_sets`에 강한 잠금을 쥐어 `lock_timeout`에 걸렸다) 다시 세지 않고 표지도 쓰지 않으며 서버 로그에만 남긴다. 수를 모르면 표지가 33번째 쓰기가 될 수 있기 때문이다. `stet.capture_settings`를 읽지 못하면 누가 에이전트인지 모르므로, 캡처 트리거가 있는 테이블에 쓰는 모든 세션이 `capture_failed` 표지를 남긴다.

**한 트랜잭션의 상한.** 그 블록은 하위 트랜잭션이고, 표에 쓸 때만 자기 트랜잭션 id를 받는다. 커밋된 하위 트랜잭션의 id는 트랜잭션이 끝날 때까지 백엔드의 캐시(64개)에 남고, 넘치면 서버 전체의 스냅숏 확인이 느려진다(PostgreSQL의 알려진 동작). 표에 쓰는 캡처 하나는 id 하나만 남긴다. 그래서 한 트랜잭션에서 캡처가 표에 쓰는 것은 32번까지다.

- 캡처는 불릴 때마다(3 뒤, 무엇을 쓰기 전에) 이 백엔드가 이 트랜잭션의 id로 남긴 묶음을 센다(`txid_current_if_assigned()`와 `pg_backend_pid()`로, `change_sets (txid)`의 인덱스를 쓴다). 31개보다 적으면 남기고, 31개면 묶음 하나를 `skipped = 'too_many_statements'`로 남기고, 32개 이상이면 아무것도 남기지 않는다.
- 세는 것은 읽기뿐이라 트랜잭션 id를 받지 않는다. 그래서 캡처하지 않는 세션(3에서 끝나는 것)과, 잘 설치된 트리거의 0행 문장에서는 캡처가 아무것도 쓰지 않고 하위 트랜잭션 id도 받지 않는다.
- 롤백된 쓰기의 묶음은 보이지 않으므로 세지 않는다. 그 하위 트랜잭션도 자리를 돌려주었다.
- 세션의 설정처럼 에이전트가 적을 수 있는 값으로 세지 않는다. 시각으로 고르지도 않는다. 프로시저가 `COMMIT`한 뒤의 트랜잭션에서는 문장의 `statement_timestamp()`가 그 트랜잭션의 시작보다 이르다.
- pid도 보는 것은 다른 클러스터에서 옮겨 온 묶음(`pg_dump`, 논리 복제)이 같은 txid를 가질 수 있어서다. 옮겨 온 묶음이 txid와 pid까지 같으면 그 트랜잭션의 캡처가 줄 수 있다. 드물다.

**그 밖에.**

- enforce에서 Stet을 지나는 트랜잭션은 게이트 대상 테이블에 쓰기를 하나만 보낼 수 있다(이미 무언가를 바꾼 트랜잭션의 쓰기는 `ST002`로 막힌다). 그래서 이 상한에 닿는 것은 섀도의 큰 트랜잭션, 함수 안에서 여러 번 쓰는 문장, Stet을 거치지 않은 세션이다.
- 캡처 여부는 Stet의 모드와 상관없다. 트리거가 달려 있으면 섀도에서도 enforce에서도, Stet이 꺼져 있어도 남는다.
- `agents.roles`가 비어 있으면(모든 세션이 게이트 대상) `roles`는 NULL이고 모든 세션의 쓰기를 남긴다.
- 기록은 바뀌지 않는다. 두 표의 `UPDATE`와 `TRUNCATE`는 문장 단위 트리거가 `ST004`로 거부한다(0행을 고치는 `UPDATE`도). `DELETE`는 막지 않는다(보존 기간). 이 트리거들은 없을 때만 만든다(11장의 잠금).
- 에이전트 계정의 세션은 `stet` 스키마의 권한이 없으므로 기록을 읽거나 고칠 수 없다. `stet` 스키마의 함수는 어느 것도 PUBLIC이 부를 수 없다(기록을 지키는 트리거의 함수도 거둔다. 트리거는 함수의 권한 없이도 불린다). 예외는 승인된 문장이다. Stet의 연결에서 돌므로, 그 문장이 부르는 함수는 Stet의 계정이 할 수 있는 일을 할 수 있다(11장).

## 5. 판단 기록에 잇기

```sql
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS backend_pid integer;      -- 그 세션의 서버 쪽 백엔드 pid
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS sent_after  timestamptz;  -- DB 시계. 문장은 이보다 뒤에 서버로 갔다
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS done_before timestamptz;  -- DB 시계. 문장의 결과는 이보다 앞에 나왔다
CREATE INDEX IF NOT EXISTS decisions_window_idx ON stet.decisions (backend_pid, done_before, sent_after);
```

- 셋 다 줄을 넣을 때만 적는다(규칙 3). 서버로 보내지 않은 쓰기와 pid를 모르는 세션의 쓰기는 NULL이다. 프록시의 세션이 도중에 끝나 결과를 잃은 쓰기에는 창이 적힐 수 있다. 그 쓰기는 `executed`가 거짓이므로 잇는 데 쓰이지 않는다.
- `backend_pid`는 프록시가 서버의 BackendKeyData에서 읽는다. 지켜보는 세션에서만 읽고, 메시지는 그대로 넘긴다.
- 두 시각은 DB의 시계다. Stet은 자기 시계의 시각을 보내지 않고, 쓰기가 Stet에 들어온 때와 게이트가 결과를 받은 때부터 줄을 넣기 직전까지 흐른 시간을 보낸다. DB는 줄을 넣는 문장의 트랜잭션이 시작한 때(`transaction_timestamp()`)에서 그것을 빼 `sent_after`와 `done_before`로 적는다. 그 트랜잭션은 넣는 문장의 메시지가 서버에 닿을 때 시작하므로, 그 뒤에 `stet.decisions`의 잠금을 기다려도 시각이 늦어지지 않는다. 두 기기의 시계가 어긋나도 틀리지 않는다. 남는 오차는 삽입 문장이 서버에 닿는 시간뿐이다. 반올림은 창을 넓히는 쪽으로만 한다.
- 인덱스로 묶음마다 후보를 찾는다. pid가 같고 `done_before`가 묶음의 `stmt_at` 이후인 범위를 훑고, `sent_after`는 인덱스 안에서 거른다. 최근의 묶음일수록 훑을 것이 적다.

**잇는 규칙.** 변경 묶음 하나는 다음을 모두 만족하는 판단 기록이 **하나뿐일 때** 그 기록에 이어진다.

- `backend_pid`가 묶음의 `pid`와 같다.
- `executed`가 참이다.
- 묶음의 `stmt_at`이 `sent_after`에서 10ms를 뺀 때와 `done_before` 사이다. 앞쪽에만 여유를 둔다. 삽입 문장이 서버에 닿는 시간만큼 두 값이 실제보다 늦게 적히기 때문이다.
- `statement`가 묶음의 `query`와 같은 글자다.

- 하나도 없으면 Stet이 판단하지 않은 변경이다. 함수나 뷰를 거친 쓰기, Stet을 거치지 않고 붙은 세션의 쓰기, 판단 기록을 넣지 못한 쓰기다. SQL의 `PREPARE`와 `EXECUTE`로 실행한 쓰기도 그렇다. 판단 기록에는 `PREPARE`의 글자가 남고 묶음의 `query`는 `EXECUTE`의 글자다.
- 둘 이상이면 잇지 않고 후보의 수만 보여 준다. 대상 키로 좁히지 않는다. 판단 기록의 `target_pk`는 에이전트가 쓴 글자이고 캡처한 `key`는 타입의 출력이어서 같은 행도 글자가 다를 수 있다(`'007'`과 `7`). 키를 다시 쓰는 트리거, 값을 남기지 않은 묶음, 아무것도 바꾸지 않은 쓰기가 섞이면 키로 좁혀도 다른 쓰기의 판단 기록에 잇게 된다. 잘못 잇는 것보다 잇지 않는 쪽이다.
- 후보가 여럿이 되는 것은 같은 연결에서 같은 글자의 문장이 서로의 창 안에서 실행됐을 때다. 한 Sync 아래의 일괄 실행과 파이프라인이 그렇다. 프록시는 Sync의 응답이 나갈 때 그 아래 쓰기들의 결과를 알리므로 각 창이 다른 쓰기의 문장을 품는다. 준비한 문장 하나를 값만 바꿔 빠르게 되풀이하는 것도 그렇다. 고친 행이 다르든 같든 이어지지 않는다.
- 연쇄로 바뀐 자식 테이블의 묶음은 `stmt_at`과 `query`가 부모의 문장과 같으므로 같은 결과를 보인다.
- 승인으로 실행된 쓰기는 `held_write`로 보류 줄에, 거기서 원래의 판단 기록에 이어진다. 이 규칙을 쓰지 않는다. 보류 줄이나 그 판단 기록이 없으면 잇지 않는다.
- 잇는 것은 저장하지 않는다. 뷰가 읽을 때 계산한다(6장). 판단 기록은 쓰기가 끝난 뒤에 들어가고 묶음은 트랜잭션이 커밋돼야 보이므로, 어느 쪽이 먼저인지 정해져 있지 않다.
- `statement`와 `current_query()`가 같은 글자로 남는 것은 단순 프로토콜과 확장 프로토콜 모두에서 테스트로 확인했다. 다르게 남는 경로(SQL의 `PREPARE`와 `EXECUTE`)의 쓰기는 이어지지 않는다.

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
| `undo_id`, `undone_at` | 이 변경을 `restored`나 `restored_differs`로 되돌린 마지막 되돌리기(7장). 건너뛰거나 실패한 줄은 들지 않는다 |

뷰는 소유자(Stet의 계정)의 권한으로 읽는다. `before`와 `after`에는 그 테이블의 모든 컬럼이 있다. 그래서 이 뷰를 읽을 수 있는 역할은 캡처하는 모든 테이블의 이전·이후 값을 본다. 그 역할이 `SELECT`할 수 없는 테이블의 것도 그렇다. `stet.undo_selection`은 부른 세션이 읽을 수 없는 테이블의 키를 보이지 않지만(7장) 뷰는 가리지 않는다. 누구에게 보여 줄지는 운영자가 정한다. 검토자에게는 `stet` 스키마의 `USAGE`와 이 뷰의 `SELECT`를 준다(7장의 권한. `stet undo-sql -grant`의 출력에 들어 있다).

## 7. 되돌리기

```sql
stet.undo_plan(agent_role text, since timestamptz, until timestamptz DEFAULT NULL, tables text[] DEFAULT NULL)
stet.undo(agent_role text, since timestamptz, until timestamptz DEFAULT NULL, tables text[] DEFAULT NULL, note text DEFAULT NULL)
  -- 둘 다 RETURNS TABLE (target_table text, key text, action text, outcome text, reason text, warning text, change_ids bigint[])
```

**READ COMMITTED에서만.** `stet.undo_plan`과 `stet.undo`는 READ COMMITTED 트랜잭션에서만 돈다. 다른 격리 수준이면(PostgreSQL이 READ COMMITTED처럼 다루는 READ UNCOMMITTED도) `ST006`으로 거절한다. 테이블이나 기록을 읽는 도우미를 직접 불러도 같다. REPEATABLE READ와 SERIALIZABLE에서는 SQL로 읽는 카탈로그가 트랜잭션의 스냅숏을 따르는데 문장 안의 이름은 지금의 카탈로그에서 풀린다. 그래서 스냅숏 뒤에 이름을 바꾸거나 다시 만든 테이블, 바뀐 컬럼을 맞추는 확인마다 빈틈이 남는다. READ COMMITTED에서는 테이블을 잠근 뒤에 시작하는 문장이 지금의 카탈로그와 행을 읽는다.

```sql
BEGIN ISOLATION LEVEL READ COMMITTED;
SELECT * FROM stet.undo_plan('agent_enricher', '2026-11-18 09:00+09');
SELECT * FROM stet.undo('agent_enricher', '2026-11-18 09:00+09');
COMMIT;  -- 결과를 보고. 아니면 ROLLBACK
```

**고르기.** `agent_role`이 그 계정이고, `at`이 `since` 이상 `until` 미만이고(`until`이 없으면 지금까지), `tables`가 있으면 `target_table`이 그 안에 있는 묶음이다. `tables`는 설정의 테이블 키다.

- 기록은 `stet.undo_selection`(소유자의 권한으로 도는 도우미)으로만 읽는다. 검토자는 `stet.change_sets`와 `stet.changes`를 읽을 권한이 없다. 이 함수는 먼저 아래의 거절을 하고, 인자로 고른 것만 돌려준다.
- 어느 테이블의 변경인지는 `relid`가 말한다. `target_table`은 트리거를 단 쪽이 고른 이름이고 `relid`는 서버가 적은 것이다. 결과에는 `target_table`을 보이지만 행을 읽고 판단하는 것은 모두 `relid`로 한다. 한 키의 변경은 `(target_table, relid, key_column, key)`로 묶는다. 지우고 같은 이름으로 다시 만든 테이블은 `relid`가 달라 그 전의 변경이 `table_gone`이 된다.
- 부른 세션이 읽을 수 없는 테이블은 키를 하나도 보이지 않는다. 키도 데이터다(이메일 같은 자연 키). 그 테이블은 `key` 없는 한 줄이고, `outcome`은 `skipped`, `reason`은 `SQLSTATE 42501: permission denied for table <이름>`이다. 읽을 수 있다는 것은 `session_user`나 그것이 `SET ROLE`로 될 수 있는 역할 가운데 하나가 그 테이블의 `SELECT`를 갖고, 그 역할에게 행 수준 보안이 적용되지 않는 것이다. 곧 행 수준 보안이 꺼져 있거나, 그 역할이 superuser나 `BYPASSRLS`이거나, 테이블 소유자의 권한을 갖고 테이블이 `FORCE ROW LEVEL SECURITY`가 아니다. 고를 때 이미 없어진 테이블도 `key` 없는 한 줄이고 `reason`은 `table_gone`이다. 이 두 경우 `stet.undo_selection`은 그 줄의 출력 컬럼 `withheld`에 이유를 담고, 계획과 되돌리기는 그것을 `reason`으로 보인다.
- 결과의 줄은 `target_table`(바이트 순서), 그다음 그 키의 첫 변경의 순서다. `key` 없는 줄은 자기 테이블의 줄 가운데 그 묶음이 남은 때에 선다.

**키마다 하나의 결정.** 고른 변경을 테이블과 키로 묶고 남긴 순서로 본다. 그 키에서 고른 첫 변경의 `before`를 B, 마지막 변경의 `after`를 A라 한다. 되돌린다는 것은 그 키의 행을 B로 만드는 것이다.

| 차례로 확인 | 걸리면 |
|---|---|
| 테이블이 없다(`relid`가 보통 테이블을 가리키지 않는다) | 건너뛴다. `table_gone` |
| 남긴 값의 컬럼 이름들이 지금 테이블의 컬럼들과 다르거나, 키 컬럼이 없다 | 건너뛴다. `columns_changed` |
| 고른 변경 가운데 키를 바꾼 `UPDATE`의 묶음에 든 것이 있다. 줄 가운데 `before`나 `after`가 없는 것이 하나라도 있는 `update` 묶음이고, 그 묶음의 줄은 양쪽이 다 있는 것도 모두 그렇다 | 건너뛴다. `key_changed`. 옛 키와 새 키의 두 줄을 한 행으로 묶을 근거가 없다. 한쪽만 되돌려지면 행이 사라진다 |
| 고른 변경 사이가 이어지지 않는다(앞 변경의 `after`가 다음 변경의 `before`와 다르다) | 건너뛴다. `interleaved`. 그 사이에 고르지 않은 쪽이 그 행을 바꿨다 |
| 지금 그 키의 행이 A와 다르다(A가 없는데 행이 있다, A가 있는데 행이 없다도 든다) | 건너뛴다. `changed_since`. 그 뒤에 누가 그 행을 바꿨다 |
| B와 A가 같다(둘 다 없는 것도 든다) | 건너뛴다. `no_net_change` |

걸리지 않으면 `action`이 정해진다. B가 없으면 `delete`(에이전트가 만든 행을 지운다), A가 없으면 `insert`(에이전트가 지운 행을 다시 넣는다), 둘 다 있으면 `update`다.

- 확인은 테이블을 잠근 뒤에 한다. 판단을 위해 테이블을 잠그는 곳은 `stet.undo_lock` 하나다(복구 전의 `ROW EXCLUSIVE`는 아래의 `referenced`가 잡는다). 격리 수준을 확인하고, 테이블을 `ACCESS SHARE`로 잠그고, 잠금을 기다리는 동안 그 이름이 다른 테이블로 넘어가지 않았는지(`55000`)와 부른 역할에게 행 수준 보안이 적용되는지(`42501`)를 본 뒤 컬럼을 읽는다. 파티션이면 `0A000`으로 거절한다. 잠근 뒤에는 트랜잭션이 끝날 때까지 그 테이블의 이름, 컬럼, 행 수준 보안을 바꾸는 `ALTER TABLE`이 기다린다.
- 비교는 남길 때와 같은 방법으로 만든 글자끼리 한다. 지금 행의 값도 4장의 다섯 설정을 고정한 함수 안에서 같은 출력 함수로 만든다.
- 키를 바꾼 `UPDATE`의 묶음을 통째로 건너뛰는 까닭. (1, A)와 (2, B)에 `UPDATE t SET id = id + 1`을 하면 키 2의 줄에는 `before` (2, B)와 `after` (2, A)가 다 있지만 둘은 서로 다른 행이다. 키 2만 (2, B)로 되돌리면 B가 두 행에 있고 A는 기록에만 남는다. 그래서 그런 묶음의 키는 셋 다 `key_changed`이고 테이블은 그대로다.
- 지금 그 키의 행은 키 컬럼의 값이 키의 글자와 같은 행이다. 키의 글자는 타입을 적지 않은 리터럴로 넣어 그 컬럼의 타입의 입력 함수로 읽고 `OPERATOR(pg_catalog.=)`로 비교한다. 같음 연산자가 `pg_catalog`에 있는 타입(정수, `text`, `uuid`, `timestamptz` 등)에서는 키의 인덱스를 쓰고, 유일 제약과 복구 문장이 보는 것과 같은 같음이다. 같음 연산자가 `pg_catalog` 밖에 있는 타입(`citext` 같은 확장 타입)에서는 그렇지 않다. `pg_catalog`의 `=`는 캐스트를 거친 다른 비교(`citext`면 글자 그대로의 `text` 비교)로 풀려, 키의 인덱스를 쓰지 못하고 찾을 때마다 테이블을 훑는다. 결과는 안전한 쪽이다. 유일 제약으로는 같지만 글자가 다른 행은 그 키의 행으로 보지 않으므로 `changed_since`로 건너뛰거나, 넣으려다 유일 제약에 걸려 `failed`(`23505`)가 된다. Stet은 시작할 때와 `stet undo-sql`의 출력에서 그런 키를 알린다(9장). 두 행 이상이 나오면 키 컬럼이 유일하지 않다는 에러(`21000`)다.
- 다른 쪽이 바꾼 것인지는 값으로만 안다. 캡처하지 않는 계정(사람, 애플리케이션)의 변경도 그래서 잡힌다. 누가 값을 바꿨다가 정확히 같은 값으로 돌려놓은 것은 구분하지 못한다. 그때는 되돌려도 잃는 것이 없다.
- 고른 범위에 값을 남기지 않은 묶음(`skipped`)이 있으면 `key` 없이 한 줄로 나온다. `outcome`은 `not_captured`이고 `reason`은 그 `skipped`다. 그 문장이 바꾼 행은 되돌려지지 않는다.

**`stet.undo_plan`**은 위의 확인만 하고 아무것도 바꾸거나 적지 않는다. 행을 잠그지 않는다. 판단한 테이블의 `ACCESS SHARE` 잠금은 부른 트랜잭션이 끝날 때까지 남는다(그동안 그 테이블의 `ALTER TABLE`이 기다린다. 쓰기는 기다리지 않는다). `outcome`은 `would_restore`, `skipped`, `not_captured`다. 키를 판단하다 난 에러(지금의 역할에 그 테이블의 `SELECT`가 없다, 행 수준 보안이 적용된다, 키가 유일하지 않다 등)는 그 키를 `skipped`로, `reason`을 `SQLSTATE <코드>: <메시지>`로 돌려준다. 경고는 되돌릴 줄에만 붙는다.

**`stet.undo`**는 키마다 하위 트랜잭션 하나(예외 블록) 안에서 다음을 한다.

1. 테이블을 잠그고 그 키의 행을 `FOR UPDATE`로 잠근 뒤 위의 확인을 다시 한다. 걸리면 건너뛴다.
2. 그 행을 가리키는 다른 행이 있는지 본다(아래의 `referenced`). 있으면 건너뛴다.
3. `action`의 문장을 실행한다.
   - `delete`는 그 키의 행을 지운다. `update`는 A와 B가 다른 컬럼만 B의 값으로 바꾼다. 결과의 행은 같고, 값이 같은 컬럼(키 컬럼 등)을 다시 쓰지 않으므로 외래 키 확인과 `UPDATE OF` 트리거가 쓸데없이 돌지 않는다. `insert`는 B의 행을 넣는다.
   - 값은 타입을 적지 않은 글자 리터럴이다. PostgreSQL이 그 컬럼의 타입(도메인이면 기본 타입의 입력 뒤 도메인의 제약)의 입력 함수로 바로 읽는다. 글자에서 그 타입으로 가는 캐스트는 돌지 않고, 타입의 이름을 부른 세션에서 풀지도 않는다. 컬럼에 맞지 않는 값은 잘리지 않고 에러다.
   - 생성 컬럼에는 값을 주지 않는다. `GENERATED ALWAYS AS IDENTITY` 컬럼은 `insert`에서만 준다(`OVERRIDING SYSTEM VALUE`. 테이블 소유자가 아니어도 된다). `UPDATE`로는 값을 줄 수 없는 컬럼이다. `update`에서 바꿀 컬럼이 그런 것뿐이면 문장이 없고, 그 행은 B가 될 수 없어 `restored_differs`가 된다.
   - `delete`와 `update`는 `ONLY`로, 키 컬럼과 `tableoid`로 그 행만 고른다.
4. 결과를 본다. 문장이 두 행 이상을 바꿨거나(`21000`), 다른 테이블에 썼거나(`55000`), 문장을 만든 뒤 실행하기 전에 그 테이블의 이름이 다른 테이블로 넘어갔으면(`55000`. 테이블을 잠가도 `ALTER SCHEMA ... RENAME`은 기다리지 않는다) 그 키는 `failed`다. 아니면 그 키의 행을 다시 읽어 B와 비교한다. 같으면 `restored`, 다르면 `restored_differs`다(고객의 트리거나 기본값이 값을 바꿨다. 값을 줄 수 없는 컬럼이 B와 달랐다. `delete`인데 행이 남아 있다).
5. 블록 안에서 에러가 나면 그 키의 블록만 되돌리고(잡은 행 잠금과 쓴 것이 함께 풀린다) `failed`로 적는다. `reason`은 `SQLSTATE <코드>: <메시지>`다. 고객 트리거의 `ASSERT` 실패도 잡는다. 교착(`40P01`)과 직렬화 실패(`40001`. READ COMMITTED에서는 고객의 코드만 낸다)도 블록을 되돌리면 트랜잭션을 계속 쓸 수 있으므로 `failed`다.

- 건너뛰는 키도 그 키의 블록을 되돌리고 나온다(Stet 안에서만 쓰는 SQLSTATE로). 그래서 1에서 잡은 행 잠금과 하위 트랜잭션 id가 검토자가 커밋하기를 기다리지 않고 풀린다. 고객의 트리거가 같은 SQLSTATE를 내면 다른 에러처럼 `failed`다.
- **끝에서 다시 보기.** 모든 바퀴(아래)가 끝나면 마지막 결과가 `restored`인 키를 모두 다시 읽는다. 뒤의 키를 복구할 때 고객의 트리거가 앞의 행을 바꾸거나 지운 행을 다시 넣었을 수 있다. 지금 행이 B가 아니면 `restored_differs`로 고친다. 다시 읽다가 에러가 나면 `restored_differs`로 고치고 `reason`에 그 에러를 적는다. 그래서 돌려주는 줄은 돌려줄 때 참이다.
- **되돌리기는 연쇄를 일으키지 않는다(`referenced`).** 에이전트가 만든 행을 지우면, 그 행을 가리키는 외래 키가 `ON DELETE CASCADE`·`SET NULL`·`SET DEFAULT`일 때 다른 쪽이 그 뒤에 만든 자식 행까지 지워지거나 바뀐다. `update`가 참조되는 컬럼의 값을 바꿀 때 `ON UPDATE`의 연쇄도 같다. 그래서 `delete`나 `update`의 키는 그 행을 잠그고 A와 같음을 확인한 뒤, 복구하기 전에 다음을 본다.
  - 먼저 자기 복구 문장을 `EXPLAIN`으로 계획한다. 실행하지 않으므로 트리거도 돌지 않는다. PostgreSQL은 문장을 분석할 때 그 테이블에 `ROW EXCLUSIVE`를 잡아 트랜잭션이 끝날 때까지 쥐고, `EXPLAIN`에서도 그 문장의 권한을 컬럼 권한까지 확인한다. 권한이 없으면 그 키는 `failed`(`42501`)다. 그 뒤 이 백엔드가 그 테이블(`relid`)에 그 잠금을 쥐었는지 `pg_locks`로 본다. 잠금을 기다리는 동안 그 이름이 다른 테이블로 넘어갔으면 쥐지 않았으므로 그 키는 `failed`(`55000`)다. 이 잠금을 쥐는 동안에는 그 테이블을 가리키는 외래 키를 더할 수 없다.
  - 그 테이블을 가리키는 외래 키 가운데 이번 복구에 대한 동작이 `CASCADE`, `SET NULL`, `SET DEFAULT`인 것을 읽는다. `delete`면 `ON DELETE`, `update`면 `ON UPDATE`다. `update`는 복구 문장이 그 외래 키가 가리키는 컬럼을 실제로 바꿀 때만 본다(값을 주는 컬럼과, 값이 달라 다시 계산되는 생성 컬럼). 파티션마다 복제된 외래 키는 보지 않는다. `NO ACTION`과 `RESTRICT`는 서버가 `23503`으로 막는다.
  - 따라가지 않는 외래 키가 있다. 컬럼 짝의 타입이 도메인을 벗긴 기본 타입에서 다르거나(`integer`와 `bigint`, `timestamptz`와 `timestamp`. 연쇄는 그런 비교에 캐스트를 더하고, 그 결과는 세션의 설정에 따를 수 있다), 비교에 드는 콜레이션 넷(부모 컬럼의 것과 그 타입의 것, 자식 컬럼의 것과 그 타입의 것) 가운데 하나라도 비결정적이거나, 연쇄가 쓸 콜레이션을 PostgreSQL이 정할 수 없는 외래 키다. 연쇄는 부모 컬럼의 타입의 콜레이션과 자식 컬럼의 콜레이션을 합쳐 쓰는데(같으면 그것, 한쪽이 기본이면 다른 쪽), 둘 다 기본이 아니고 서로 다르면 정할 수 없다. 제약의 같음 연산자가 `pg_catalog` 밖에 있고 의사 타입(`anyarray` 등)을 받는 외래 키도 따라가지 않는다(바로 아래). 그런 외래 키를 불러일으킬 복구는 자식이 있든 없든 `failed`(`SQLSTATE 0A000`)이고, 메시지가 제약과 자식 테이블을 말한다.
  - 나머지 외래 키마다, 잠근 행과 자식 테이블을 PostgreSQL의 연쇄가 쓰는 비교 그대로 조인해 연쇄가 닿을 자식 행이 있는지 본다. 비교는 제약 자신의 같음 연산자이고, 양쪽을 그 연산자가 받는 타입으로 캐스트하고, 연쇄가 쓰는 콜레이션을 적는다. 그래서 다른 쪽이 그 스키마에 컬럼의 타입끼리의 같은 이름의 연산자를 더해도 고르지 않는다. 연산자가 의사 타입을 받으면 이 말이 성립하지 않는다. 의사 타입으로의 캐스트는 피연산자의 타입을 바꾸지 않아서, 컬럼의 타입끼리의 같은 이름의 연산자가 있으면 그것이 골라진다. `pg_catalog`에는 superuser만 연산자를 더할 수 있으므로 `pg_catalog`의 그런 연산자(배열의 같음 등)는 그대로 따라가고, 다른 스키마의 것은 위에서 거절한다. 부모의 값은 남긴 글자를 다시 읽지 않고 잠근 행에서 가져온다. 자기를 가리키는 외래 키에서는 그 행 자신을 뺀다.
  - 자식 테이블은 검토자의 권한으로 읽는다. 읽을 권한이 없거나, 조사 앞뒤로 행 수준 보안이 검토자에게 적용되면 그 키는 `failed`(`42501`)다. 연쇄는 행 수준 보안을 지나가므로 숨은 자식을 놓칠 수 있기 때문이다.
  - 조사한 뒤에 읽은 것이 그 외래 키의 자식 테이블인지 확인한다. 조사는 자식 테이블을 이름으로 적는데, 조사가 그 테이블의 잠금을 기다리는 동안 그 이름이 다른 테이블로 넘어갈 수 있다(`ALTER SCHEMA ... RENAME`은 테이블을 잠그지 않는다). 그러면 PostgreSQL은 이름이 지금 가리키는 테이블을 읽고, 조사는 자식이 없다고 답하며, 복구는 진짜 자식으로 연쇄한다. 그래서 두 가지를 다 본다. 조사에 적은 바로 그 이름이 아직 그 외래 키의 테이블을 가리키고, 이 백엔드가 그 테이블에 `ACCESS SHARE`를 쥐고 있어야 한다. 하나라도 아니면 그 키는 `failed`(`55000`)이고 아무것도 쓰지 않는다. 이름만 보면 조사 뒤에 돌아온 이름을 놓치고, 잠금만 보면 같은 되돌리기의 앞의 키가 잡아 둔 잠금에 속는다. 남는 경우는 11장에 있다.
  - 자식 행이 있으면 그 키를 건너뛴다. `reason`은 `referenced: <테이블들>`이다. 그 자식 행이 같은 되돌리기에서 먼저 지워지면(에이전트가 부모와 자식을 함께 만들었다) 아래의 "다시 해 보기"에서 된다.
  - 잠근 뒤에 보므로 그 사이에 새 자식이 생기지 않는다. 비교가 결정적이고 부모의 유일 인덱스의 같음이므로, 그런 자식은 모두 잠근 행을 가리킨다. 그런 자식을 넣거나 이 행을 가리키게 고치는 쪽은 이 행에 `FOR KEY SHARE`를 잡아야 하므로 검토자의 트랜잭션이 끝나기를 기다린다.
- **순서와 다시 해 보기.** 순서가 맞아야 되는 것이 있다. 지워진 부모는 자식보다 먼저 넣어야 하고, 함께 만든 부모와 자식은 자식부터 지워야 한다. 순서를 미리 계산하지 않는다. 첫 바퀴는 `delete`를 그 키의 첫 변경이 나중인 것부터, 그다음 나머지를 결과의 순서대로, `insert`를 그 키의 마지막 변경이 앞선 것부터 해 본다. 외래 키 위반(`23503`), 유일 제약 위반(`23505`), `referenced`로 되지 않은 키는 다음 바퀴에 다시 해 본다. 한 바퀴에서 하나도 되돌리지 못하면 그만두고, 남은 것은 그 이유로 `failed`나 `skipped`다. 그 밖의 실패는 다시 해 보지 않는다. 고른 줄은 처음에 한 번 읽고, 바퀴마다 다시 고르지 않는다. `stet.undo`는 고른 줄을 그 값(B와 A)과 함께 `jsonb` 하나로 들고 다닌다. `jsonb` 하나는 256MB를 넘지 못하므로, 고른 값이 그보다 크면 `stet.undo`는 `54000`으로 끝나고 아무것도 되돌리지 않는다. `stet.undo_plan`은 줄을 하나씩 읽으므로 된다. 그때는 시간 범위나 `tables`로 나눠 부른다. 코드에서 읽은 것이고 그런 크기로 해 보지는 않았다. 캡처에도 같은 한도가 있다. 한 값이 256MB 이상인 행을 바꾼 문장은 값 없이 `capture_failed` 표지만 남기므로(11장), 되돌리기에서는 `not_captured`로 나온다.
- **유일 제약의 값을 맞바꾼 행은 되돌리지 못한다.** PostgreSQL은 미룰 수 없는 유일 제약을 행마다 확인하므로, 한 행씩 돌리는 어떤 순서도 통과하지 못한다. 두 키 모두 `failed`(`SQLSTATE 23505`)이고 그대로다. 제약이 `DEFERRABLE`이고 검토자가 미루면(`SET CONSTRAINTS ... DEFERRED`) 돌아온다. 한 행에서 다른 행으로 옮겨 간 값은 두 번째 바퀴에서 돌아온다.
- **에러를 내지 않는다.** 건너뛴 것과 실패한 것은 결과의 줄로 돌려준다. 검토자는 결과를 보고 커밋하거나 롤백한다. 에러를 내는 것은 아래의 거절(`ST006`), 함수의 `EXECUTE`가 없는 것(`42501`. `stet.undo`와 키마다의 블록 밖에서 부르는 도우미), 취소와 시간 초과(`57014`. 아무것도 기록되지 않는다), 읽기 전용 트랜잭션(`25006`. 기록을 적을 때 난다. 그 전에 모든 키가 `failed`다), 고른 값이 `jsonb` 하나에 담기지 않는 것(`54000`. 위)뿐이다. 미룬 제약(`DEFERRABLE INITIALLY DEFERRED`인 외래 키와 유일 제약, 제약 트리거, 검토자의 `SET CONSTRAINTS`)은 커밋할 때 확인되므로, 그 실패는 결과의 줄이 아니라 검토자의 `COMMIT`에서 나오고 되돌리기 전체가 롤백된다.
- **복구 문장은 부른 세션의 설정으로 돈다.** `search_path`, `TimeZone` 같은 설정을 `stet.undo`가 바꾸지 않는다. 고객의 트리거가 평소와 같은 환경에서 돌게 하려는 것이다. 대신 `stet.undo`의 몸통은 이름을 푸는 일을 하지 않는다. 함수와 변수의 타입을 스키마까지 적는다(부른 세션의 임시 스키마는 릴레이션과 타입의 이름을 `pg_catalog`보다 먼저 찾는다). Stet 자신의 읽기와 비교는 일곱 설정을 고정한 도우미 함수 안에서 하고, 조립한 복구 문장은 테이블을 스키마까지 적고 연산자를 `OPERATOR(pg_catalog.=)`로 적는다.
- **경고(`warning`).** 되돌리기는 하되 검토자가 알아야 하는 것이다. 되돌리는 줄에만 붙는다(계획의 `would_restore`, 되돌리기의 `restored`와 `restored_differs`).
  - `children_not_captured: <테이블들>`. `insert`로 되돌리는 테이블을 가리키는 외래 키가 `ON DELETE CASCADE`·`SET NULL`·`SET DEFAULT`인데, 그 자식 테이블에 그 연쇄가 일으키는 쓰기(`CASCADE`는 `DELETE`, 나머지는 `UPDATE`)의 켜진 캡처 트리거가 없다. 함께 지워지거나 바뀐 자식 행은 남지 않았으므로 돌아오지 않는다.
  - `not_selected: <테이블들>`. 같은 문장이 다른 테이블에서도 행을 바꿨는데(연쇄 등) `tables`로 고르지 않아 그 변경은 되돌리지 않는다.
- 같은 것을 두 번 되돌리면 두 번째는 모두 `changed_since`로 건너뛴다. 지금 행이 A가 아니기 때문이다.

**거절하는 경우.** SQLSTATE는 `ST006`이고 `stet.undo_plan`과 `stet.undo`에 똑같이 적용한다. `agent_role`이나 `since`가 NULL이다. `until`이 `since`보다 앞이거나 같다. `session_user`가 `agent_role`이다(자기 쓰기를 되돌리거나 그 기록을 읽을 수 없다). 트랜잭션의 격리 수준이 READ COMMITTED가 아니다(위).

**기록.** `stet.undo`는 부를 때마다 검토자의 트랜잭션 안에서 세 표에 적는다. 롤백하면 이 기록도 사라진다. 일어나지 않은 되돌리기는 남지 않는다.

- `stet.undos`에 한 줄. id, 때, `session_user`, 고른 조건, 검토자가 적은 `note`, 결과의 수 셋이다. `restored`는 `restored`와 `restored_differs`를, `skipped`는 `skipped`와 `not_captured`를, `failed`는 `failed`를 센다.
- `stet.undo_items`에 결과의 줄마다 한 줄. 결과에서의 차례(`line`, 1부터)와 결과의 일곱 컬럼이다.
- `stet.undone_changes`에 되돌린 줄(`restored`, `restored_differs`)의 변경마다 한 줄. `stet.history`의 `undo_id`와 `undone_at`이 이것으로 찾는다.

세 표는 `stet.change_sets`와 `stet.changes`를 외래 키로 가리키지 않는다. 보존 기간(9장)이 오래된 변경을 지울 때 이 표들을 확인하거나 잠그지 않고, 지운 변경의 되돌리기 기록도 남는다.

**권한.** `stet.undo`와 `stet.undo_plan`은 부른 계정의 권한으로 돈다(`SECURITY INVOKER`). 도우미도 대부분 그렇다. `SECURITY DEFINER`인 것은 기록을 읽는 `stet.undo_selection`과 기록을 적는 `stet.undo_record` 둘이다.

- `stet.undo_selection`은 위의 거절을 하고, 인자로 고른 것만, 부른 세션이 읽을 수 있는 테이블의 키만 돌려준다.
- `stet.undo_record`는 받은 줄을 믿지 않는다. 줄의 꼴을 확인하고(아니면 `22023`), 수는 스스로 센다. 기록의 주인은 `session_user`이고 인자로 받지 않는다. `stet.undone_changes`에는 고른 조건에 드는 그 계정의 변경만 적는다. 그래서 직접 불러도 남의 이름으로 기록을 만들거나, 다른 계정·테이블·시간의 변경을 되돌렸다고 보이게 할 수 없다. 자기 이름으로 지어낸 기록은 만들 수 있다(11장).
- `stet.undo`, `stet.undo_plan`과 도우미, 곧 `stet.undo*` 함수 18개의 `EXECUTE`를 모두 PUBLIC에서 거둔다. 테이블 소유자도 `stet` 스키마의 `USAGE`를 가지므로(4장), 열어 두면 도우미로 남긴 값을 읽을 수 있다.

누가 되돌릴 수 있는지는 운영자가 정한다. 검토자에게 줄 문장은 `stet undo-sql -config stet.yaml -grant <역할>`이 출력하고(9장), Stet의 계정이 실행한다.

```sql
-- Run as stet (the account Stet connects as): lets "reviewer" read stet.history and undo an agent's changes.
GRANT USAGE ON SCHEMA stet TO reviewer;
GRANT SELECT ON stet.history TO reviewer;
GRANT EXECUTE ON FUNCTION stet.undo(text, timestamp with time zone, timestamp with time zone, text[], text) TO reviewer;
GRANT EXECUTE ON FUNCTION stet.undo_failure(text, text) TO reviewer;
-- (stet.undo* 함수마다 한 줄, 모두 18줄)
-- The role also needs SELECT, INSERT, UPDATE and DELETE on the tables it is to restore, and SELECT on the tables that
-- reference those with ON DELETE or ON UPDATE CASCADE, SET NULL or SET DEFAULT; grant those as the tables' owners.
```

- 검토자는 되돌릴 테이블에 `SELECT`, `INSERT`, `UPDATE`, `DELETE`가 있어야 한다. 행을 `FOR UPDATE`로 잠그는 데 `UPDATE`가 든다. 그 테이블을 연쇄 외래 키로 가리키는 자식 테이블에는 `SELECT`가 든다(`referenced`를 보려고 읽는다). 없는 것이 있으면 그 키는 `failed`(`42501`)다. 고객의 트리거가 쓰는 다른 테이블의 권한도 검토자의 것으로 확인된다. 테이블의 권한은 테이블 소유자가 준다.
- 계획만 볼 검토자에게는 `stet` 스키마의 `USAGE`, 일곱 함수(`stet.undo_plan`, `stet.undo_selection`, `stet.undo_lock`, `stet.undo_row`, `stet.undo_judge`, `stet.undo_warning`, `stet.undo_refuse`)의 `EXECUTE`, 테이블마다 `SELECT`를 준다(`schema.sql`의 주석. `-grant`는 되돌릴 수 있는 검토자의 것을 출력한다). 이 계정은 `stet.undo`도 `stet.undo_record`도 부를 수 없으므로 아무것도 바꾸거나 적지 못한다.

## 8. 모드와 규칙

- **섀도(규칙 6).** Stet 프로세스의 동작은 바뀌지 않는다. 캡처는 테이블 소유자가 트리거를 설치해서 켜는 것이고, 켜면 에이전트의 쓰기에 시간이 더해진다(2장의 숫자). 캡처가 실패해도 쓰기는 성공한다. 예외는 4장의 셋이다. 그때 쓰기는 실패하고 다시 보내야 한다(SERIALIZABLE의 직렬화 실패가 늘어난다). 한 문장의 상한(`max_rows`)이 캡처의 시간을 묶는다. 남는 차이는 11장에 적는다.
- **규칙 1.** 캡처는 에이전트의 트랜잭션 안에서 돌지만 모델을 부르지 않는다. 되돌리기도 모델을 부르지 않는다.
- **규칙 3.** `stet.decisions`의 새 컬럼 셋은 줄을 넣을 때만 적는다.
- **규칙 5.** 남긴 값과 `query`, `app`은 에이전트가 쓴 데이터다. 모델에 보내지 않고, 판정에 쓰지 않는다. 되돌리기는 그것을 비교와 복구의 재료로만 쓰고, SQL에 넣을 때는 인용한다.
- 승인된 쓰기를 실행하는 쪽은 실행 전에 `stet.held_write`를 적는 것만 달라진다.
- Stet이 시작할 때 하는 일(9장)은 Stet 자신의 연결로만 하고 클라이언트의 세션을 건드리지 않는다. 두 모드에서 같다.

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

- `undo` 절이 없으면 Stet은 아래 "Stet이 시작할 때"의 일을 하지 않는다. `stet.capture_settings`를 맞추지 않고, `undo.tables`의 경고를 내지 않고, 오래된 묶음을 지우지 않는다. `stet undo-sql`도 `-drop` 말고는 거절한다. 멈추지 않는 것도 있다. 이미 설치된 트리거는 계속 캡처한다(4장). 그때 `stet.capture()`는 표에 남은 `stet.capture_settings`의 줄, 곧 마지막으로 맞춘 `agents.roles`와 `undo.max_rows`를 그대로 읽는다(줄이 없으면 모든 세션, 상한 10,000). 게이트는 `undo` 절과 상관없이 판단 기록에 `backend_pid`, `sent_after`, `done_before`를 적는다(5장). `schema.sql`의 표와 함수는 Stet이 스키마를 올릴 때 만들어진다. 값을 적지 않은 `undo:`는 절이 없는 것으로 읽힌다.
- 절이 없는데 캡처 트리거가 남아 있으면 Stet이 시작할 때 경고한다. 연결 풀을 연 뒤(게이트 대상 테이블이 있을 때) 카탈로그를 한 번 읽어 `stet.capture()`를 부르는 트리거가 달린 테이블을 찾고, 있으면 한 줄로 알린다. 테이블들의 이름, 캡처가 계속되고 오래된 기록을 아무도 지우지 않는다는 것, 고치는 두 방법이 들어 있다. 시작은 한다. 읽지 못하면 확인하지 못했다고 경고한다. 게이트 대상 테이블도 `undo` 절도 없으면 Stet은 풀을 열지 않으므로 이 확인도 없다.

```text
level=WARN msg="the config has no undo clause, but triggers that call stet.capture() are installed on public.contacts, public.deals: writes to these tables are still recorded, under the settings stet.capture_settings last had, and nothing deletes old change records, so stet.change_sets and stet.changes only grow; fix: put the undo clause back, or have the tables' owners run the output of stet undo-sql -drop"
```

- 되돌리기를 끌 때는 `stet undo-sql -config stet.yaml -drop`의 출력을 테이블 소유자들이 실행하고, 설정에서 `undo` 절을 지운다. 어느 쪽을 먼저 해도 된다. 절이 없는 설정으로 부른 `-drop`은 테이블을 카탈로그에서 찾는다(아래). 남은 변경 기록은 지워지지 않는다. Stet의 계정이 `stet.change_sets`에서 지울 수 있다(행의 줄은 연쇄로 지워진다).
- `undo` 절이 있는데 `tables`가 없거나 비어 있으면 설정 오류다.
- `tables`의 키에 있는 테이블에 `pk`를 적었는데 그쪽과 다르면 설정 오류다. 없는 키인데 `pk`가 없어도 설정 오류다. `max_rows`와 `keep`이 위의 범위 밖이어도 설정 오류다.
- `undo` 절이 있으면 게이트 대상 테이블이 없어도 Stet은 연결 풀을 열고 스키마를 올리고 아래의 일을 한다. 게이트와 기존의 시작 확인은 지금처럼 게이트 대상 테이블이 있을 때만 있다.

**`stet undo-sql -config stet.yaml [-drop] [-grant <역할>]`.** Stet의 계정으로(설정의 `upstream`) 붙어 읽기 전용 트랜잭션에서 카탈로그만 읽고, SQL을 표준 출력에 쓴다. 아무것도 실행하지 않는다. 실패하면 아무것도 출력하지 않고 0이 아닌 코드로 끝난다. 설정에 `undo` 절이 없으면 `-drop`만 되고(아래), 설치 SQL과 `-grant`는 거절한다.

- 데이터베이스에 `stet.capture()`가 없으면 "이 설정으로 Stet을 한 번 시작해 `stet` 스키마를 만든 뒤 다시 실행하라"고 말하고 끝난다. 첫 묶음의 `GRANT`는 그 스키마가 있어야 실행되는데, 이 명령은 스키마를 만들지 않는다.
- 서버가 PostgreSQL 14보다 오래되면 거절한다. `CREATE OR REPLACE TRIGGER`가 14부터 있고, 시험한 것도 14뿐이다.
- 출력은 묶음 여럿이고, 묶음마다 `-- N. Run as <계정> (...)`으로 시작한다. Stet의 계정이 아닌 테이블 소유자가 있으면 첫째는 Stet의 계정이 실행할 것이다(그런 소유자마다 `GRANT USAGE ON SCHEMA stet`, `GRANT EXECUTE ON FUNCTION stet.capture()`). 그다음은 테이블 소유자마다 그 소유자가 실행할 것(4장의 트리거)이다. 소유자의 묶음은 한 트랜잭션이고 `lock_timeout`을 5초로 둔다.
- 트리거는 `CREATE OR REPLACE TRIGGER`로 만들고 아무것도 지우지 않는다. 다시 실행하면 정의가 다른 트리거를 고치고 꺼진 트리거를 다시 켠다. 넷과 다른 이름으로 `stet.capture()`를 부르는 트리거는 그대로 둔다(시작할 때 경고한다). 테이블 소유자가 지운다. 넷 가운데 하나의 이름을 가진 제약 트리거는 바꿀 수 없으므로 소유자가 먼저 지운다.
- 테이블을 찾지 못하거나, 받지 않는 종류이거나, 키 컬럼이 받지 않는 것이면(4장) 테이블마다 그 이유를 말하고 0이 아닌 코드로 끝난다. SQL을 출력하지 않는다.
- 캡처하지 않는 자식 테이블(`ON DELETE CASCADE`·`SET NULL`·`SET DEFAULT`로 가리키는데 `undo.tables`에 없는 것. 7장의 `children_not_captured`가 될 것)은 주석으로 알린다. 키의 유일 인덱스가 `pg_catalog` 밖의 연산자로 비교하는 테이블도 주석으로 알린다. 되돌리기가 키마다 그 테이블을 훑는다(7장). 설치는 된다.
- `CREATE OR REPLACE TRIGGER`는 그 테이블에 쓰기를 막는 잠금을 잠깐 잡는다. 읽기는 막지 않는다. 열린 트랜잭션이 그 테이블에 쓰고 있으면 5초를 기다리다 실패하고, 그 소유자의 묶음은 아무것도 바꾸지 않는다.
- `-drop`은 찾은 테이블마다 트리거 넷을 지우는 SQL(`DROP TRIGGER IF EXISTS`)을 소유자마다 한 트랜잭션으로 출력한다. 찾지 못했거나 트리거를 가질 수 없는 테이블은 주석으로 알린다. 서버의 버전과 `stet.capture()`는 보지 않는다.
- 설정에 `undo` 절이 없으면 `-drop`은 테이블을 카탈로그에서 찾는다. `stet.capture()`를 부르는 트리거가 달린 모든 테이블이고, 그 트리거를 이름이 무엇이든 하나씩 지우는 SQL을 소유자마다 한 트랜잭션으로 출력한다. 넷과 다른 이름의 것도 지운다. 절이 없으면 그것을 경고해 줄 것이 없기 때문이다. 그런 트리거가 하나도 없으면 그렇게 말하고 0이 아닌 코드로 끝난다.
- `-grant <역할>`은 그 역할이 `stet.history`를 읽고 되돌릴 수 있게 하는 권한만 출력한다(7장의 권한). Stet의 계정이 실행한다. `stet` 스키마의 `USAGE`, `stet.history`의 `SELECT`, `stet.undo`와 `stet.undo_`로 시작하는 함수마다의 `EXECUTE`다. 함수는 카탈로그에서 읽고, `stet.capture()`의 소유자가 소유한 보통 함수만 넣는다. 다른 역할이 그 스키마에 같은 이름이나 다른 인자로 만든 함수는 넣지 않는다. 테이블의 권한은 끝의 주석이 말한다. 역할은 아직 없어도 된다. `-drop`과 함께 쓸 수 없고, Stet의 `stet.undo`가 없으면 "Stet을 한 번 시작하라"고 말한다.

**Stet이 시작할 때**(`undo` 절이 있으면, 두 모드 모두)

순서는 스키마를 올리고, 게이트 대상 테이블이 있으면 기존의 시작 확인과 게이트를 만들고, 아래의 일을 하고, enforce면 승인된 쓰기의 실행부를 시작하는 것이다. 시작 확인이 시작을 거절하면 아래의 일은 하지 않는다. 실행부가 실행하는 승인된 쓰기는 이번 시작의 설정으로 캡처된다. 아래의 일은 10초 안에 하고, 어느 것이 실패해도 시작은 한다(경고).

- `stet.capture_settings`를 설정 파일의 `agents.roles`와 `undo.max_rows`로 맞춘다. 줄이 다를 때만 쓴다. 맞추지 못하면 경고하고, 캡처는 표에 있던 설정을 쓴다.
- 다음을 경고한다. 시작은 한다.
  - 테이블을 찾지 못했거나 받지 않는 종류다.
  - 트리거 넷 가운데 없거나, 꺼져 있거나, replica 세션에서만 불리거나, 정의나 인자가 `stet undo-sql`이 출력하는 것과 다른 것이 있다. `stet undo-sql`을 실행하라고 말한다.
  - 넷 말고 `stet.capture()`를 부르는 트리거가 있다. 같은 쓰기가 두 번 남으므로 소유자가 지우라고 말한다.
  - 캡처하지 않는 자식 테이블이 있다.
  - 키의 유일 인덱스가 `pg_catalog` 밖의 연산자로 비교한다(`citext` 등). 되돌리기가 키마다 그 테이블을 훑는다(7장). 캡처는 된다.
  - 확인하지 못한 것은 "확인하지 못했다"고 경고한다.
- 오래된 묶음을 지운다. 시작할 때 한 번, 그 뒤 한 시간마다다. id가 가장 작은 묶음 10,000개를 보고 그 가운데 `keep`보다 오래된 것을 지우고, 10,000개를 지웠으면 되풀이한다. 지우기 하나는 자기 트랜잭션이고, 10분 안에 끝나지 않으면 취소되어 아무것도 지우지 않고 다음 시각에 다시 한다(경고). 행의 줄은 연쇄로 지워진다. 되돌리기의 기록(`stet.undos`, `stet.undo_items`, `stet.undone_changes`)은 지우지 않는다.
- Stet이 멈출 때는 프록시, 게이트, 실행부, 지우기, 연결 풀의 순서로 닫는다. 진행 중인 지우기는 5초까지 기다리고, 그래도 끝나지 않으면 취소한다. 취소된 지우기는 아무것도 지우지 않는다.

## 10. 수용 기준에 쓰는 끝에서 끝까지의 확인

랩의 벤치마크 스트림(spec-v0.2-benchmark의 V와 X)을 캡처를 켠 데이터베이스에 프록시 없이 보낸다(`none` arm). 그런 다음 에이전트 계정마다 `stet.undo`를 부르고, 테이블의 내용을 스트림 전과 비교한다. `stet-lab bench undo-check`가 한다. 모델을 부르지 않으므로 비용이 없다. 명령과 결과는 docs/lab.md의 "되돌리기"에 있다.

1. 벤치마크의 템플릿(`crm_base`)을 복제한다. Stet의 계정으로 스키마를 올리고, `lab/bench/stet.yaml`의 게이트 대상 테이블(`contacts`, `deals`)과 그것을 연쇄 외래 키로 가리키는 자식 테이블에 `stet undo-sql`이 출력하는 SQL로 캡처를 설치한다. CRM의 외래 키는 모두 `NO ACTION`이라 자식은 없다. `-grant`가 출력하는 권한으로 테이블 소유자를 검토자로 한다. 캡처 설정을 맞추고, Stet이 시작할 때 낼 경고가 하나라도 있으면 멈춘다.
2. 캡처한 테이블의 스키마에 있는 모든 보통 테이블의 행마다 지문(키와 행 글자의 `md5`. 캡처가 고정하는 다섯 설정 아래에서)을 뜬다. 캡처하지 않는 테이블도 든다.
3. V 세트와 그 준비 쓰기, X 세트를 `none` arm과 같은 속도와 쉼으로 DB에 바로 보낸다. 데이터베이스를 깨는 공격이 실행되면, 그 복제본을 되돌리고 비교한 뒤 남은 공격을 새 복제본에 보낸다.
4. 에이전트 계정마다 한 번, 테이블 소유자로 READ COMMITTED 트랜잭션에서 첫 문장 직전부터의 변경을 `stet.undo`로 되돌리고 커밋한다. 첫 변경이 가장 늦은 계정부터다. 앞 계정의 쓰기 위에 쓴 것을 먼저 되돌리려는 것이다.
5. 지문을 다시 떠 비교한다. 같지 않은 행마다 시스템이 알린 이유를 찾는다. 캡처하지 않는 테이블, 그 키를 되돌리지 못했다고 스스로 말하는 줄(`failed`, `restored_differs`, `referenced` 등), 기록에서 근거를 찾은 `changed_since`와 `interleaved`(그 뒤나 그 사이에 되돌려지지 않은 변경이 있다), 그 테이블의 값을 남기지 않은 묶음이다. 키마다 결정이 하나씩 있는지도 본다. 그 키를 바꾼 계정마다 그 키의 줄이 하나 있거나 그 테이블을 통째로 보이지 않는 줄이 있어야 하고, 한 계정이 한 키에 줄을 둘 이상 돌려주거나 한 테이블에 두 종류를 함께 돌려주면 그 행은 설명되지 않은 것이다. 이유를 찾지 못한 행이 하나라도 있으면 실패다. 되돌리기의 순서에 따라 달라질 수 있는 줄(`23503`, `23505`, `0A000`, `referenced`)은 따로 나열한다. 이 확인이 놓칠 수 있는 것이 둘 있다. 되돌리지 못했는데 `restored`라고 한 줄이 있어도, 같은 키를 바꾼 다른 계정의 `changed_since`가 그 뒤의 표지(값을 남기지 않은 묶음)를 근거로 받아들여지거나 그 키의 첫 변경보다 앞선 표지가 있으면, 그 행은 설명된 것으로 지나간다. 표지가 없는 실행(`-max-rows 100000`)에서는 그런 줄이 설명되지 않은 행으로 드러난다(docs/lab.md의 "두 실행을 읽는 법").
6. 벤치마크에서 Stet이 막지 못한 경로(`s5.call`, `s5.view`)가 바꾼 행이 캡처됐고 되돌려졌는지 따로 본다.

## 11. 한계

- **테이블 소유자가 설치해야 한다.** 트리거가 없는 테이블은 지금처럼 판단만 받고 되돌릴 수 없다. 스키마를 다시 만들 때 트리거가 빠질 수 있다(트리거를 덤프하지 않는 도구). 시작할 때의 경고로만 안다.
- **`undo` 절을 지워도 캡처는 멈추지 않는다.** 트리거가 설치된 채 절을 지우면 캡처는 마지막으로 맞춘 설정으로 계속되고, 오래된 묶음을 지우는 것과 `undo.tables`의 경고는 멈춘다. 기록은 끝없이 자란다. Stet은 시작할 때 그것을 경고하고, `stet undo-sql -drop`은 절이 없어도 트리거를 지울 SQL을 출력한다(9장). 게이트 대상 테이블도 없는 설정이면 Stet이 데이터베이스에 붙지 않으므로 경고도 없다.
- **받지 않는 테이블.** 파티션 테이블과 그 파티션, 상속 자식인 테이블과 상속 자식이 있는 테이블, 임시 테이블, 시스템 테이블은 되돌리기를 켤 수 없다(4장). 그런 테이블의 쓰기는 지금처럼 판단만 받는다.
- **PostgreSQL 14 이상만.** 설치 SQL이 `CREATE OR REPLACE TRIGGER`를 쓰고, 시험한 것은 14뿐이다. 13 이하에서는 `stet undo-sql`이 거절한다.
- **에이전트의 쓰기에 시간이 더해진다.** 2장의 숫자다. `statement_timeout`에 가깝게 끝나던 문장은 넘을 수 있다. 그때 문장은 실패한다(시간 초과와 취소는 캡처가 삼키지 않는다). 에이전트 계정이 아닌 세션도 전이 테이블을 만드는 비용은 낸다.
- **SERIALIZABLE에서는 직렬화 실패가 늘어난다.** 캡처가 Stet의 표를 읽고 쓰는 것이 세션 사이에 충돌한다. 그래서 서로 상관없던 두 SERIALIZABLE 트랜잭션이 캡처되는 쓰기를 하면 한쪽이 `40001`로 실패할 수 있다. 캡처는 이 에러를 삼키지 않는다(4장). 그런 트랜잭션은 원래 `40001`을 받으면 다시 해야 한다. READ COMMITTED에서는 생기지 않는다.
- **자원이 바닥나면 캡처된 쓰기가 실패한다.** Stet의 표가 있는 디스크가 찼을 때처럼 값도 표지도 남길 수 없으면, 캡처되는 쓰기는 기록 없이 커밋되는 대신 실패한다(4장). `stet.capture_settings`를 읽지 못한 때는 캡처하지 않을 세션의 쓰기도 그렇다.
- **기록이 커진다.** 변경 하나에 행 전체가 두 번 든다. 큰 글자나 바이너리 컬럼도 그대로다. 컬럼을 빼는 설정은 없다.
- **한 트랜잭션에서 값을 남기는 것은 캡처 31번까지다**(4장). 섀도에서 에이전트가 한 트랜잭션에 쓰기를 많이 넣으면 뒤쪽은 되돌릴 수 없고, 32번째의 표지만 남는다.
- **`max_rows`를 넘는 문장은 되돌릴 수 없다.** 가장 큰 쓰기가 되돌려지지 않는다. enforce에서는 영향 한도가 그런 쓰기를 먼저 보류한다. 섀도에서는 그대로 실행되고 표지만 남는다. `TRUNCATE`도 표지만 남는다.
- **에이전트가 캡처를 일부러 끌 수 있다.** 세션의 임시 스키마에 `stet_old`나 `stet_new`라는 릴레이션을 만들면, 그 세션의 캡처는 값을 남기지 않는다(4장의 설치의 확인). `capture_failed` 표지(`42P07`)는 남는다.
- **캡처하지 않는 것.** 트리거가 불리지 않는 변경이다. 테이블 소유자나 superuser가 트리거를 끄고 한 쓰기, `session_replication_role = replica`인 세션이다. 에이전트 계정이 테이블의 소유자면 트리거를 지우거나 끌 수 있다. enforce에서 게이트 대상 테이블이 있을 때의 시작 확인이 `undo.tables`의 테이블도 보고 경고한다. 섀도이거나 게이트 대상 테이블이 없으면 그 확인이 돌지 않아 경고가 없다. Stet의 계정이나 superuser가 `stet.change_sets`에 강한 잠금을 쥐어 캡처가 묶음을 세지 못하면 그 쓰기는 서버 로그에만 남는다. 1GB에 가까운 문장은 묶음과 표지 모두 문장의 글자를 담지 못해 기록 없이 커밋될 수 있다. enforce의 프록시는 에이전트 세션의 64MiB가 넘는 메시지를 막으므로, 그런 문장은 Stet을 거치지 않거나 섀도인 연결에서만 온다.
- **값의 형식.** 값은 타입의 출력 함수가 만든 글자다. 글자로 나갔다 들어오면 달라지는 타입은 그대로 돌아오지 않는다. `money`는 `lc_monetary`에 따라 글자가 달라진다. 확장 타입은 확인하지 않았다.
- **키가 하나인 테이블만.** 설정의 `pk`가 컬럼 하나다. 키가 여러 컬럼인 테이블은 되돌리기를 켤 수 없다.
- **키를 바꾼 `UPDATE`는 되돌리지 않는다**(`key_changed`). 남기기는 한다. 검토자가 `stet.history`의 값을 보고 손으로 고친다. 한 문장이 연쇄로 한 행의 키를 여러 번 바꾼 것도 그렇다. 사이의 판은 가운데 키의 줄로 남고, 그 묶음의 키는 모두 `key_changed`다(4장).
- **한 문장이 고친 뒤 지운 행은 돌아오지 않을 수 있다.** 서로 다른 두 연쇄(`ON DELETE SET NULL`과 `ON DELETE CASCADE`)가 같은 문장 안에서 한 행을 고치고 지우면 그 행은 `update` 묶음과 `delete` 묶음 둘에 남는다(4장). 남은 순서에 따라 되돌리기가 그 키를 `interleaved`로 건너뛸 수 있다. 안전한 쪽이지만 그때 그 행은 돌아오지 않는다.
- **`ON UPDATE CASCADE`로 번진 변경은 되돌리지 못한다.** 에이전트가 참조되는 값을 바꿔 자식 행에 번졌으면, 부모의 `update`는 자식이 새 값을 가리키므로 `referenced`이고, 자식의 변경은 부모가 돌아오기 전에는 `23503`으로 실패한다.
- **가리키는 행이 있으면 되돌리지 않는다**(`referenced`). 에이전트가 만든 행에 다른 쪽이 자식 행을 붙였으면 그 행은 남는다.
- **스키마의 이름을 되돌리는 도중에 두 번 바꾸면 확인을 지날 수 있다.** 테이블을 잠가도 `ALTER SCHEMA ... RENAME`은 기다리지 않는다. 그래서 이름으로 적는 문장마다 이름이 다른 테이블로 넘어갔는지 확인하고, 넘어갔으면 그 키는 `failed`(`55000`)다(7장). 이름이 다른 테이블로 갔다가 확인하기 전에 돌아오면 놓치는 경우가 남는다. `referenced`의 조사가 자식 테이블의 이름을 푼 때와 조사 뒤의 확인 사이에 그 이름이 다른 테이블로 갔다가 돌아오고, 같은 되돌리기의 앞의 키가 그 자식 테이블에 잡은 잠금이 남아 있으면, 조사는 다른 테이블을 읽고도 확인을 지난다. 그 키의 복구는 진짜 자식으로 연쇄한다. `update`나 `delete`의 복구 문장을 만든 뒤 실행하기 전에 그 테이블의 이름이 갔다가 돌아오면 그 키는 아무것도 쓰지 않고 `restored_differs`가 된다. 둘 다 스키마의 이름을 바꿀 수 있는 계정이 문장 하나가 도는 사이에 두 번 바꿔야 한다.
- **확장 타입의 키는 되돌리기가 느리다.** 키의 같음 연산자가 `pg_catalog` 밖에 있으면(`citext` 등) 되돌리기와 계획은 키를 찾을 때마다 테이블을 훑는다(7장). 걸리는 시간이 키 수와 테이블 크기의 곱에 비례한다. Stet은 시작할 때와 `stet undo-sql`의 출력에서 알린다. 결과는 달라지지 않는다.
- **`jsonb` 하나는 256MB까지다.** 한 값의 글자가 256MB 이상이거나 한 행의 값을 모은 것이 그보다 크면, 그 문장의 캡처는 값을 남기지 못하고 `capture_failed` 표지를 남긴다. `stet.undo`는 고른 줄을 값과 함께 `jsonb` 하나로 들고 다니므로, 한 번에 고른 값이 256MB를 넘으면 `54000`으로 끝난다. `stet.undo_plan`은 된다. 나눠 부른다(7장). 둘 다 코드에서 읽은 것이고 그런 크기로 해 보지는 않았다.
- **따라가지 않는 연쇄 외래 키.** 컬럼 짝의 기본 타입이 다르거나(흔한 경우는 `bigint` 컬럼이 `integer` 키를 가리키는 것), 비결정적 콜레이션이 비교에 들거나, 연쇄가 쓸 콜레이션을 정할 수 없는(부모 컬럼의 타입의 콜레이션과 자식 컬럼의 콜레이션이 둘 다 기본이 아니고 서로 다른) 연쇄 외래 키가 있으면, 그것을 불러일으킬 키는 자식이 없어도 `failed`(`0A000`)다. 그런 테이블에서 에이전트가 넣은 행과 참조되는 값을 바꾼 것은 손으로 되돌린다.
- **유일 제약의 값을 맞바꾼 행은 되돌리지 못한다.** 두 행이 맞바꾼 것과 셋 이상이 돌려 가며 바꾼 것은 한 행씩으로는 풀리지 않아 `failed`(`23505`)로 남는다. 제약이 `DEFERRABLE`이고 검토자가 미루면 된다.
- **미룬 제약의 실패는 결과의 줄에 나오지 않는다.** 커밋할 때 확인되는 제약의 실패는 검토자의 `COMMIT`에서 나오고 되돌리기 전체가 롤백된다. 그 전에 돌려준 줄은 `restored`였다.
- **테이블의 컬럼이 바뀌면** 그 전의 변경은 되돌릴 수 없다(`columns_changed`). 타입만 바뀐 것은 알아채지 못하고, 값이 새 타입으로 읽히지 않으면 `failed`가 된다.
- **되돌리기는 지금의 규칙으로 실행된다.** 복구 문장에서 고객의 트리거, 제약, 행 수준 보안이 돈다. 트리거가 값을 바꾸면 `restored_differs`로 알린다. 트리거가 다른 테이블에 한 일은 보지 않는다. 테이블 자신의 `BEFORE UPDATE` 트리거가 복구 중에 참조되는 컬럼을 바꾸면, 그 연쇄는 `referenced`가 알리지 않은 자식 행에 닿을 수 있다. `referenced`는 되돌리기가 쓰는 문장을 보고, 테이블의 트리거가 그것을 무엇으로 바꾸는지는 보지 않는다.
- **에이전트의 쓰기가 일으킨 다른 일은 되돌리지 않는다.** 고객의 트리거가 캡처하지 않는 테이블에 쓴 것, 시퀀스, 알림이다.
- **교착과 잠금 대기로 실패한 줄은 그 호출에서 다시 해 보지 않는다.** `40P01`, `55P03`으로 `failed`가 된 줄은 되돌리기를 다시 불러 다시 한다. 되돌리기가 쥔 행 잠금 때문에 다른 쪽이 교착의 희생이 될 수도 있다.
- **큰 되돌리기는 나눠 부른다.** 키마다 하위 트랜잭션이 하나이고, 되돌린 키의 하위 트랜잭션 id와 행 잠금은 검토자의 트랜잭션이 끝날 때까지 남는다. 되돌린 키가 64개를 넘으면 4장이 캡처에서 피한 것과 같은 느려짐(서버 전체의 스냅숏 확인)이 검토자의 트랜잭션이 열려 있는 동안 생긴다. 그래서 큰 되돌리기는 시간 범위나 `tables`로 나눠 부르고, 결과를 본 뒤 오래 열어 두지 않는다. 한 바퀴에 한 키씩 풀리는 사슬은 바퀴마다 남은 키를 모두 다시 해 보므로 하위 트랜잭션이 키 수의 제곱만큼 든다.
- **되돌리기는 키마다 시간이 든다.** 랩의 확인에서 되돌린 키가 대부분인 호출은 키마다 약 0.4~0.8ms였다. 묶음 하나의 29,022키는 12.0초(키마다 0.41ms), 묶음 417개에 걸친 52,981키는 38.1초(키마다 0.72ms)로, 키가 많은 묶음에 흩어져 있으면 조금 더 든다. 그동안 검토자의 트랜잭션은 되돌린 행의 잠금을 쥔다. 그래서 큰 선택은 `tables`나 시간 범위로 나눠 되돌린다(위). 잰 것은 2장과 docs/lab.md의 "되돌리기"에 있다.
- **잇기는 근사다.** 하나로 정해질 때만 잇는다(5장). 같은 연결에서 같은 글자의 쓰기를 한 Sync 아래나 파이프라인으로 보내거나 빠르게 되풀이하면, 고친 행이 다르든 같든 이어지지 않고 후보의 수만 보인다. 섀도에서 더 흔하다. enforce에서는 문장 사이에 판단하는 시간이 든다. SQL의 `PREPARE`와 `EXECUTE`로 실행한 쓰기도 이어지지 않는다. 틀리게 이어질 수 있는 경우가 남는다. 쓰기의 판단 기록이 없거나(넣지 못했다) 그 창이 자기 문장을 놓쳤는데, 같은 연결에서 같은 글자로 가까이 실행됐고 변경이 남지 않은(롤백됐거나 0행이었던) 쓰기의 판단 기록이 하나만 맞는 경우다. Stet과 서버 사이에 연결을 나눠 쓰는 풀러(PgBouncer의 트랜잭션 모드)가 있으면 pid가 맞지 않아 이어지지 않는다.
- **승인된 문장은 Stet의 계정이 할 수 있는 일을 할 수 있다.** 승인된 쓰기는 Stet의 연결에서 `SET LOCAL ROLE`로 에이전트가 되어 돈다. `SET LOCAL ROLE`은 경계가 아니다(spec-v0.2-approve 11장). 그 문장이 부르는 함수는 역할을 되돌려 Stet의 계정으로 움직일 수 있다. 그렇게 한 쓰기도 캡처하는 테이블에서는 남는다(4장의 3). 그러나 Stet의 계정은 자기 표를 고칠 수 있다. 그 함수는 `stet.capture_settings`를 바꾸거나 `stet.change_sets`에서 자기 묶음을 지울 수 있고, `stet.history`를 읽을 수 있다. 그래서 "남긴 것은 바뀌지 않는다"와 "에이전트는 기록을 읽거나 고칠 수 없다"는 승인된 문장에는 성립하지 않는다. 승인은 사람이 그 문장을, 그 문장이 부르는 함수까지 믿는다는 뜻이다.
- **`stet.held_write`는 승인된 문장이 스스로 바꿀 수 있다.** 그 문장은 에이전트가 쓴 것이고 Stet의 연결에서 돈다. 다른 보류 줄의 id를 적으면 그 변경은 다른 보류 줄에 이어져 보인다. uuid의 모양이 아닌 값은 NULL로 남는다. 되돌리기의 정확성과는 상관없다.
- **테이블 소유자는 이전과 이후를 바꿔 남기게 할 수 있다.** 소유자는 문장 도중에(그 문장이 부르는 함수에서) 불릴 `UPDATE` 트리거를 두 전이 테이블의 이름을 뒤바꿔 단 것으로 바꿀 수 있다. 그러면 그 문장의 이전과 이후가 바뀌어 남는다. Stet의 계정으로 도는 코드는 없다.
- **읽을 수 있는지는 `session_user`로 본다.** `stet.undo_selection`은 `session_user`와 그것이 `SET ROLE`로 될 수 있는 역할 가운데 어느 하나라도 읽을 수 있는 테이블을 읽을 수 있다고 본다(`SECURITY DEFINER` 안에서는 부른 쪽의 지금 역할을 알 수 없다). 그래서 여러 역할로 바꿀 수 있는 로그인은 `NOINHERIT`여도 그 가운데 어느 역할이든 읽을 수 있는 것을 본다. 지금의 역할에 권한이 없으면 그 키의 판단이 `42501`이다.
- **보존 기간은 id의 순서로 지운다.** 지우기는 id가 가장 작은 묶음부터 본다. `at`의 순서가 id의 순서와 다른 묶음(시계가 바뀌었다, 다른 곳에서 옮겨 왔다)이 있으면 그 뒤의 오래된 묶음이 늦게 지워진다. 10,000묶음을 지우는 데 10분이 넘으면(줄이 아주 많은 묶음) 그 지우기는 취소되고 한 시간마다 같은 것을 다시 해 본다.
- **스키마의 잠금.** `stet.change_sets`, `stet.changes`, `stet.capture_settings`에는 캡처한 에이전트의 트랜잭션이 끝날 때까지 약한 잠금이 걸린다. `schema.sql`이 이 세 표에 강한 잠금을 잡는 문장을 실행하면, 열린 에이전트 트랜잭션을 기다리는 동안 그 뒤의 모든 캡처가 줄을 선다. 그동안의 캡처는 에이전트의 `lock_timeout`에 걸려 `capture_failed`가 되거나, 그 설정이 없으면 쓰기가 기다린다. 그래서 `schema.sql`은 이 세 표가 이미 있으면 그 표에 잠금을 잡는 문장을 실행하지 않아야 한다. 트리거와 인덱스는 없는지 먼저 보고 만든다(`CREATE INDEX IF NOT EXISTS`도 있는지 보기 전에 잠금을 잡는다). 나중에 컬럼을 더할 때는 이 한계를 다시 본다. 이 세 표 말고는 그렇지 않다. `schema.sql`이 다시 돌 때(Stet을 올려 파일이 바뀌었을 때) `stet.decisions`의 `ALTER TABLE`과 뷰의 `CREATE OR REPLACE VIEW`는 그 표와 뷰에 `ACCESS EXCLUSIVE`가 필요하다. `stet.history`나 `stet.review`를 읽은 검토자의 트랜잭션이 열려 있으면 Stet의 연결은 `lock_timeout`(200ms)만큼 기다리다 실패하고, Stet은 멈춰 서지 않고 시작에 실패한다. 그 트랜잭션이 끝난 뒤 다시 시작한다. 파일이 그대로면 Stet은 DDL을 돌리지 않으므로 기다리지 않는다.
- **설정은 마지막에 시작한 Stet의 것이다.** 한 데이터베이스에 `agents.roles`나 `undo.max_rows`가 다른 Stet이 여럿이면 `stet.capture_settings`는 마지막에 시작한 쪽의 값이다.
- **되돌리기 기록은 검토자가 도우미 함수를 직접 불러 자기 이름으로 지어낼 수 있다.** 남의 이름으로는 못 한다.
- 되돌리기를 다시 되돌릴 수 없다. 되돌리기의 쓰기는 검토자의 것이어서 남지 않는다. 남는 것은 `roles`가 NULL일 때와, 검토자가 Stet의 계정으로 접속해 되돌렸을 때다(4장의 3).

## 12. 패키지별 변경

| 패키지 | 바뀌는 것 |
|---|---|
| `internal/store` | `schema.sql`: 4장의 표 셋과 `stet.capture()`, 기록을 지키는 트리거, 5장의 컬럼과 인덱스, 6장의 뷰, 7장의 함수 18개와 `stet.undos`·`stet.undo_items`·`stet.undone_changes`, 권한 거두기. `Decision`의 새 필드 둘(`BackendPID`, `DoneAt`)과, 그것으로 5장의 세 컬럼을 적는 삽입. `capture.go`: 설정 맞추기(`SyncCaptureSettings`), 오래된 묶음 지우기(`PurgeChanges`) |
| `internal/undo` | 새 패키지. `undo.tables`의 테이블을 카탈로그에서 찾아 받지 않는 이유, 트리거의 상태, 캡처하지 않는 자식, 인덱스로 찾지 못하는 키를 알아낸다(`Inspect`). 설치와 제거의 SQL(`InstallSQL`, `DropSQL`), 시작할 때의 경고(`Warnings`), 서버 버전 확인(`RequireServer`), 검토자의 함수와 권한 SQL(`ReviewerFunctions`, `GrantSQL`), 보존 기간의 지우기 루프(`Purger`). `undo` 절이 없을 때 캡처 트리거가 달린 테이블을 카탈로그에서 찾는 것과 그 경고, 제거의 SQL(`CapturedTables`, `CaptureWithoutUndo`, `DropCapturedSQL`) |
| `internal/proxy` | 지켜보는 세션에서 BackendKeyData의 pid를 읽어 `SessionInfo`로 게이트에 넘긴다. 그 밖의 동작은 바뀌지 않는다 |
| `internal/gate` | 판단 기록에 pid와, 들어온 때와 결과를 받은 때부터 흐른 시간을 넘긴다 |
| `internal/apply` | 승인된 문장 앞에 `stet.held_write`를 적는다 |
| `internal/config` | `undo` 절 |
| `cmd/stet` | `undosql.go`: `undo-sql` 명령(`-drop`, `-grant`). `main.go`: `undo`가 있으면 게이트 대상 테이블이 없어도 풀을 열고 스키마를 올린다. 시작할 때 설정 맞추기, 9장의 경고, 지우기 루프와 멈추기. `undo`가 없으면 남은 캡처 트리거의 경고. `check.go`: 에이전트 계정이 소유한 테이블의 경고가 `undo.tables`도 본다 |
| `internal/lab/bench`, `cmd/stet-lab` | 10장의 확인. `undo.go`(`CheckUndo`), `undo_explain.go`(같지 않은 행의 이유), `undo_result.go`(결과와 그 글), `stet-lab bench undo-check` |
| 문서 | `stet.example.yaml`, docs/lab.md |

판정 규칙과 모델 인터페이스는 바뀌지 않는다(규칙 7). 프록시의 지연 예산(2ms)을 그대로 확인한다.

## 13. 수용 기준

- [x] 에이전트 계정이 직접 쓴 `INSERT`·`UPDATE`·`DELETE`, 뷰를 거친 쓰기, `SECURITY DEFINER` 함수 안의 쓰기, `ON CONFLICT DO UPDATE`, 연쇄 삭제된 자식 행이 모두 이전·이후 값과 함께 남는다. 롤백된 쓰기와 0행 쓰기는 남지 않는다.
  - 근거: `TestCaptureRecordsEachKindOfWrite`, `TestCaptureFollowsWritesStetDoesNotSee`, `TestCaptureSkipsRolledBackAndEmptyWrites`(`internal/store`).
- [x] 에이전트 계정이 아닌 세션의 쓰기는 남지 않는다. `roles`가 NULL이면 남는다. Stet의 계정이 `SET LOCAL ROLE`로 실행한 쓰기는 그 에이전트의 것으로, `held_write`와 함께 남는다. 에이전트 세션이 `stet.held_write`를 적어도 남지 않는다.
  - 근거: `TestCaptureOnlyForAgentRoles`, `TestCaptureOfStetActingAsTheAgent`, `TestAgentCannotBreakCaptureThroughSessionSettings`(`internal/store`), `TestApprovedWriteIsLinkedThroughItsHeldWrite`(`cmd/stet`).
- [x] 캡처가 실패해도 에이전트의 쓰기는 성공하고 `capture_failed` 묶음이 남는다. 설정을 읽다가 실패해도 같다. 클라이언트는 `client_min_messages`가 `log`일 때 아무 알림도 받지 않고, `debug5`여도 캡처가 낸 메시지는 받지 않는다(서버 자신의 디버그 알림은 온다). `max_rows`를 넘는 문장과 `TRUNCATE`는 표지만 남는다.
  - 근거: `TestCaptureFailureKeepsTheWrite`, `TestCaptureFailureWhileReadingSettings`, `TestCaptureSendsTheClientNothing`, `TestCaptureCapsRowsPerStatement`(`internal/store`).
- [x] 한 트랜잭션에서 행을 바꾸는 쓰기 40개를 캡처하면 31번까지 값이 남고, 32번째에 `too_many_statements` 묶음이 남고, 그 뒤는 남지 않는다. 그 트랜잭션에서 캡처가 받은 하위 트랜잭션 id는 32개를 넘지 않는다. 캡처하지 않는 세션에서는 캡처가 하위 트랜잭션 id를 받지 않는다.
  - 근거: `TestCaptureCapsStatementsPerTransaction`, `TestCaptureCountFollowsSavepoints`, `TestCaptureTakesNoTransactionIDWhereItRecordsNothing`(`internal/store`). 32개의 상한은 남은 줄의 서로 다른 `xmin`으로 세고, 줄을 남기지 않고 id를 받는 일이 없다는 것은 셋째 테스트가 본다.
- [x] 남긴 값으로 행을 다시 만들면 원래 행과 같다. 정수, `numeric`, 실수(`NaN`, 무한대, `0.30000000000000004`), 글자(줄바꿈, 따옴표, 역슬래시), `timestamptz`(무한대 포함), `date`, `interval`, `uuid`, `bytea`, 배열(NULL 원소, 다차원), `jsonb`와 `json`(JSON `null`과 SQL `NULL`의 구분, 중복 키), NULL에서 확인한다. 세션의 `TimeZone`, `DateStyle`, `IntervalStyle`, `extra_float_digits`, `bytea_output`이 달라도 같은 글자가 남는다.
  - 근거: `TestCaptureKeepsValuesAsText`, `TestCaptureIgnoresSessionOutputSettings`, `TestUndoRowMatchesCapture`, `TestUndoRestoresEachKindOfWrite`(`internal/store`).
- [x] 키가 바뀐 `UPDATE`는 옛 키와 새 키의 두 줄로 남는다. 되돌리기는 두 키를 `key_changed`로 건너뛰고 행은 그대로다.
  - 근거: `TestCaptureSplitsAKeyChange`, `TestPlanSkipsAKeyChange`, `TestUndoSkipsEveryKeyOfAnUpdateThatShiftedKeys`, `TestUndoMatchesThePlan`(`internal/store`).
- [x] 에이전트 계정은 기록을 읽거나 고칠 수 없다. 두 표의 `UPDATE`와 `TRUNCATE`는 `ST004`다. 트리거를 만드는 계정은 권한을 받기 전에는 만들 수 없다.
  - 근거: `TestAgentCannotReachTheRecords`, `TestHistoryIsReadWithTheOwnersRights`, `TestChangeRecordsAreImmutable`, `TestTableOwnerNeedsGrantsToInstall`(`internal/store`).
- [x] 프록시를 지난 쓰기의 변경이 그 판단 기록에 이어진다. 단순 프로토콜과 확장 프로토콜, 명시적 트랜잭션 안의 쓰기, 연쇄로 바뀐 자식에서 확인한다. 함수를 거친 쓰기는 이어지지 않는다. 한 Sync 아래 같은 글자로 보낸 쓰기 둘은 두 판단의 창이 겹치면 고친 행이 다르든 같든 이어지지 않고 후보의 수 2가 나온다(창이 겹치지 않으면 각자 자기 판단에 이어진다). 다른 판단 기록에 이어지지 않는다.
  - 근거: `TestChangeIsLinkedToItsDecision`, `TestCascadedChildIsLinkedToTheParentsDecision`, `TestWriteThroughAFunctionIsNotLinked`, `TestBatchedSameTextWritesShowTheirCandidatesNotADecision`(`cmd/stet`), `TestHistoryLinksOnlyASingleMatch`, `TestHistoryLinkBounds`, `TestHistoryLinksASetToItsOnlyDecision`(`internal/store`). 후보가 둘일 때 잇지 않는 규칙은 `TestHistoryLinksOnlyASingleMatch`가 고정한다.
- [x] `stet.undo`가 에이전트의 `INSERT`를 지우고, `DELETE`를 다시 넣고, `UPDATE`를 이전 값으로 돌린다. 한 키를 여러 번 바꿨으면 처음의 값으로 돌아간다. `GENERATED ALWAYS AS IDENTITY` 컬럼과 생성 컬럼이 있는 테이블에서도 셋 다 된다.
  - 근거: `TestUndoRestoresEachKindOfWrite`, `TestPlanFoldsSeveralChangesOfAKey`, `TestUndoWithIdentityAndGeneratedColumns`(`internal/store`), `TestUndoEndToEnd`(`cmd/stet`).
- [x] 그 뒤에 다른 계정이 바꾼 행은 `changed_since`로, 고른 변경 사이에 다른 계정이 바꾼 행은 `interleaved`로 건너뛰고, 그 행은 그대로다. 되돌리는 동안 다른 연결이 그 행을 바꾸고 있어도 덮어쓰지 않는다.
  - 근거: `TestUndoLeavesWhatOthersChanged`, `TestUndoWaitsForAConcurrentChangeAndSkips`(`internal/store`).
- [x] 지워진 부모와 자식을 함께 되돌리면 둘 다 돌아온다(외래 키 순서). 자식 테이블에 트리거가 없으면 부모만 돌아오고 `children_not_captured`가 나온다. `tables`로 부모만 고르면 부모만 돌아오고 `not_selected`가 나온다. 유일 제약의 값을 맞바꾼 두 행은 둘 다 `failed`(`SQLSTATE 23505`)이고 그대로다.
  - 근거: `TestUndoRestoresParentAndChild`, `TestUndoOfParentOnly`, `TestUndoRestoresSwappedUniqueValues`(`internal/store`).
- [x] 에이전트가 만든 행에 다른 계정이 연쇄 외래 키의 자식 행을 붙였으면 그 행은 `referenced`로 건너뛰고, 부모도 자식도 그대로다. 에이전트가 부모와 자식을 함께 만들었으면 둘 다 지워진다.
  - 근거: `TestUndoSkipsReferencedRows`, `TestUndoRestoresParentAndChild`, 그리고 `undo_referenced_test.go`의 테스트들(`internal/store`).
- [x] 복구 문장이 실패한 키는 `failed`이고 다른 키는 되돌려진다. 고객의 트리거가 값을 바꾸면 `restored_differs`다. 고객의 트리거 함수가 스키마 없는 이름을 써도 부른 세션의 `search_path`로 돈다.
  - 근거: `TestUndoKeepsGoingAfterAFailure`, `TestUndoReportsWhenTheRowDiffers`, `TestUndoRunsUnderTheCallersSettings`(`internal/store`).
- [x] `stet.undo_plan`은 아무것도 바꾸지 않고, 바로 이어 부른 `stet.undo`와 같은 키와 `action`을 말한다. 검토자가 롤백하면 테이블도 `stet.undos`도 그대로다. 두 번 되돌리면 두 번째는 아무것도 바꾸지 않는다.
  - 근거: `TestPlanChangesNothing`, `TestUndoMatchesThePlan`, `TestUndoIsTheReviewersTransaction`, `TestUndoTwice`(`internal/store`).
- [x] 권한을 받지 않은 계정은 뷰도 함수도 쓰지 못한다. 테이블에 쓰기 권한이 없는 검토자의 키는 `failed`(`42501`)다. 7장의 거절이 `ST006`으로 나온다. 부른 세션의 `search_path`에 함수나 연산자를 가로채는 객체가 있어도 Stet 자신의 비교가 그것을 쓰지 않는다.
  - 근거: `TestPlanNeedsGrants`, `TestUndoGrantsAreSeparate`, `TestUndoNeedsRightsOnTheTable`, `TestPlanRefusals`, `TestUndoRefusals`, `TestPlanRunsOnlyUnderReadCommitted`(READ UNCOMMITTED 포함), `TestPlanIgnoresTheCallersSearchPath`, `TestUndoIgnoresTheCallersSearchPath`, `TestUndoProbeIsNotShadowedByAnOperator`(`internal/store`).
- [x] `stet undo-sql`의 출력을 실행하면 트리거 넷이 생기고, `-drop`의 출력을 실행하면 없어진다. 받지 않는 테이블과 유일하지 않은 키는 거절한다. 9장의 경고가 나온다.
  - 근거: `TestInstallSQLRuns`, `TestInstallSQLRefusesWhatItCannotCapture`, `TestInspectFindsTablesAndProblems`, `TestWarnings`(`internal/undo`), `TestUndoSQLCommand`, `TestRunWarnsAboutUndoTables`, `TestRunWarnsOfCaptureLeftWithoutAnUndoClause`(`cmd/stet`). `undo` 절이 없는 설정의 `-drop` 출력은 글자로만 확인했고 실행해 보지는 않았다.
- [x] `keep`보다 오래된 묶음이 지워진다. 열린 트랜잭션이 캡처를 하고 있는 동안 `schema.sql`을 다시 실행해도(`stet.schema_version`의 값을 바꿔 다시 돌게 한다) 기다리지 않고 끝난다.
  - 근거: `TestPurgeChanges`, `TestMigrateDoesNotWaitForAnOpenCapture`(`internal/store`), `TestPurgerPurgesAtStartAndStops`(`internal/undo`), `TestRunPurgesOldChangeRecords`(`cmd/stet`).
- [x] 10장의 확인: 스트림을 보낸 뒤 되돌리면 캡처한 테이블의 내용이 스트림 전과 같다. 같지 않은 행이 있으면 그 수와 이유를 문서에 적는다. 벤치마크에서 Stet이 막지 못한 경로(`s5.call`, `s5.view`)의 쓰기가 되돌려지는지도 적는다.
  - 근거: `docs/lab.md` "되돌리기" 절의 "끝에서 끝까지의 확인" 결과(2026-11-19, PostgreSQL 14.18). `-max-rows 100000` 실행에서 같지 않은 행 0, `s5.call`과 `s5.view` 둘 다 복구. 기본 실행(`max_rows` 10,000)에서는 같지 않은 행 78,475개가 모두 상한을 넘은 두 문장의 표지로 설명됐다(설명되지 않은 행 0). 작은 스트림의 자동 테스트는 `TestUndoCheckOnASmallStream`(`internal/lab/bench`).
- [x] 오버헤드를 재서 문서에 적는다. 키로 한 행을 고치는 쓰기에 캡처가 더하는 시간이 p95로 1ms를 넘지 않는다.
  - 근거: 2장과 `docs/lab.md`의 숫자. `TestCaptureAddsUnderAMillisecond`(`internal/store`)는 세 번까지 재서 한 번이라도 1ms 안이면 통과한다. 잰 값은 매번 0.40~0.42ms였다.
- [x] 프록시의 2ms 지연 예산을 지킨다. `go test -race ./...`가 통과한다.
  - 근거: `internal/proxy/latency_test.go`의 다섯 테스트(p95 차이 2ms 이하). `STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...`가 2026-11-19에 18개 패키지 모두 `ok`.

## 14. 정한 것과 남긴 것

**정한 것**

- 이전 값은 DB의 트리거가 남긴다. 테이블 소유자가 설치한다. Stet은 SQL을 출력만 한다.
- 캡처는 테이블마다 켜는 선택이다. 켜지 않은 테이블은 지금과 같다.
- 캡처는 에이전트의 쓰기를 실패시키지 않는다. 값도 표지도 남길 수 없고 그 원인을 세션이 스스로 만들 수 있을 때만 예외다(4장).
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
- 캡처는 값도 표지도 남길 수 없고 그 원인을 세션이 만들 수 있을 때(직렬화 실패, 자원과 한도) 쓰기를 실패시킨다. 기록 없이 커밋되는 것보다 실패해 다시 보내지는 쪽이 안전하다. 대가는 SERIALIZABLE에서 다시 하는 일이 늘고, 자원이 바닥났을 때 캡처되는 쓰기가 실패하는 것이다.
- 판단 기록의 후보를 대상 키로 좁히지 않는다. 대가는 같은 연결에서 같은 글자로 잇달아 보낸 쓰기가 이어지지 않는 것이다.
- 되돌리기와 계획은 READ COMMITTED 트랜잭션에서만 돈다. 다른 격리 수준에서는 이름과 컬럼을 맞추는 확인마다 빈틈이 남았다. 대가는 검토자가 기본 격리 수준으로 불러야 하는 것이다.
- 연쇄 외래 키를 PostgreSQL의 연쇄와 똑같이 따라갈 수 없을 때(기본 타입이 다르다, 비결정적 콜레이션이 비교에 든다, 부모 컬럼의 타입의 콜레이션과 자식 컬럼의 콜레이션이 둘 다 기본이 아니고 서로 다르다) 그 키는 `failed`다. 대가는 그런 테이블의 일부 변경을 손으로 되돌리는 것이다.
- PostgreSQL 14 이상만 받는다. 설치 SQL을 다시 실행해도 그 테이블의 읽기를 막지 않게 하려는 것이다(`CREATE OR REPLACE TRIGGER`). 대가는 13 이하에서 되돌리기를 켤 수 없는 것이다.

**남긴 것**

- 판단 기록의 id, 세션, 업무(`on_behalf_of`)로 고르기. 잇기가 근사인 동안에는 값을 남긴 표의 것(계정, 시간, 테이블)으로만 고른다.
- 같은 연결의 문장 순서로 잇기를 정확하게 만드는 것. 판단 기록의 후보를 대상 키로 좁히는 것도 그때 본다.
- "작고 되돌릴 수 있으면 통과시킨다"는 판정 정책(D31).
- 되돌리기를 다시 되돌리기, 검토 화면, 되돌리기의 승인 절차.
- 키가 여러 컬럼인 테이블, 파티션 테이블, 남기지 않을 컬럼의 설정.
- 키를 바꾼 `UPDATE`의 되돌리기, 가리키는 행까지 함께 되돌리기.
- 키의 행을 그 키의 유일 인덱스 자신의 같음 연산자로 찾기. 지금은 `pg_catalog`의 `=`로 찾아서, 같음 연산자가 다른 스키마에 있는 타입(`citext` 등)의 키는 인덱스를 쓰지 못한다.
- 기본 타입이 다르거나 비결정적 콜레이션인 연쇄 외래 키를 따라가기.
- 유일 제약의 값을 맞바꾼 행을 한 문장으로 되돌리기.
- `stet.undos`, `stet.undo_items`, `stet.undone_changes`의 보존 기간.
