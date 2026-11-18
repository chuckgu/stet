-- Stet의 스키마. 판단 기록은 docs/spec-v0.1.md 8장.
-- Store.Migrate는 이 파일이 데이터베이스가 마지막으로 실행한 것과 다를 때만 실행한다. 같은지는 파일의 SHA-256을
-- stet.schema_version과 비교해 안다. 같으면 DDL을 하나도 돌리지 않으므로, Stet의 테이블에 열린 트랜잭션이 있어도
-- Stet의 시작이 잠금을 기다리지 않는다. 손으로 지운 객체는 이 파일이 바뀌기 전에는 다시 만들어지지 않는다.
-- 이미 있는 스키마 위에서도 돌므로 여러 번 실행해도 안전해야 한다.

CREATE SCHEMA IF NOT EXISTS stet;

-- Store.Migrate가 마지막으로 실행한 이 파일의 SHA-256 (hex). 한 줄뿐이다.
CREATE TABLE IF NOT EXISTS stet.schema_version (
  one     boolean PRIMARY KEY DEFAULT true CHECK (one),
  sha256  text NOT NULL
);

CREATE TABLE IF NOT EXISTS stet.decisions (
  id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  created_at       timestamptz NOT NULL DEFAULT now(),   -- 쓰기가 들어온 시각
  mode             text NOT NULL CHECK (mode IN ('shadow','enforce')),

  -- 누가
  agent_id         text NOT NULL,
  on_behalf_of     text,
  session_id       text,

  -- 무엇을
  statement        text NOT NULL,
  statement_fp     text NOT NULL,                       -- 상수를 뺀 문장 지문
  params           jsonb,                               -- 바인딩 값(텍스트 배열). 쓰기 스트림 재생용
  op               text NOT NULL
                   CHECK (op IN ('insert','update','delete','merge','copy','truncate')),
  target_table     text NOT NULL,                       -- 설정 파일의 테이블 키
  target_pk        jsonb,
  proposed         jsonb,                               -- 에이전트가 쓰려는 값 전체 (send_fields로 거르기 전)

  -- 무엇을 보고 (판단하지 않은 쓰기는 NULL)
  state            jsonb,                               -- 모델에 보낸 상태 그대로
  state_version    jsonb,                               -- 대상 행 xmin 등

  -- 어떻게 판단했나 (모델을 부르지 않았으면 questions·answers는 NULL)
  model            text NOT NULL,
  model_version    text NOT NULL,
  questions        jsonb,
  answers          jsonb,
  policy_version   text NOT NULL,                       -- 설정 파일 해시
  verdict          text NOT NULL
                   CHECK (verdict IN ('commit','hold','escalate','reject','unsupported')),
  reasons          text[] NOT NULL DEFAULT '{}',

  -- 무엇이 일어났나
  executed         boolean NOT NULL,
  exec_error       text,
  latency_ms       integer,                             -- 프록시가 클라이언트 경로에 더한 지연
  model_latency_ms integer,
  cost_usd         numeric(12,6),

  -- 나중에 붙는 정답
  label            text CHECK (label IN ('correct','false_hold','missed','unclear')),
  label_by         text,
  labeled_at       timestamptz
);

-- 다중 행 쓰기의 영향 측정 (docs/spec-v0.2-impact.md 6장). 측정하지 않은 쓰기는 NULL.
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS impact jsonb;

-- 누적 한도 (docs/spec-v0.2-accounting.md 3·4장). 모르는 값은 NULL.
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS login_role text;       -- DB가 인증한 접속 계정
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS rows_affected bigint;  -- 쓰기가 게이트 대상 테이블에서 바꾼 행 수
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS cumulative jsonb;      -- 누적 한도 판단 근거
-- enforce (docs/spec-v0.2-enforce.md 11장). Stet이 문장을 보내지 않고 돌려준 SQLSTATE. 보냈으면 NULL. 입력 전용 컬럼이다.
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS refused text;
CREATE INDEX IF NOT EXISTS decisions_role_idx ON stet.decisions (login_role, target_table, created_at DESC);
-- 되돌리기의 변경을 판단 기록에 잇는다 (docs/spec-v0.2-undo.md 5장). 셋 다 줄을 넣을 때만 적는다(규칙 3).
-- 서버로 보내지 않은 쓰기와 pid를 모르는 세션의 쓰기는 NULL이다. 두 시각은 DB의 시계다. Stet은 자기 시계의 시각을
-- 보내지 않고, 줄을 넣는 순간까지 흐른 시간을 보내 transaction_timestamp()에서 뺀다. 넣는 문장의 트랜잭션은 그 문장의
-- 메시지가 서버에 닿을 때 시작하므로, 그 뒤에 이 표의 잠금을 기다려도 시각이 늦어지지 않는다(계획의 판정 U12).
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS backend_pid integer;      -- 그 세션의 서버 쪽 백엔드 pid
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS sent_after  timestamptz;  -- DB 시계. 문장은 이보다 뒤에 서버로 갔다
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS done_before timestamptz;  -- DB 시계. 문장의 결과는 이보다 앞에 나왔다
-- stet.history가 묶음마다 후보를 찾는 인덱스. 창의 두 끝이 모두 인덱스 안에 있어 힙을 읽지 않고 거른다. done_before가
-- 앞이므로 묶음의 stmt_at 이후로 끝난 것만 훑는다. 검토와 되돌리기는 대개 최근의 변경을 읽는다. 앞의 단계에서 만든
-- 두 컬럼의 decisions_backend_idx는 지운다. 같은 이름으로는 IF NOT EXISTS가 옛 인덱스를 그대로 두기 때문에 이름을 바꿨다.
DROP INDEX IF EXISTS stet.decisions_backend_idx;
CREATE INDEX IF NOT EXISTS decisions_window_idx ON stet.decisions (backend_pid, done_before, sent_after);

CREATE INDEX IF NOT EXISTS decisions_target_idx ON stet.decisions (target_table, target_pk, created_at DESC);
CREATE INDEX IF NOT EXISTS decisions_verdict_idx ON stet.decisions (verdict, created_at DESC);
CREATE INDEX IF NOT EXISTS decisions_agent_idx ON stet.decisions (agent_id, created_at DESC);

-- 규칙 3: append-only. 사후 라벨 컬럼만 바꿀 수 있다. 역할 권한과 별개로 DB가 직접 막는다.
-- search_path를 고정한다. 트리거를 일으킨 세션의 search_path에 행 타입에 꼭 맞는 to_jsonb 같은 함수가 있으면
-- pg_catalog의 것 대신 그것이 불려 무엇이든 통과시킬 수 있다. 아래의 held_writes_guard도 같다.
CREATE OR REPLACE FUNCTION stet.decisions_append_only() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  IF TG_OP = 'UPDATE'
     AND (to_jsonb(NEW) - '{label,label_by,labeled_at}'::text[])
         = (to_jsonb(OLD) - '{label,label_by,labeled_at}'::text[]) THEN
    RETURN NEW;
  END IF;
  RAISE EXCEPTION 'stet.decisions is append-only: only label, label_by, labeled_at may change'
    USING ERRCODE = 'ST004';
END
$$;

DROP TRIGGER IF EXISTS decisions_append_only ON stet.decisions;
CREATE TRIGGER decisions_append_only
  BEFORE UPDATE OR DELETE ON stet.decisions
  FOR EACH ROW EXECUTE FUNCTION stet.decisions_append_only();

DROP TRIGGER IF EXISTS decisions_no_truncate ON stet.decisions;
CREATE TRIGGER decisions_no_truncate
  BEFORE TRUNCATE ON stet.decisions
  FOR EACH STATEMENT EXECUTE FUNCTION stet.decisions_append_only();

-- 읽기 집계 (docs/spec-v0.2-accounting.md 5장). 추가만 하는 장부가 아니므로 트리거를 두지 않는다.
CREATE TABLE IF NOT EXISTS stet.reads (
  bucket        timestamptz NOT NULL,          -- 분의 시작 시각
  login_role    text NOT NULL,                 -- 모르면 ''
  target_table  text NOT NULL,                 -- 설정의 테이블 키
  statements    bigint NOT NULL,
  rows          bigint NOT NULL,               -- 클라이언트에게 나간 행 수
  max_rows      bigint NOT NULL DEFAULT 0,     -- 한 읽기가 내보낸 가장 큰 행 수. 커서와 포털은 그때까지의 합계
  PRIMARY KEY (bucket, login_role, target_table)
);

-- 큰 읽기 기록
CREATE TABLE IF NOT EXISTS stet.read_log (
  id            uuid PRIMARY KEY,
  created_at    timestamptz NOT NULL,          -- 문장이 들어온 시각
  login_role    text,
  agent_id      text NOT NULL,                 -- application_name. 스스로 적는 값
  on_behalf_of  text,
  session_id    text,
  statement     text NOT NULL,
  statement_fp  text NOT NULL,
  target_tables text[] NOT NULL,               -- 읽은 게이트 대상 테이블의 설정 키
  rows          bigint NOT NULL,               -- 클라이언트에게 나간 행 수
  exec_error    text                           -- 실패하거나 끊긴 문장이면 그 원인
);

-- 보류 건 (docs/spec-v0.2-enforce.md 9장, docs/spec-v0.2-approve.md 4장). hold와 escalate로 막은 쓰기.
-- 내용은 바뀌지 않고 status는 정해진 화살표로만 바뀐다. 아래의 트리거가 막는다.
CREATE TABLE IF NOT EXISTS stet.held_writes (
  id            uuid PRIMARY KEY,
  decision_id   uuid NOT NULL,                 -- stet.decisions의 id
  created_at    timestamptz NOT NULL,          -- 문장이 들어온 시각
  status        text NOT NULL DEFAULT 'pending',  -- 처음 값은 아래에서 unconfirmed로 바꾼다. 상태는 spec-v0.2-approve 3장
  login_role    text NOT NULL,
  database      text NOT NULL,
  statement     text NOT NULL,
  params        jsonb,                         -- 바인딩 값의 텍스트 배열
  param_types   oid[],                         -- 클라이언트가 Parse에서 준 타입
  target_table  text NOT NULL,
  target_pk     text,
  row_version   text,                          -- 상태를 읽을 때의 xmin. 단일 행 UPDATE·DELETE만
  verdict       text NOT NULL,                 -- hold | escalate
  reasons       text[] NOT NULL
);
CREATE INDEX IF NOT EXISTS held_writes_pending ON stet.held_writes (created_at) WHERE status = 'pending';

-- 승인과 거부 (docs/spec-v0.2-approve.md 4장)
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

-- 보류된 것은 바뀌지 않는다 (docs/spec-v0.2-approve.md 4장, 계획의 보정 B4). 역할 권한과 별개로 DB가 직접 막는다.
-- free의 아홉 컬럼만 바뀔 수 있고, 나머지는 나중에 더해지는 컬럼까지 모두 고정이다. 아홉도 저마다 바뀌는 때가 정해져 있다.
-- in_transaction은 unconfirmed에서 pending이 되는 그 갱신에서만 바뀐다. status는 3장의 화살표로만 바뀐다.
-- resolved_by, resolved_at, note, max_rows는 pending에서 approved나 rejected가 되는 그 갱신에서만,
-- applied_at, rows_affected, error는 approved에서 applied나 failed가 되는 그 갱신에서만 바뀐다 (계획의 판정 12).
-- 그래서 승인된 줄의 max_rows를 넓히거나 resolved_at을 늦춰 10분을 다시 세게 할 수 없다. Stet의 계정도 마찬가지다.
-- DELETE는 막지 않는다.
CREATE OR REPLACE FUNCTION stet.held_writes_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  free CONSTANT text[] := '{status,in_transaction,resolved_by,resolved_at,note,max_rows,applied_at,rows_affected,error}';
BEGIN
  IF to_jsonb(NEW) - free <> to_jsonb(OLD) - free THEN
    RAISE EXCEPTION 'stet.held_writes: what was held cannot change' USING ERRCODE = 'ST004';
  END IF;
  IF NEW.in_transaction IS DISTINCT FROM OLD.in_transaction
     AND NOT (OLD.status = 'unconfirmed' AND NEW.status = 'pending') THEN
    RAISE EXCEPTION 'stet.held_writes: in_transaction is set once, when the row becomes pending' USING ERRCODE = 'ST004';
  END IF;
  IF NEW.status <> OLD.status
     AND (OLD.status, NEW.status) NOT IN (('unconfirmed', 'pending'), ('unconfirmed', 'undelivered'),
                                          ('pending', 'approved'), ('pending', 'rejected'),
                                          ('approved', 'applied'), ('approved', 'failed')) THEN
    RAISE EXCEPTION 'stet.held_writes: status cannot go from % to %', OLD.status, NEW.status USING ERRCODE = 'ST004';
  END IF;
  IF (NEW.resolved_by, NEW.resolved_at, NEW.note, NEW.max_rows)
       IS DISTINCT FROM (OLD.resolved_by, OLD.resolved_at, OLD.note, OLD.max_rows)
     AND NOT (OLD.status = 'pending' AND NEW.status IN ('approved', 'rejected')) THEN
    RAISE EXCEPTION 'stet.held_writes: resolved_by, resolved_at, note and max_rows are set once, when the row is approved or rejected'
      USING ERRCODE = 'ST004';
  END IF;
  IF (NEW.applied_at, NEW.rows_affected, NEW.error) IS DISTINCT FROM (OLD.applied_at, OLD.rows_affected, OLD.error)
     AND NOT (OLD.status = 'approved' AND NEW.status IN ('applied', 'failed')) THEN
    RAISE EXCEPTION 'stet.held_writes: applied_at, rows_affected and error are set once, when the row is applied or fails'
      USING ERRCODE = 'ST004';
  END IF;
  RETURN NEW;
END
$$;

DROP TRIGGER IF EXISTS held_writes_guard ON stet.held_writes;
CREATE TRIGGER held_writes_guard
  BEFORE UPDATE ON stet.held_writes
  FOR EACH ROW EXECUTE FUNCTION stet.held_writes_guard();

-- 검토자가 보는 것 (docs/spec-v0.2-approve.md 5장). 보류 건과 판단 기록의 근거를 함께 보여 준다.
-- 판단 기록이 없는 줄도 나온다. expires_at이 지난 pending은 expired로 보인다(저장하는 상태가 아니다).
-- 뷰는 소유자(Stet의 계정)의 권한으로 읽으므로 검토자에게 두 테이블의 권한이 필요 없다.
-- settings는 텍스트 값이 읽힐 설정이다. 마지막 컬럼으로 둔다. CREATE OR REPLACE VIEW는 컬럼을 끝에 더하는 것만 받는다.
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
       h.applied_at, h.rows_affected, h.error,
       h.settings
FROM stet.held_writes h LEFT JOIN stet.decisions d ON d.id = h.decision_id;

-- 승인과 거부 (docs/spec-v0.2-approve.md 6장). 줄의 상태만 바꾸고 문장은 실행하지 않는다. 실행은 Stet 프로세스가 한다(7장).
-- 줄을 잠근 뒤 정해진 순서로 확인하고, 걸리면 ST005로 거절한다. 시각은 트랜잭션의 시작(now())이 아니라
-- clock_timestamp()다(계획의 보정 B9). 검토자가 오래 열어 둔 트랜잭션에서도 만료를 지금 시각으로 보고,
-- resolved_at은 실제로 승인·거부한 시각이 된다. 상한이 없는 승인은 받지 않는다(보정 B5).
CREATE OR REPLACE FUNCTION stet.approve(id uuid, note text DEFAULT NULL, max_rows bigint DEFAULT NULL) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  h stet.held_writes;
BEGIN
  SELECT * INTO h FROM stet.held_writes w WHERE w.id = approve.id FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'held write % does not exist', approve.id USING ERRCODE = 'ST005';
  END IF;
  IF h.status = 'unconfirmed' THEN
    RAISE EXCEPTION 'held write % is unconfirmed: it is not confirmed that the agent was told of it', approve.id
      USING ERRCODE = 'ST005';
  END IF;
  IF h.status <> 'pending' THEN
    RAISE EXCEPTION 'held write % is %, not pending', approve.id, h.status USING ERRCODE = 'ST005';
  END IF;
  IF h.expires_at IS NULL OR h.expires_at <= clock_timestamp() THEN
    RAISE EXCEPTION 'held write % has expired', approve.id USING ERRCODE = 'ST005';
  END IF;
  IF session_user = h.login_role THEN
    RAISE EXCEPTION '% sent this write and cannot approve it', session_user USING ERRCODE = 'ST005';
  END IF;
  IF 'concurrent_change' = ANY (h.reasons) THEN
    RAISE EXCEPTION 'held write % was held because its row kept changing while it was judged (concurrent_change): '
      'its stored state is stale and it would not run; the agent must send it again', approve.id
      USING ERRCODE = 'ST005';
  END IF;
  IF approve.max_rows < 0 THEN
    RAISE EXCEPTION 'max_rows must not be negative' USING ERRCODE = 'ST005';
  END IF;
  IF approve.max_rows IS NULL AND (h.rows_exact IS NOT TRUE OR h.planned_rows IS NULL) THEN
    RAISE EXCEPTION 'held write % was not measured exactly (planned_rows: %): pass max_rows, the most rows it may change',
      approve.id, coalesce(h.planned_rows::text, 'unknown') USING ERRCODE = 'ST005';
  END IF;
  UPDATE stet.held_writes w
     SET status = 'approved', resolved_by = session_user, resolved_at = clock_timestamp(),
         note = approve.note, max_rows = approve.max_rows
   WHERE w.id = approve.id;
  RETURN 'approved';
END
$$;

-- 거부는 앞의 세 확인만 한다. 만료된 줄도, 자기 쓰기도 거부할 수 있다.
CREATE OR REPLACE FUNCTION stet.reject(id uuid, note text DEFAULT NULL) RETURNS text
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, pg_temp AS $$
DECLARE
  h stet.held_writes;
BEGIN
  SELECT * INTO h FROM stet.held_writes w WHERE w.id = reject.id FOR UPDATE;
  IF NOT FOUND THEN
    RAISE EXCEPTION 'held write % does not exist', reject.id USING ERRCODE = 'ST005';
  END IF;
  IF h.status = 'unconfirmed' THEN
    RAISE EXCEPTION 'held write % is unconfirmed: it is not confirmed that the agent was told of it', reject.id
      USING ERRCODE = 'ST005';
  END IF;
  IF h.status <> 'pending' THEN
    RAISE EXCEPTION 'held write % is %, not pending', reject.id, h.status USING ERRCODE = 'ST005';
  END IF;
  UPDATE stet.held_writes w
     SET status = 'rejected', resolved_by = session_user, resolved_at = clock_timestamp(), note = reject.note
   WHERE w.id = reject.id;
  RETURN 'rejected';
END
$$;

-- 되돌리기의 캡처 (docs/spec-v0.2-undo.md 4장). 테이블 소유자가 설치한 문장 단위 트리거가 stet.capture()를 부르고,
-- 이 함수가 에이전트의 쓰기가 바꾼 행의 이전·이후 값을 쓰기와 같은 트랜잭션에서 남긴다. 롤백된 쓰기는 남지 않는다.
-- 이 세 표에는 캡처한 에이전트의 트랜잭션이 끝날 때까지 약한 잠금이 걸린다. 표가 이미 있으면 이 파일은 그 표에 잠금을
-- 잡는 문장을 실행하지 않는다(11장의 "스키마의 잠금"). 인덱스와 트리거는 없는지 먼저 보고 만든다. 컬럼을 더할 때는
-- 이것을 다시 본다.

-- 문장 하나가 한 테이블에 한 일
CREATE TABLE IF NOT EXISTS stet.change_sets (
  id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at            timestamptz NOT NULL,          -- 남긴 때. clock_timestamp()
  stmt_at       timestamptz NOT NULL,          -- 서버가 그 문장의 메시지를 받은 때. statement_timestamp()
  txid          bigint,                        -- txid_current(). 묶음을 쓰는 INSERT가 어차피 트랜잭션 id를 받는다
  pid           integer NOT NULL,              -- pg_backend_pid()
  login_role    text NOT NULL,                 -- session_user
  agent_role    text NOT NULL,                 -- 이 쓰기의 주인으로 보는 계정 (stet.capture()의 2)
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
  roles     text[],                            -- 캡처할 계정. NULL이면 모든 세션
  max_rows  bigint NOT NULL                    -- 한 문장이 이보다 많은 행을 바꾸면 값을 남기지 않는다
);

-- CREATE INDEX IF NOT EXISTS는 인덱스가 있는지 보기 전에 표에 잠금을 잡는다. to_regclass는 잠금을 잡지 않는다.
DO $$
BEGIN
  IF to_regclass('stet.change_sets_role_idx') IS NULL THEN
    CREATE INDEX change_sets_role_idx ON stet.change_sets (agent_role, at);
  END IF;
  IF to_regclass('stet.change_sets_backend_idx') IS NULL THEN
    CREATE INDEX change_sets_backend_idx ON stet.change_sets (pid, stmt_at);
  END IF;
  IF to_regclass('stet.changes_set_idx') IS NULL THEN
    CREATE INDEX changes_set_idx ON stet.changes (set_id);
  END IF;
  -- 캡처가 한 트랜잭션에서 남긴 묶음을 세는 데 쓴다(stet.capture()의 상한)
  IF to_regclass('stet.change_sets_txid_idx') IS NULL THEN
    CREATE INDEX change_sets_txid_idx ON stet.change_sets (txid);
  END IF;
END
$$;

-- 기록은 바뀌지 않는다 (4장). 두 표의 UPDATE와 TRUNCATE는 Stet의 계정도 못 한다. 역할 권한과 별개로 DB가 직접 막는다.
-- DELETE는 막지 않는다(보존 기간). 문장 단위 트리거라 0행을 고치는 UPDATE도 거절한다.
CREATE OR REPLACE FUNCTION stet.changes_guard() RETURNS trigger
LANGUAGE plpgsql SET search_path = pg_catalog, pg_temp AS $$
BEGIN
  RAISE EXCEPTION 'stet.%: what capture recorded cannot change; % is refused, only DELETE is allowed', TG_TABLE_NAME, TG_OP
    USING ERRCODE = 'ST004';
END
$$;

-- DROP TRIGGER IF EXISTS와 CREATE TRIGGER는 표에 강한 잠금을 잡는다. 열린 캡처가 있으면 그것을 기다리고, 그 뒤의 모든
-- 캡처가 이것을 기다린다(11장의 "스키마의 잠금"). 그래서 pg_trigger에 없을 때만 만든다. 카탈로그를 읽는 것과 regclass는
-- 표에 잠금을 잡지 않는다.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_trigger
                  WHERE tgrelid = 'stet.change_sets'::regclass AND tgname = 'change_sets_guard') THEN
    CREATE TRIGGER change_sets_guard
      BEFORE UPDATE OR TRUNCATE ON stet.change_sets
      FOR EACH STATEMENT EXECUTE FUNCTION stet.changes_guard();
  END IF;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_trigger
                  WHERE tgrelid = 'stet.changes'::regclass AND tgname = 'changes_guard') THEN
    CREATE TRIGGER changes_guard
      BEFORE UPDATE OR TRUNCATE ON stet.changes
      FOR EACH STATEMENT EXECUTE FUNCTION stet.changes_guard();
  END IF;
END
$$;

-- 캡처 트리거 함수. 테이블마다 트리거 넷이 이 함수를 부른다. 인자는 설정의 테이블 키와 키 컬럼의 이름이다:
--   CREATE TRIGGER stet_capture_ins AFTER INSERT ON public.deals REFERENCING NEW TABLE AS stet_new
--     FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
--   CREATE TRIGGER stet_capture_upd AFTER UPDATE ON public.deals REFERENCING OLD TABLE AS stet_old
--     NEW TABLE AS stet_new FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
--   CREATE TRIGGER stet_capture_del AFTER DELETE ON public.deals REFERENCING OLD TABLE AS stet_old
--     FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
--   CREATE TRIGGER stet_capture_trunc AFTER TRUNCATE ON public.deals
--     FOR EACH STATEMENT EXECUTE FUNCTION stet.capture('deals', 'id');
-- 소유자(Stet의 계정)의 권한으로 돈다. 값의 글자가 세션 설정에 따라 달라지지 않게 다섯 설정을 함수 안에서만 고정하고,
-- client_min_messages로 함수가 남기는 로그가 클라이언트로 가지 않게 한다. 모델을 부르지 않는다(규칙 1).
-- 남기는 값과 query, app은 에이전트가 쓴 데이터다(규칙 5).
-- 전이 테이블 stet_old, stet_new는 동적 SQL에서도 보인다. 컬럼이 테이블마다 다르므로 문장을 그때의 pg_attribute로
-- 만든다. 그 트리거에 없는 전이 테이블을 가리키는 문장은 실행하지 않는다. 이름이 긴 것은 세션의 임시 릴레이션과 우연히
-- 겹치지 않게 하려는 것이다(아래, 계획의 판정 U11).
-- 트리거가 위와 다르게 설치됐으면 전이 테이블을 읽기 전에 에러를 낸다. 그 이름의 전이 테이블이 없으면 그 이름은 세션의 임시
-- 스키마(pg_temp, 릴레이션을 찾을 때 빠지지 않는다)의 같은 이름의 릴레이션으로 읽힌다. 캡처되는 세션은 누구나, 에이전트도,
-- 그런 뷰를 만들어 자기 코드를 Stet의 계정으로 돌리고 없던 행을 남기게 할 수 있다. 키 컬럼 인자도 그 테이블의 컬럼인지 본다.
-- 컬럼이 아니면 r.<인자>가 행을 받는 함수로 읽힌다(to_jsonb(r)은 행의 타입에서 json으로 가는 캐스트를 부른다).
-- 값의 글자는 타입의 출력 함수로 만든다(format의 %s). ::text는 그 타입에서 text로 가는 캐스트가 있으면 그 함수를 부른다.
-- 캐스트는 타입의 소유자가 만든 함수일 수 있고, 여기서는 Stet의 계정으로 돈다. 출력 함수는 C로만 만들 수 있다(superuser).
-- NULL은 num_nulls로 가린다. 복합 타입의 IS NULL은 모든 필드가 NULL인 값에도 참이다.
-- 캡처는 쓰기를 실패시키지 않는다(4장). 1부터 7까지와 설치의 확인이 한 EXCEPTION WHEN OTHERS 블록 안이고, 블록 밖에서는
-- 실패할 수 없는 것만 한다. 블록 안에서 에러가 나면 블록이 한 일(묶음, 줄)이 되돌려지고, 묶음 하나를
-- skipped = 'capture_failed'와 detail의 SQLSTATE, 메시지로 남긴다. 그것도 실패하면 서버 로그(RAISE LOG)에만 남긴다.
-- client_min_messages가 warning이므로 클라이언트에게는 아무것도 가지 않는다. WHEN OTHERS는 시간 초과와 취소(57014)를
-- 잡지 않으므로 그때는 쓰기가 실패한다. lock_timeout(55P03)은 잡는다.
-- 삼키면 에이전트가 일으켜 쓰기를 기록도 표지도 없이 커밋시킬 수 있는 에러는 쓰기로 되돌려 보낸다(계획의 판정 R8). 쓰기가
-- 실패해 다시 보내지는 것은 안전하고, 기록 없이 커밋되는 것은 안전하지 않다.
-- - serialization_failure(40001)는 두 블록 모두 되돌려 보낸다. SERIALIZABLE에서는 캡처가 stet의 표를 읽고 쓰는 것이 세션
--   사이에 충돌하고, 에이전트는 두 세션으로 이것을 일으킨다. 표지를 쓰는 블록도 같은 충돌에 걸린다.
-- - 자원과 한도(53, 54 부류, 예를 들어 깊은 함수 호출 끝에서 캡처만 스택이 모자라는 54001)는 아무것도 남길 수 없을 때,
--   곧 표지를 쓰는 블록이 실패했거나 세는 것이 실패했을 때만 되돌려 보낸다. 큰 블록만 실패하면 표지가 남는다.
-- - 교착(40P01)은 삼킨다. 캡처는 stet의 표의 잠금만 기다리고, 에이전트는 그 표를 잠글 수 없으므로 캡처가 낀 교착을 만들 수
--   없다. REPEATABLE READ의 40001도 같은 코드라 되돌려 보내지만, 캡처의 문장(삽입과 잠그지 않는 읽기)은 그것을 내지 않는다.
-- 한 트랜잭션의 상한. EXCEPTION 블록은 하위 트랜잭션이고, 표에 쓸 때만 자기 트랜잭션 id를 받는다. 커밋된 하위 트랜잭션의
-- id는 트랜잭션이 끝날 때까지 백엔드의 캐시(64개)에 남고, 넘치면 서버 전체의 스냅숏 확인이 느려진다. 되돌려진 하위
-- 트랜잭션은 자리를 돌려준다. 표에 쓰는 캡처 하나는 id 하나만 남긴다: 값이나 표지를 남기면 큰 블록의 것, 실패하면 큰 블록은
-- 되돌려지고 표지를 쓴 작은 블록의 것이다. 그래서 한 트랜잭션에서 표에 쓰는 것은 32번까지다. 31번 썼으면 다음에 쓰는 것은
-- too_many_statements 표지 하나이고, 그 뒤로는 아무것도 쓰지 않는다.
-- 쓴 횟수(done)는 이 백엔드가 이 트랜잭션의 id로 남긴 묶음의 수다(change_sets_txid_idx). 한 트랜잭션의 묶음은 모두 한
-- 백엔드의 것이다. pid도 보는 것은 다른 클러스터에서 옮겨 온 묶음(pg_dump, 논리 복제)이 같은 txid를 가질 수 있어서다.
-- 겹칠 수는 여전히 있지만 드물다(계획의 판정 R9).
-- 캡처할 때마다 센다. 세션의 값(설정)으로 세지 않는다. 에이전트가 적을 수 있는 값이어서, 낮추면 상한을 넘어 표지 없이 남기지 않게 되고 높이면 캡처가 꺼졌다(계획의 판정
-- R7, 보정 U9를 대신한다). 되돌려진 쓰기의 묶음은 보이지 않으므로 세지 않는다. 그 하위 트랜잭션도 자리를 돌려주었다.
-- 시각으로 고르지 않는다. 프로시저가 COMMIT한 뒤의 트랜잭션에서는 문장의 statement_timestamp()가 그 트랜잭션의 now()보다
-- 이르다(같은 Sync 아래 뒤따르는 문장도 그 트랜잭션에 든다). 묶음의 txid는 txid_current()다. 트랜잭션이 id를 받기 전에
-- 쓰는 표지(0행 문장의 거절)도 그 id를 남기고 세어진다.
-- 세는 것은 3 뒤, 무엇을 쓰기 전이다. 캡처하지 않는 세션은 세지 않는다. 트랜잭션에 아직 id가 없으면 묶음도 없으므로 묻지
-- 않고 0이다. 세는 것은 읽기뿐이라 id를 받지 않는다. 그래서 캡처하지 않는 세션과 잘 설치된 트리거의 0행 문장은 아무것도
-- 쓰지 않고 id도 받지 않는다.
-- 큰 블록이 세기 전에(설정을 읽다가) 실패했으면 done은 NULL이고, 표지를 쓰는 작은 블록이 같은 방법으로 센다. 세는 것
-- 자체가 실패했으면(stet.change_sets의 잠금에 lock_timeout이 걸렸다) 다시 세지 않고 표지도 쓰지 않는다. 같은 잠금을 다시
-- 기다리게 되고, 수를 모르면 표지가 33번째 쓰기일 수 있다. 서버 로그에만 남긴다. 에이전트는 stet의 표를 잠글 수 없다.
-- 블록 밖에서는 세션의 값을 읽지 않는다. stet.held_write의 정규식은 앞의 37글자에만 건다(정규식은 글자마다 4바이트로
-- 넓혀 맞추므로, 아주 긴 값은 그것만으로 메모리 요청이 실패한다).
CREATE OR REPLACE FUNCTION stet.capture() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
DECLARE
  text_of       CONSTANT text := 'CASE WHEN num_nulls(r.%1$I) = 0 THEN format(''%%s'', r.%1$I) END';
  capture_roles text[];
  cap           bigint;
  agent         text := session_user;
  held_text     text;
  held          uuid;
  old_table     name;
  new_table     name;
  changed       bigint;
  skip          text;
  set_id        bigint;
  names         text[];
  img           text;
  keyval        text;
  done          integer;        -- 이 트랜잭션에서 캡처가 표에 쓴 횟수. 세기 전에는 NULL
  counting      boolean := false;  -- 세기 시작했다. done이 NULL이면 세는 것이 실패했다
  tx            bigint;
  failure       text;
  code          text;
  message       text;
BEGIN
  BEGIN
    -- 2. 쓰기의 주인. SECURITY DEFINER 안에서 current_user는 이 함수의 소유자, 곧 Stet의 계정이다. Stet의 계정이
    --    SET LOCAL ROLE로 에이전트가 되어 실행한 승인된 쓰기는 그 에이전트의 것이다. 1보다 먼저 정해, 설정을 읽다가
    --    실패해도 표지에 맞는 주인이 남게 한다.
    --    7. 그때만 stet.held_write를 읽는다. 다른 계정의 세션에서는 에이전트가 스스로 적을 수 있는 값이다. Stet의 세션에서도
    --    승인된 문장(에이전트가 쓴 것)이 바꿀 수 있으므로(11장), uuid의 모양이 아니면 NULL로 남긴다.
    IF session_user = current_user THEN
      agent := coalesce(nullif(current_setting('role'), 'none'), session_user);
      held_text := current_setting('stet.held_write', true);
      IF left(held_text, 37) ~* '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$' THEN
        held := held_text::uuid;
      END IF;
    END IF;

    -- 1. 설정. 줄이 없으면 모든 세션을 캡처하고 상한은 10,000이다.
    SELECT s.roles, s.max_rows INTO capture_roles, cap FROM stet.capture_settings s;
    IF NOT FOUND THEN
      capture_roles := NULL;
      cap := 10000;
    END IF;

    -- 3. 캡처할 계정이 아니면 아무것도 하지 않는다.
    IF capture_roles IS NOT NULL AND (agent = ANY (capture_roles)) IS NOT TRUE THEN
      RETURN NULL;
    END IF;

    -- 한 트랜잭션의 상한. 이 트랜잭션이 남긴 묶음을 센다(위의 주석). 무엇을 쓰기 전이고, 읽기만 하므로 id를 받지 않는다.
    counting := true;
    tx := txid_current_if_assigned();
    IF tx IS NULL THEN
      done := 0;
    ELSE
      SELECT count(*) INTO done FROM stet.change_sets s WHERE s.txid = tx AND s.pid = pg_backend_pid();
    END IF;
    IF done >= 32 THEN
      RETURN NULL;
    END IF;

    -- 설치의 확인. 전이 테이블을 읽기 전, 키 인자를 문장에 넣기 전이다(위의 주석).
    IF TG_WHEN <> 'AFTER' OR TG_LEVEL <> 'STATEMENT' THEN
      RAISE EXCEPTION 'trigger % on %.% must be AFTER ... FOR EACH STATEMENT, as stet undo-sql prints it, not % ... FOR EACH %',
        TG_NAME, TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_WHEN, TG_LEVEL USING ERRCODE = 'trigger_protocol_violated';
    END IF;
    IF TG_OP <> 'TRUNCATE' THEN
      SELECT t.tgoldtable, t.tgnewtable INTO old_table, new_table
        FROM pg_catalog.pg_trigger t
       WHERE t.tgrelid = TG_RELID AND t.tgname = TG_NAME;
      IF (TG_OP <> 'INSERT' AND old_table IS DISTINCT FROM 'stet_old') OR (TG_OP <> 'DELETE' AND new_table IS DISTINCT FROM 'stet_new') THEN
        RAISE EXCEPTION 'trigger % on %.% must say REFERENCING %, as stet undo-sql prints it', TG_NAME, TG_TABLE_SCHEMA, TG_TABLE_NAME,
          CASE TG_OP WHEN 'INSERT' THEN 'NEW TABLE AS stet_new' WHEN 'DELETE' THEN 'OLD TABLE AS stet_old'
                     ELSE 'OLD TABLE AS stet_old NEW TABLE AS stet_new' END
          USING ERRCODE = 'trigger_protocol_violated';
      END IF;
    END IF;
    IF TG_NARGS <> 2 OR NOT EXISTS (SELECT FROM pg_catalog.pg_attribute a
                                     WHERE a.attrelid = TG_RELID AND a.attname = TG_ARGV[1]
                                       AND a.attnum > 0 AND NOT a.attisdropped) THEN
      RAISE EXCEPTION 'trigger % on %.% must pass stet.capture() two arguments, the table''s key in the configuration and '
                      'the name of one of its columns; it passes %, the second %',
        TG_NAME, TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_NARGS, coalesce(quote_literal(TG_ARGV[1]), 'missing')
        USING ERRCODE = 'invalid_parameter_value';
    END IF;

    -- 4. TRUNCATE는 표지만 남긴다. 5. 바뀐 행을 센다. 0이면 아무것도 남기지 않고, 상한을 넘으면 표지만 남긴다.
    IF TG_OP = 'TRUNCATE' THEN
      skip := 'truncate';
    ELSE
      -- 전이 테이블을 처음 읽기 바로 앞이다. 위의 pg_trigger 확인은 지금의 카탈로그를 읽지만, PostgreSQL은 문장이 시작할 때
      -- 읽어 둔 트리거를 부른다. 테이블 소유자는 문장 도중에(그 문장이 부르는 함수에서) 불릴 트리거의 이름을 바꾸거나 지우고
      -- 같은 이름으로 전이 테이블이 있는 트리거를 만들 수 있다. 그러면 위의 확인은 통과하고, 불린 트리거에는 전이 테이블이
      -- 없어 그 이름이 pg_temp의 릴레이션으로 읽힌다. 그래서 세션의 임시 스키마에 그 이름의 릴레이션이 있으면 읽지 않는다.
      -- 이 확인부터 전이 테이블을 마지막으로 읽는 문장까지는 고객의 코드가 돌 수 있는 것을 두지 않는다. 그 사이의 문장은
      -- stet의 표에 넣는 것, 카탈로그를 읽는 것, 내장 함수와 타입의 출력 함수(C, superuser만 만든다)뿐이다.
      -- 세는 문장과 값을 읽는 문장은 따로 이름을 찾지만, 그 사이에도 고객의 코드는 돌지 않는다.
      IF EXISTS (SELECT FROM pg_catalog.pg_class c
                  WHERE c.relnamespace = pg_catalog.pg_my_temp_schema() AND c.relname IN ('stet_old', 'stet_new')) THEN
        RAISE EXCEPTION 'this session has a temporary relation named stet_old or stet_new; stet.capture() does not read its '
                        'transition tables while one exists, since a trigger without them would read that relation instead'
          USING ERRCODE = 'duplicate_table';
      END IF;
      EXECUTE CASE TG_OP WHEN 'DELETE' THEN 'SELECT count(*) FROM stet_old' ELSE 'SELECT count(*) FROM stet_new' END INTO changed;
      IF changed = 0 THEN
        RETURN NULL;
      END IF;
      IF changed > cap THEN
        skip := 'too_many_rows';
      END IF;
    END IF;
    IF done = 31 THEN
      skip := 'too_many_statements';
    END IF;

    -- 6. 묶음 하나
    INSERT INTO stet.change_sets (at, stmt_at, txid, pid, login_role, agent_role, app, held_write,
                                  target_table, key_column, relid, op, query, rows, skipped)
    VALUES (clock_timestamp(), statement_timestamp(), txid_current(), pg_backend_pid(), session_user, agent,
            current_setting('application_name'), held,
            TG_ARGV[0], TG_ARGV[1], TG_RELID, lower(TG_OP), current_query(), changed, skip)
    RETURNING id INTO set_id;

    -- 6. 행마다 한 줄. 값은 jsonb_object(컬럼 이름의 배열, 값의 글자의 배열)이다. 지워진 컬럼은 뺀다.
    IF skip IS NULL THEN
      SELECT array_agg(a.attname::text ORDER BY a.attnum),
             'jsonb_object($2, ARRAY[' || string_agg(format(text_of, a.attname), ', ' ORDER BY a.attnum) || '])'
        INTO names, img
        FROM pg_catalog.pg_attribute a
       WHERE a.attrelid = TG_RELID AND a.attnum > 0 AND NOT a.attisdropped;
      keyval := format(text_of, TG_ARGV[1]);
      IF TG_OP = 'INSERT' THEN
        EXECUTE format('INSERT INTO stet.changes (set_id, key, after) SELECT $1, %s, %s FROM stet_new r', keyval, img)
          USING set_id, names;
      ELSIF TG_OP = 'DELETE' THEN
        EXECUTE format('INSERT INTO stet.changes (set_id, key, before) SELECT $1, %s, %s FROM stet_old r', keyval, img)
          USING set_id, names;
      ELSE
        -- 이전 행들과 이후 행들을 키로 묶는다. 키가 바뀐 행은 옛 키의 줄(before만)과 새 키의 줄(after만)이 된다.
        EXECUTE format('INSERT INTO stet.changes (set_id, key, before, after)
                        SELECT $1, k, b.img, a.img
                          FROM (SELECT %1$s AS k, %2$s AS img FROM stet_old r) b
                          FULL JOIN (SELECT %1$s AS k, %2$s AS img FROM stet_new r) a USING (k)', keyval, img)
          USING set_id, names;
      END IF;
    END IF;

    RETURN NULL;
  EXCEPTION
    WHEN serialization_failure THEN
      RAISE;
    WHEN OTHERS THEN
      GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, message = MESSAGE_TEXT;
      failure := code || ': ' || message;
  END;

  -- 큰 블록 안의 길은 모두 RETURN으로 끝난다. 여기에 오는 것은 큰 블록이 실패했을 때뿐이다. 새 길을 더할 때도 RETURN으로
  -- 끝낸다. 그렇지 않은 길이 생겨도 detail 없는 표지를 남기지 않게 failure를 본다.
  IF failure IS NULL THEN
    RETURN NULL;
  END IF;
  -- 캡처가 실패했다. 블록 안의 변수(agent, held, changed, counting, done)는 되돌려지지 않고 남아 있다. 세는 것 자체가
  -- 실패했으면 다시 세지 않고 쓰지도 않는다(위의 주석).
  -- 자원이나 한도 때문이면(53, 54) 쓰기를 실패시킨다(위의 주석).
  IF counting AND done IS NULL THEN
    IF left(code, 2) IN ('53', '54') THEN
      RAISE EXCEPTION USING ERRCODE = code, MESSAGE = message;
    END IF;
    RAISE LOG 'stet.capture() on %.% (trigger %, pid %, txid %, agent %): could not count what this transaction recorded (%), so recorded nothing',
      TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_NAME, pg_backend_pid(), tx, agent, failure;
    RETURN NULL;
  END IF;
  -- 세기 전에 실패했으면 여기서 센다. 상한에 닿았으면 쓰지 않고, 32번째면 too_many_statements 표지를 쓴다. 이 표지를 쓰는
  -- 작은 블록도 하위 트랜잭션이고, 실패하면 자리를 돌려준다.
  BEGIN
    IF done IS NULL THEN
      tx := txid_current_if_assigned();
      IF tx IS NULL THEN
        done := 0;
      ELSE
        SELECT count(*) INTO done FROM stet.change_sets s WHERE s.txid = tx AND s.pid = pg_backend_pid();
      END IF;
    END IF;
    IF done >= 32 THEN
      RETURN NULL;
    END IF;
    INSERT INTO stet.change_sets (at, stmt_at, txid, pid, login_role, agent_role, app, held_write,
                                  target_table, key_column, relid, op, query, rows, skipped, detail)
    VALUES (clock_timestamp(), statement_timestamp(), txid_current(), pg_backend_pid(), session_user, agent,
            current_setting('application_name'), held,
            coalesce(TG_ARGV[0], ''), coalesce(TG_ARGV[1], ''), TG_RELID, lower(TG_OP), current_query(), changed,
            CASE WHEN done = 31 THEN 'too_many_statements' ELSE 'capture_failed' END, failure);
  EXCEPTION
    WHEN serialization_failure OR insufficient_resources OR program_limit_exceeded THEN
      RAISE;
    WHEN OTHERS THEN
      GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, message = MESSAGE_TEXT;
      RAISE LOG 'stet.capture() on %.% (trigger %, pid %): capture failed with %, and recording that failed with %: %',
        TG_TABLE_SCHEMA, TG_TABLE_NAME, TG_NAME, pg_backend_pid(), failure, code, message;
  END;
  RETURN NULL;
END
$$;

-- 새로 만든 함수는 PUBLIC이 실행할 수 있다. 이 파일을 실행할 때마다 거둔다. Migrate는 한 트랜잭션이므로 PUBLIC이
-- 부를 수 있는 틈이 없고, CREATE OR REPLACE는 이미 준 권한을 지우지 않는다. 누가 승인할 수 있는지는 운영자가 정한다:
--   GRANT USAGE ON SCHEMA stet TO reviewer;
--   GRANT SELECT ON stet.review TO reviewer;
--   GRANT EXECUTE ON FUNCTION stet.approve(uuid, text, bigint), stet.reject(uuid, text) TO reviewer;
REVOKE EXECUTE ON FUNCTION stet.approve(uuid, text, bigint), stet.reject(uuid, text) FROM PUBLIC;
-- 캡처 트리거를 만들 테이블 소유자에게는 이 둘을 준다(stet undo-sql이 출력한다). 트리거 함수는 직접 부를 수 없으므로
-- 이 권한으로 할 수 있는 일은 트리거를 만드는 것뿐이다. 쓰는 계정에는 아무것도 주지 않는다:
--   GRANT USAGE ON SCHEMA stet TO table_owner;
--   GRANT EXECUTE ON FUNCTION stet.capture() TO table_owner;
REVOKE EXECUTE ON FUNCTION stet.capture() FROM PUBLIC;
-- 기록을 지키는 트리거의 함수는 아무에게도 주지 않는다. 트리거는 함수의 권한 없이도 불린다.
REVOKE EXECUTE ON FUNCTION stet.changes_guard() FROM PUBLIC;

-- 검토자가 보는 것 (docs/spec-v0.2-undo.md 6장). 캡처한 변경마다 한 줄이고, 값을 남기지 않은 묶음(skipped)은 key 없이
-- 한 줄이다. 변경을 그 쓰기를 판단한 기록에 잇는다(5장의 "잇는 규칙", 계획의 판정 U16).
-- 묶음 하나는 다음을 모두 만족하는 판단 기록이 하나뿐일 때만 그것에 이어진다: backend_pid가 묶음의 pid와 같고,
-- executed가 참이고, 묶음의 stmt_at이 sent_after에서 10ms를 뺀 때부터 done_before까지(양 끝 포함)이고, statement가
-- 묶음의 query와 같은 글자다. 둘 이상이면 잇지 않고 그 수를 decision_candidates로 보여 준다. 키로 좁히지 않는다.
-- target_pk와 캡처한 key는 잇는 데 쓰지 않는다. 키로 좁히는 규칙은 키를 다시 쓰는 트리거, 에이전트가 다르게 쓴 키, 값을
-- 남기지 않은 묶음, 아무것도 바꾸지 않은 쓰기가 섞이면 다른 쓰기의 판단 기록에 이었다. 잘못 잇는 것보다 잇지 않는 쪽이다.
-- 그래서 한 연결에서 같은 글자로 잇달아(한 Sync 아래, 파이프라인으로) 보낸 쓰기는 이어지지 않고 후보의 수만 보인다.
-- 한 문장이 남긴 묶음들(연쇄로 바뀐 자식 테이블의 것 등)은 pid, stmt_at, query가 같으므로 같은 결과를 보인다.
-- 하나도 없으면 Stet이 판단하지 않은 변경이다.
-- held_write가 있는 묶음(승인으로 실행된 쓰기)은 이 규칙을 쓰지 않고 그 보류 줄의 decision_id로 잇는다. 보류 줄이나 그
-- 판단 기록이 없으면 잇지 않는다.
-- 잇는 것은 저장하지 않고 읽을 때 계산한다. 판단 기록은 쓰기가 끝난 뒤에 들어가고 묶음은 커밋돼야 보이므로 어느 쪽이 먼저인지
-- 정해져 있지 않다. 후보는 묶음마다 한 번 모은다(LATERAL). decisions_window_idx로 찾는다: pid가 같고 done_before가
-- stmt_at 이후인 범위를 훑고, sent_after는 인덱스 안에서 거른다. 최근의 묶음일수록 훑을 것이 적다. 반대쪽 끝(sent_after의
-- 아래쪽)은 두지 않는다. 쓰기가 서버에 머문 시간에는 상한이 없어서, 끝을 두면 후보를 놓칠 수 있고 놓치면 잘못 이을 수 있다.
-- 줄은 changes_set_idx로 붙인다.
-- 소유자(Stet의 계정)의 권한으로 읽는다. 검토자에게는 두 가지만 준다(운영자가 정한다). before와 after에는 그 테이블의 모든
-- 컬럼이 있다:
--   GRANT USAGE ON SCHEMA stet TO reviewer;
--   GRANT SELECT ON stet.history TO reviewer;
-- 캡처가 남긴 값과 query, app은 에이전트가 쓴 데이터다(규칙 5). 보여 주기만 한다.
-- undo_id와 undone_at은 되돌리기(7장)가 채운다. CREATE OR REPLACE VIEW는 컬럼을 끝에 더하는 것만 받으므로 컬럼의 순서와
-- 타입은 6장의 표 그대로 둔다.
CREATE OR REPLACE VIEW stet.history AS
SELECT c.id AS change_id, s.id AS set_id, s.at,
       s.agent_role, s.login_role, s.app,
       s.target_table, s.op, c.key, c.before, c.after,
       s.rows, s.skipped, s.detail, s.query,
       s.held_write,
       d.id AS decision_id, link.candidates AS decision_candidates,
       d.verdict, d.reasons,
       NULL::bigint AS undo_id, NULL::timestamptz AS undone_at
FROM stet.change_sets s
CROSS JOIN LATERAL (
  SELECT CASE WHEN count(*) = 1 THEN (array_agg(m.id))[1] END AS decision_id,
         count(*)::integer AS candidates
    FROM (
      -- 승인으로 실행된 쓰기: 보류 줄의 판단 기록
      SELECT hd.id
        FROM stet.held_writes h
        JOIN stet.decisions hd ON hd.id = h.decision_id
       WHERE h.id = s.held_write
      UNION ALL
      -- 잇는 규칙
      SELECT x.id
        FROM stet.decisions x
       WHERE s.held_write IS NULL
         AND x.backend_pid = s.pid
         AND x.done_before >= s.stmt_at
         AND x.sent_after <= s.stmt_at + interval '10 milliseconds'
         AND x.executed
         AND x.statement = s.query
    ) m
) link
LEFT JOIN stet.changes c ON c.set_id = s.id
LEFT JOIN stet.decisions d ON d.id = link.decision_id;

-- 되돌리기 (docs/spec-v0.2-undo.md 7장): stet.undo_plan과, 그것이 stet.undo와 함께 쓰는 도우미. 모델을 부르지 않는다
-- (규칙 1). 캡처가 남긴 값과 키는 에이전트가 쓴 데이터다(규칙 5). 비교와 복구의 재료로만 쓰고, SQL의 글자로 넣을 때는
-- %L과 %I로 인용한다.
-- 모든 함수가 일곱 설정(search_path = pg_catalog, pg_temp와 다섯 설정, client_min_messages)을 함수 안에서만 고정한다.
-- 그래서 부른 세션의 search_path와 임시 객체가 Stet의 읽기와 비교가 쓰는 함수, 연산자, 테이블을 정하지 못하고(함수와
-- 연산자는 pg_temp에서 찾지 않는다), 지금 행의 글자가 캡처가 남긴 글자와 같다. 카탈로그는 pg_catalog.로, 고객의 테이블은
-- relid로 찾은 스키마와 이름을 %I로 적는다.
-- 어느 테이블의 변경인지는 relid가 말한다. target_table은 트리거를 단 쪽이 고른 트리거의 첫 인자이고 relid는 서버가 적은
-- 것이다. tables는 target_table(설정의 테이블 키)로 고르고 결과에도 target_table을 보이지만, 행을 읽고 판단하는 것은 모두
-- relid로 한다. 한 키의 변경은 (target_table, relid, key_column, key)로 묶는다. 지우고 같은 이름으로 다시 만든 테이블은
-- relid가 달라 그 전의 변경이 table_gone이 된다. key_column까지 묶는 것은, 트리거를 다른 키 컬럼으로 다시 단 테이블에서
-- 두 컬럼의 같은 글자를 한 행으로 보지 않으려는 것이다.
-- 판단은 값으로만 한다. 비교는 캡처와 같은 식으로 만든 jsonb끼리 하고, 행이 없는 쪽은 NULL이다.

-- 거절 (7장 "거절하는 경우", 계획의 보정 U4). stet.undo_plan, stet.undo_selection, stet.undo가 먼저 부른다. SQLSTATE는
-- ST006이다. agent_role이나 since가 NULL이다. until이 since보다 앞이거나 같다(NULL이면 지금까지다). session_user가
-- agent_role이다: 자기 쓰기를 되돌리거나 그 기록을 읽을 수 없다.
CREATE OR REPLACE FUNCTION stet.undo_refuse(agent_role text, since timestamptz, until timestamptz) RETURNS void
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
BEGIN
  IF undo_refuse.agent_role IS NULL THEN
    RAISE EXCEPTION 'agent_role must be given: whose changes to undo' USING ERRCODE = 'ST006';
  END IF;
  IF undo_refuse.since IS NULL THEN
    RAISE EXCEPTION 'since must be given: undo the changes recorded from when' USING ERRCODE = 'ST006';
  END IF;
  IF undo_refuse.until <= undo_refuse.since THEN
    RAISE EXCEPTION 'until (%) must be after since (%)', undo_refuse.until, undo_refuse.since USING ERRCODE = 'ST006';
  END IF;
  IF session_user = undo_refuse.agent_role THEN
    RAISE EXCEPTION '% cannot undo its own changes', session_user USING ERRCODE = 'ST006';
  END IF;
END
$$;

-- 고르기 (7장 "고르기"). 검토자는 stet.change_sets와 stet.changes를 읽을 권한이 없으므로, 기록을 읽는 것은 이 함수뿐이고
-- 소유자(Stet의 계정)의 권한으로 돈다(SECURITY DEFINER). 그래서 이 함수로 읽을 수 있는 것을 좁힌다.
-- - 먼저 stet.undo_refuse로 네 거절을 한다. 에이전트가 이 함수의 권한을 받았어도 자기 기록은 읽지 못한다.
-- - 인자로 고른 것만 돌려준다: agent_role의 묶음 가운데 at이 since 이상 until 미만(until이 NULL이면 지금까지)이고, tables가
--   있으면 target_table이 그 안에 있는 것. 돌려주는 컬럼은 아래의 열둘뿐이다.
-- - 남긴 값(b, a)은 부른 세션이 그 테이블을 읽을 수 있을 때만 돌려준다. 곧 session_user나 session_user가 SET ROLE로 될 수
--   있는 역할 가운데 하나가 그 테이블의 SELECT를 가질 때다. 아니면 NULL이다. 읽을 수 없는 테이블의 지난 값을 이 함수로
--   보지 못하게 하려는 것이다. 이 함수를 부르는 stet.undo_plan과 stet.undo는 그런 테이블의 행을 읽다가 42501로 건너뛰므로
--   그 값이 필요 없다. 값이 NULL인 키는 어떤 판단으로도 되돌려지지 않는다(no_net_change나 changed_since가 된다).
--   키, key_changed, interleaved, 테이블 이름은 돌려준다. 계획이 읽을 수 없는 테이블의 키도 보여 줘야 하기 때문이다.
-- 값을 남긴 묶음의 줄은 (target_table, relid, key_column, key)마다 한 줄이다. 남긴 순서(변경의 id 순)로 본다.
--   b는 고른 첫 변경의 before, a는 마지막 변경의 after다. change_ids는 그 키의 변경들, first_change는 첫 변경의 id다.
--   key_changed는 고른 변경 가운데 update 묶음인데 before나 after가 없는 줄이 있는가다(키를 바꾼 UPDATE).
--   interleaved는 이어진 두 변경에서 앞의 after와 뒤의 before가 다른가다(행이 없는 것끼리는 같다).
-- 값을 남기지 않은 묶음(skipped)은 묶음마다 key 없이 한 줄이다. 값과 변경과 두 표시는 NULL이고 skipped가 그 이유다.
-- also_changed는 같은 문장(pid, txid, stmt_at이 같은 이 계정의 묶음)이 바꾼 테이블 가운데 tables가 고르지 않은 것의
-- target_table이다. 시각으로 거르지 않는다. 정렬해 돌려주고, 없으면 NULL이다.
-- 줄의 순서. 이 함수는 줄을 결과의 순서로 돌려준다. 부르는 쪽은 WITH ORDINALITY로 그 순서를 지킨다. target_table(바이트
--   순서, COLLATE "C"), 그다음 자리 순이다. 키의 줄의 자리는 first_change다. 값을 남기지 않은 묶음의 자리는 그 묶음 뒤에
--   처음 남은 변경, 곧 그 묶음보다 id가 큰 묶음 가운데 줄이 있는 첫 묶음의 가장 작은 변경 id다(그런 변경이 없으면 맨
--   뒤). 자리가 같으면 값을 남기지 않은 묶음이 먼저이고, 그것끼리는 묶음의 id 순이다. 그래서 그 줄은 자기 테이블의 줄
--   가운데 그 묶음이 남은 때에 선다.
CREATE OR REPLACE FUNCTION stet.undo_selection(agent_role text, since timestamptz, until timestamptz, tables text[])
RETURNS TABLE (target_table text, relid oid, key_column text, key text, b jsonb, a jsonb, change_ids bigint[],
               first_change bigint, key_changed boolean, interleaved boolean, skipped text, also_changed text[])
LANGUAGE plpgsql SECURITY DEFINER
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
BEGIN
  PERFORM stet.undo_refuse(undo_selection.agent_role, undo_selection.since, undo_selection.until);
  RETURN QUERY
  WITH sel AS (
    SELECT cs.id AS set_id, cs.target_table AS tt, cs.relid AS rel, cs.key_column AS kc, cs.op AS op,
           cs.skipped AS skip, cs.pid AS pid, cs.txid AS tx, cs.stmt_at AS stmt
      FROM stet.change_sets cs
     WHERE cs.agent_role = undo_selection.agent_role
       AND cs.at >= undo_selection.since
       AND (undo_selection.until IS NULL OR cs.at < undo_selection.until)
       AND (undo_selection.tables IS NULL OR (cs.target_table = ANY (undo_selection.tables)) IS TRUE)
  ),
  unselected AS (
    SELECT sel.set_id, array_agg(DISTINCT o.target_table) AS names
      FROM sel
      JOIN stet.change_sets o
        ON o.pid = sel.pid AND o.stmt_at = sel.stmt AND o.txid IS NOT DISTINCT FROM sel.tx
       AND o.agent_role = undo_selection.agent_role
     WHERE undo_selection.tables IS NOT NULL
       AND (o.target_table = ANY (undo_selection.tables)) IS NOT TRUE
     GROUP BY sel.set_id
  ),
  readable AS (
    SELECT r.rel,
           EXISTS (SELECT FROM pg_catalog.pg_roles ro
                    WHERE pg_catalog.pg_has_role(session_user, ro.oid, 'MEMBER')
                      AND pg_catalog.has_table_privilege(ro.oid, r.rel, 'SELECT')) AS ok
      FROM (SELECT DISTINCT sel.rel FROM sel) r
  ),
  lines AS (
    SELECT sel.set_id, sel.tt, sel.rel, sel.kc, ch.key AS k, ch.id AS change_id, ch.before AS bef, ch.after AS aft,
           sel.op = 'update' AND (ch.before IS NULL OR ch.after IS NULL) AS key_change,
           row_number() OVER w > 1 AND ch.before IS DISTINCT FROM lag(ch.after) OVER w AS broken
      FROM sel JOIN stet.changes ch ON ch.set_id = sel.set_id
     WHERE sel.skip IS NULL
    WINDOW w AS (PARTITION BY sel.tt, sel.rel, sel.kc, ch.key ORDER BY ch.id)
  ),
  keyed AS (
    SELECT l.tt, l.rel, l.kc, l.k,
           (array_agg(l.bef ORDER BY l.change_id))[1] AS b0,
           (array_agg(l.aft ORDER BY l.change_id DESC))[1] AS a0,
           array_agg(l.change_id ORDER BY l.change_id) AS ids,
           min(l.change_id) AS first_id,
           bool_or(l.key_change) AS kchg,
           bool_or(l.broken) AS brk
      FROM lines l
     GROUP BY l.tt, l.rel, l.kc, l.k
  ),
  keyed_unselected AS (
    SELECT ks.tt, ks.rel, ks.kc, ks.k, array_agg(DISTINCT nm.name) AS names
      FROM (SELECT DISTINCT l.tt, l.rel, l.kc, l.k, l.set_id FROM lines l) ks
      JOIN unselected u ON u.set_id = ks.set_id
     CROSS JOIN LATERAL unnest(u.names) AS nm(name)
     GROUP BY ks.tt, ks.rel, ks.kc, ks.k
  ),
  result AS (
    SELECT kd.tt, kd.rel, kd.kc, kd.k,
           CASE WHEN rd.ok THEN kd.b0 END AS b0, CASE WHEN rd.ok THEN kd.a0 END AS a0,
           kd.ids, kd.first_id, kd.kchg, kd.brk, NULL::text AS skip, ku.names,
           kd.first_id AS pos, 1 AS kind, NULL::bigint AS sid
      FROM keyed kd
      JOIN readable rd ON rd.rel = kd.rel
      LEFT JOIN keyed_unselected ku ON ku.tt = kd.tt AND ku.rel = kd.rel AND ku.kc = kd.kc AND ku.k = kd.k
    UNION ALL
    SELECT sel.tt, sel.rel, sel.kc, NULL::text, NULL::jsonb, NULL::jsonb,
           NULL::bigint[], NULL::bigint, NULL::boolean, NULL::boolean, sel.skip, u.names,
           (SELECT min(c2.id) FROM stet.changes c2
             WHERE c2.set_id = (SELECT min(c3.set_id) FROM stet.changes c3 WHERE c3.set_id > sel.set_id)),
           0, sel.set_id
      FROM sel LEFT JOIN unselected u ON u.set_id = sel.set_id
     WHERE sel.skip IS NOT NULL
  )
  SELECT rs.tt, rs.rel, rs.kc, rs.k, rs.b0, rs.a0, rs.ids, rs.first_id, rs.kchg, rs.brk, rs.skip,
         (SELECT array_agg(x.name ORDER BY x.name COLLATE pg_catalog."C") FROM unnest(rs.names) AS x(name))
    FROM result rs
   ORDER BY rs.tt COLLATE pg_catalog."C", rs.pos NULLS LAST, rs.kind, rs.sid;
END
$$;

-- 지금 그 키의 행 (7장 "비교는 남길 때와 같은 방법으로 만든 글자끼리 한다"). 행이 없으면 NULL이다. lock이면 FOR UPDATE로
-- 읽고(stet.undo), 아니면 아무것도 잠그지 않는다(stet.undo_plan). 부른 계정의 권한으로 읽는다(SECURITY INVOKER).
-- 그림은 stet.capture()와 같은 식, 같은 일곱 설정으로 만든다. 컬럼은 지금의 pg_attribute에서 attnum 순으로 읽고(지워진
-- 컬럼은 뺀다), 값은 CASE WHEN num_nulls(v) = 0 THEN format('%s', v) END다. 타입의 출력 함수(C, superuser만 만든다)이고
-- 캐스트가 아니다(계획의 판정 U10). 그래서 같은 행이 캡처가 남긴 것과 같은 jsonb가 된다. 캡처의 함수와 코드를 나누지
-- 않으므로 둘이 같다는 것은 테스트(TestUndoRowMatchesCapture)가 지킨다. 한쪽을 바꾸면 다른 쪽도 바꾼다.
-- 행은 키로 찾는다. 키 컬럼의 값이 키의 글자를 그 컬럼의 기본 타입의 리터럴로 읽은 것과 OPERATOR(pg_catalog.=)로 같은
-- 행이다. 키의 인덱스를 쓰고, 유일 제약과 복구 문장이 보는 것과 같은 같음이다. 글자는 다르지만 타입으로는 같은 값의
-- 행(키 1.50 자리에 1.5)도 그 키의 행이다. 그 행의 그림은 키 컬럼의 글자가 a와 달라 changed_since가 된다. 리터럴은 타입의
-- 입력 함수로 읽힌다. 글자에서 그 타입으로 가는 캐스트는 쓰지 않고, 도메인은 기본 타입으로 읽어 도메인의 CHECK를 돌리지
-- 않는다. 타입 이름은 format_type(타입, -1)이다(bpchar가 character(1)로 읽히지 않는다. pg_catalog에 없는 타입은
-- 스키마까지 나온다). 두 행 이상이 나오면 키 컬럼이 유일하지 않다는 에러다(cardinality_violation).
-- 컬럼 이름은 그대로 SQL에 들어가므로(r.<이름>), 그 이름이 컬럼이 아니면 r을 받는 함수로 읽힌다. 그래서 테이블을 먼저
-- 잠그고(ACCESS SHARE. SELECT 권한이 없으면 여기서 42501이다) 컬럼을 읽은 뒤, 읽은 이름이 모두 지금의 카탈로그(syscache)에도
-- 있고 그 뒤에 더해진 컬럼이 없는지 본다. REPEATABLE READ에서는 pg_attribute를 읽는 것이 트랜잭션의 스냅숏을 따르므로
-- 그 뒤에 바뀐 컬럼을 놓칠 수 있다. 잠근 뒤에는 테이블의 컬럼이 바뀌지 않는다.
CREATE OR REPLACE FUNCTION stet.undo_row(relid oid, key_column text, key text, lock boolean) RETURNS jsonb
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
DECLARE
  text_of   CONSTANT text := 'CASE WHEN num_nulls(r.%1$I) = 0 THEN format(''%%s'', r.%1$I) END';
  tbl       text;
  names     text[];
  img       text;
  last_att  smallint;
  typ       oid;
  kind      "char";
  base      oid;
  hit       record;
  hits      integer := 0;
  found_row jsonb;
BEGIN
  SELECT format('%I.%I', ns.nspname, cl.relname) INTO tbl
    FROM pg_catalog.pg_class cl JOIN pg_catalog.pg_namespace ns ON ns.oid = cl.relnamespace
   WHERE cl.oid = undo_row.relid AND cl.relkind = 'r';
  IF tbl IS NULL THEN
    RAISE EXCEPTION 'no table has the oid %', undo_row.relid USING ERRCODE = 'undefined_table';
  END IF;
  EXECUTE format('LOCK TABLE ONLY %s IN ACCESS SHARE MODE', tbl);

  SELECT array_agg(att.attname::text ORDER BY att.attnum),
         'jsonb_object($1, ARRAY[' || string_agg(format(text_of, att.attname), ', ' ORDER BY att.attnum) || '])',
         max(att.attnum),
         min(att.atttypid) FILTER (WHERE att.attname = undo_row.key_column)
    INTO names, img, last_att, typ
    FROM pg_catalog.pg_attribute att
   WHERE att.attrelid = undo_row.relid AND att.attnum > 0 AND NOT att.attisdropped;
  IF typ IS NULL THEN
    RAISE EXCEPTION 'table % has no column %', tbl, quote_ident(undo_row.key_column) USING ERRCODE = 'undefined_column';
  END IF;
  -- 읽은 컬럼이 지금의 카탈로그와 같은가. 없는 이름은 has_column_privilege가 에러를 낸다. attnum으로 물으면 없는 컬럼은 NULL이다.
  PERFORM pg_catalog.has_column_privilege(undo_row.relid, nm.name, 'SELECT') FROM unnest(names) AS nm(name);
  IF pg_catalog.has_column_privilege(undo_row.relid, (last_att + 1)::smallint, 'SELECT') IS NOT NULL THEN
    RAISE EXCEPTION 'the columns of % changed after this transaction''s snapshot', tbl
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  -- 도메인은 기본 타입으로
  LOOP
    SELECT ty.typtype, ty.typbasetype INTO kind, base FROM pg_catalog.pg_type ty WHERE ty.oid = typ;
    EXIT WHEN kind IS DISTINCT FROM 'd';
    typ := base;
  END LOOP;

  FOR hit IN EXECUTE format(
      'SELECT %s AS img FROM ONLY %s r WHERE r.%I OPERATOR(pg_catalog.=) %L::%s LIMIT 2%s',
      img, tbl, undo_row.key_column, undo_row.key, pg_catalog.format_type(typ, -1),
      CASE WHEN undo_row.lock THEN ' FOR UPDATE' ELSE '' END)
    USING names
  LOOP
    hits := hits + 1;
    IF hits > 1 THEN
      RAISE EXCEPTION 'more than one row of % has % as its %', tbl, quote_literal(undo_row.key), quote_ident(undo_row.key_column)
        USING ERRCODE = 'cardinality_violation';
    END IF;
    found_row := hit.img;
  END LOOP;
  RETURN found_row;
END
$$;

-- 키마다 하나의 결정 (7장 "키마다 하나의 결정"). 스펙의 여섯 확인을 그 순서로 하고 처음 걸린 것을 reason으로 돌려준다:
-- table_gone(relid가 보통 테이블을 가리키지 않는다), columns_changed(b나 a의 컬럼 이름들이 지금 테이블의 컬럼들과 다르거나
-- 키 컬럼이 없다. 계획의 보정 U1), key_changed, interleaved(둘은 stet.undo_selection이 남은 변경으로 정한다),
-- changed_since(지금 행이 a와 다르다. 없는 행은 NULL), no_net_change(b와 a가 같다). 하나도 걸리지 않으면 reason은 NULL이고
-- action이 b가 없으면 delete, a가 없으면 insert, 둘 다 있으면 update다. 걸리면 action은 NULL이다.
-- 지금 행은 stet.undo_row로 읽는다. lock은 그대로 넘긴다. 부른 계정의 권한으로 돈다(SECURITY INVOKER).
-- REPEATABLE READ에서는 pg_attribute를 트랜잭션의 스냅숏으로 읽으므로, 그 뒤에 바뀐 컬럼은 columns_changed로 잡히지 않고
-- stet.undo_row가 에러로 알린다.
CREATE OR REPLACE FUNCTION stet.undo_judge(relid oid, key_column text, key text, b jsonb, a jsonb,
                                           key_changed boolean, interleaved boolean, lock boolean)
RETURNS TABLE (action text, reason text)
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
DECLARE
  cols text[];
  img  jsonb;
BEGIN
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_class cl WHERE cl.oid = undo_judge.relid AND cl.relkind = 'r') THEN
    reason := 'table_gone';
  ELSE
    SELECT array_agg(att.attname::text) INTO cols
      FROM pg_catalog.pg_attribute att
     WHERE att.attrelid = undo_judge.relid AND att.attnum > 0 AND NOT att.attisdropped;
    IF (undo_judge.key_column = ANY (cols)) IS NOT TRUE THEN
      reason := 'columns_changed';
    END IF;
    FOREACH img IN ARRAY ARRAY[undo_judge.b, undo_judge.a] LOOP
      IF img IS NOT NULL
         AND ((SELECT count(*) FROM jsonb_object_keys(img)) <> cardinality(cols) OR (img ?& cols) IS NOT TRUE) THEN
        reason := 'columns_changed';
      END IF;
    END LOOP;
    IF reason IS NOT NULL THEN
      NULL;
    ELSIF undo_judge.key_changed THEN
      reason := 'key_changed';
    ELSIF undo_judge.interleaved THEN
      reason := 'interleaved';
    ELSIF stet.undo_row(undo_judge.relid, undo_judge.key_column, undo_judge.key, undo_judge.lock)
          IS DISTINCT FROM undo_judge.a THEN
      reason := 'changed_since';
    ELSIF undo_judge.b IS NOT DISTINCT FROM undo_judge.a THEN
      reason := 'no_net_change';
    END IF;
  END IF;
  action := CASE WHEN reason IS NOT NULL THEN NULL
                 WHEN undo_judge.b IS NULL THEN 'delete'
                 WHEN undo_judge.a IS NULL THEN 'insert'
                 ELSE 'update' END;
  RETURN NEXT;
END
$$;

-- 경고 (7장 "경고(warning)"). 되돌리기는 하되 검토자가 알아야 하는 것이다. action이 NULL이면(건너뛴 줄) 없다.
-- children_not_captured: insert로 되돌리는 테이블을 ON DELETE CASCADE, SET NULL, SET DEFAULT로 가리키는 외래 키의 자식
--   테이블 가운데, 그 연쇄가 일으키는 쓰기(CASCADE는 DELETE, 나머지는 UPDATE)에 켜진(ENABLE이나 ENABLE ALWAYS)
--   stet.capture() 트리거가 없는 것. 스키마까지 적고(%I.%I) 바이트 순으로 정렬해 ", "로 잇는다.
-- not_selected: also_changed(stet.undo_selection)의 테이블 키를 정렬해 ", "로 잇는다.
-- 둘 다 있으면 "; "로 잇는다. 카탈로그만 읽는다(SECURITY INVOKER).
CREATE OR REPLACE FUNCTION stet.undo_warning(relid oid, action text, also_changed text[]) RETURNS text
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
DECLARE
  children   text;
  unselected text;
BEGIN
  IF undo_warning.action IS NULL THEN
    RETURN NULL;
  END IF;
  IF undo_warning.action = 'insert' THEN
    SELECT string_agg(kid.name, ', ' ORDER BY kid.name COLLATE pg_catalog."C") INTO children
      FROM (SELECT DISTINCT format('%I.%I', ns.nspname, cl.relname) AS name
              FROM pg_catalog.pg_constraint fk
              JOIN pg_catalog.pg_class cl ON cl.oid = fk.conrelid
              JOIN pg_catalog.pg_namespace ns ON ns.oid = cl.relnamespace
             WHERE fk.contype = 'f' AND fk.confrelid = undo_warning.relid AND fk.confdeltype IN ('c', 'n', 'd')
               AND NOT EXISTS (SELECT FROM pg_catalog.pg_trigger tg
                                WHERE tg.tgrelid = fk.conrelid
                                  AND tg.tgfoid = 'stet.capture()'::pg_catalog.regprocedure
                                  AND tg.tgenabled IN ('O', 'A')
                                  AND (tg.tgtype::integer & CASE fk.confdeltype WHEN 'c' THEN 8 ELSE 16 END) <> 0)) kid;
  END IF;
  SELECT string_agg(t.name, ', ' ORDER BY t.name COLLATE pg_catalog."C") INTO unselected
    FROM (SELECT DISTINCT x.name FROM unnest(undo_warning.also_changed) AS x(name) WHERE x.name IS NOT NULL) t;
  RETURN nullif(concat_ws('; ', 'children_not_captured: ' || children, 'not_selected: ' || unselected), '');
END
$$;

-- 계획 (7장 "stet.undo_plan"). 무엇이 되돌려지고 무엇이 건너뛰어질지 말하고, 아무것도 바꾸거나 적거나 잠그지 않는다
-- (stet.undo_judge에 lock = false). 부른 계정의 권한으로 고객의 테이블을 읽는다(SECURITY INVOKER). 기록은
-- stet.undo_selection으로만 읽는다.
-- 줄은 stet.undo_selection의 순서(target_table, 그 키의 첫 변경 id)다. outcome은 would_restore, skipped, not_captured다.
-- 값을 남기지 않은 묶음은 key와 action이 NULL이고 reason이 그 skipped다. 경고는 되돌릴 줄(would_restore)에만 붙는다.
-- 에러를 내는 것은 네 거절(ST006)과 이 함수와 도우미의 권한이 없는 것(42501)뿐이다. 키마다의 판단은 블록 안에서 하고,
-- 거기서 난 에러(검토자에게 그 테이블의 SELECT가 없다, 키가 유일하지 않다 등)는 그 키를 skipped로, reason을
-- 'SQLSTATE <코드>: <메시지>'로 돌려준다. 블록은 하위 트랜잭션이지만 아무것도 쓰지 않으므로 트랜잭션 id를 받지 않는다.
-- 시간 초과와 취소는 잡지 않는다.
CREATE OR REPLACE FUNCTION stet.undo_plan(agent_role text, since timestamptz, until timestamptz DEFAULT NULL,
                                          tables text[] DEFAULT NULL)
RETURNS TABLE (target_table text, key text, action text, outcome text, reason text, warning text, change_ids bigint[])
LANGUAGE plpgsql
SET search_path = pg_catalog, pg_temp
SET DateStyle = 'ISO, MDY'
SET IntervalStyle = 'postgres'
SET extra_float_digits = 1
SET TimeZone = 'UTC'
SET bytea_output = 'hex'
SET client_min_messages = warning
AS $$
DECLARE
  s       record;
  code    text;
  message text;
BEGIN
  PERFORM stet.undo_refuse(undo_plan.agent_role, undo_plan.since, undo_plan.until);
  FOR s IN SELECT sel.*
             FROM stet.undo_selection(undo_plan.agent_role, undo_plan.since, undo_plan.until, undo_plan.tables)
                  WITH ORDINALITY AS sel
            ORDER BY sel.ordinality LOOP
    target_table := s.target_table;
    key := s.key;
    change_ids := s.change_ids;
    action := NULL;
    reason := NULL;
    warning := NULL;
    IF s.skipped IS NOT NULL THEN
      outcome := 'not_captured';
      reason := s.skipped;
    ELSE
      BEGIN
        SELECT j.action, j.reason INTO action, reason
          FROM stet.undo_judge(s.relid, s.key_column, s.key, s.b, s.a, s.key_changed, s.interleaved, false) j;
        outcome := CASE WHEN reason IS NULL THEN 'would_restore' ELSE 'skipped' END;
        warning := stet.undo_warning(s.relid, action, s.also_changed);
      EXCEPTION WHEN OTHERS THEN
        GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, message = MESSAGE_TEXT;
        action := NULL;
        outcome := 'skipped';
        reason := 'SQLSTATE ' || code || ': ' || message;
        warning := NULL;
      END;
    END IF;
    RETURN NEXT;
  END LOOP;
END
$$;

-- 되돌리기의 함수들은 PUBLIC이 부를 수 없다(7장 "권한"). 테이블 소유자도 stet 스키마의 USAGE를 가지므로(4장) 열어 두면
-- stet.undo_selection으로 남긴 값을 읽을 수 있다. 누가 계획을 볼 수 있는지는 운영자가 정한다. stet.undo_plan을 쓰는
-- 검토자에게는 이것을 준다. 도우미는 계획이 검토자의 권한으로 부르므로 그 EXECUTE도 있어야 한다:
--   GRANT USAGE ON SCHEMA stet TO reviewer;
--   GRANT EXECUTE ON FUNCTION stet.undo_plan(text, timestamptz, timestamptz, text[]),
--                             stet.undo_selection(text, timestamptz, timestamptz, text[]),
--                             stet.undo_row(oid, text, text, boolean),
--                             stet.undo_judge(oid, text, text, jsonb, jsonb, boolean, boolean, boolean),
--                             stet.undo_warning(oid, text, text[]),
--                             stet.undo_refuse(text, timestamptz, timestamptz) TO reviewer;
-- 그리고 되돌릴 테이블마다 SELECT다(GRANT SELECT ON public.deals TO reviewer). 없는 테이블의 키는 SQLSTATE 42501로 건너뛴다.
-- stet.change_sets와 stet.changes의 권한은 주지 않는다. 이 함수들은 아무것도 바꾸거나 적지 않는다.
REVOKE EXECUTE ON FUNCTION stet.undo_plan(text, timestamptz, timestamptz, text[]),
                           stet.undo_selection(text, timestamptz, timestamptz, text[]),
                           stet.undo_row(oid, text, text, boolean),
                           stet.undo_judge(oid, text, text, jsonb, jsonb, boolean, boolean, boolean),
                           stet.undo_warning(oid, text, text[]),
                           stet.undo_refuse(text, timestamptz, timestamptz) FROM PUBLIC;
