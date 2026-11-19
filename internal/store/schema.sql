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
-- 메시지가 서버에 닿을 때 시작하므로, 그 뒤에 이 표의 잠금을 기다려도 시각이 늦어지지 않는다.
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS backend_pid integer;      -- 그 세션의 서버 쪽 백엔드 pid
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS sent_after  timestamptz;  -- DB 시계. 문장은 이보다 뒤에 서버로 갔다
ALTER TABLE stet.decisions ADD COLUMN IF NOT EXISTS done_before timestamptz;  -- DB 시계. 문장의 결과는 이보다 앞에 나왔다
-- stet.history가 묶음마다 후보를 찾는 인덱스. 창의 두 끝이 모두 인덱스 안에 있어 힙을 읽지 않고 거른다. done_before가
-- 앞이므로 묶음의 stmt_at 이후로 끝난 것만 훑는다. 검토와 되돌리기는 대개 최근의 변경을 읽는다. 이 단계의 앞선 판을
-- 실행한 데이터베이스에는 두 컬럼의 decisions_backend_idx가 남아 있다. 그것을 지운다. 같은 이름으로는 IF NOT EXISTS가
-- 옛 인덱스를 그대로 두기 때문에 이름을 바꿨다.
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
  roles     text[],                            -- 캡처할 계정. NULL이면 모든 세션. Stet의 계정이 접속한 세션은 거르지 않는다
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
-- 겹치지 않게 하려는 것이다(아래).
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
-- 삼키면 에이전트가 일으켜 쓰기를 기록도 표지도 없이 커밋시킬 수 있는 에러는 쓰기로 되돌려 보낸다(4장). 쓰기가
-- 실패해 다시 보내지는 것은 안전하고, 기록 없이 커밋되는 것은 안전하지 않다.
-- - serialization_failure(40001)는 두 블록 모두 되돌려 보낸다. SERIALIZABLE에서는 캡처가 stet의 표를 읽고 쓰는 것이 세션
--   사이에 충돌하고, 에이전트는 두 세션으로 이것을 일으킨다. 표지를 쓰는 블록도 같은 충돌에 걸린다.
-- - 자원과 한도(53, 54 부류, 예를 들어 깊은 함수 호출 끝에서 캡처만 스택이 모자라는 54001)는 아무것도 남길 수 없을 때,
--   곧 표지를 쓰는 블록이 실패했거나 세는 것이 실패했을 때만 되돌려 보낸다. 큰 블록만 실패하면 표지가 남는다.
-- - 교착(40P01)은 삼킨다. 캡처는 stet의 표의 잠금만 기다리고, 에이전트는 그 표를 잠글 수 없으므로 캡처가 낀 교착을 만들 수
--   없다. REPEATABLE READ의 40001도 같은 코드라 되돌려 보내지만, 캡처의 문장은 그것을 내지 않는다. 캡처는 넣고 읽기만 하고,
--   행을 잠그는 것은 줄을 넣을 때의 외래 키 확인(FOR KEY SHARE)뿐인데 그것은 이 트랜잭션이 방금 넣은 묶음의 줄을 잠근다.
--   다른 트랜잭션이 고친 행을 잠그거나 고치는 일이 없다.
-- 한 트랜잭션의 상한. EXCEPTION 블록은 하위 트랜잭션이고, 표에 쓸 때만 자기 트랜잭션 id를 받는다. 커밋된 하위 트랜잭션의
-- id는 트랜잭션이 끝날 때까지 백엔드의 캐시(64개)에 남고, 넘치면 서버 전체의 스냅숏 확인이 느려진다. 되돌려진 하위
-- 트랜잭션은 자리를 돌려준다. 표에 쓰는 캡처 하나는 id 하나만 남긴다: 값이나 표지를 남기면 큰 블록의 것, 실패하면 큰 블록은
-- 되돌려지고 표지를 쓴 작은 블록의 것이다. 그래서 한 트랜잭션에서 표에 쓰는 것은 32번까지다. 31번 썼으면 다음에 쓰는 것은
-- too_many_statements 표지 하나이고, 그 뒤로는 아무것도 쓰지 않는다.
-- 쓴 횟수(done)는 이 백엔드가 이 트랜잭션의 id로 남긴 묶음의 수다(change_sets_txid_idx). 한 트랜잭션의 묶음은 모두 한
-- 백엔드의 것이다. pid도 보는 것은 다른 클러스터에서 옮겨 온 묶음(pg_dump, 논리 복제)이 같은 txid를 가질 수 있어서다.
-- 겹칠 수는 여전히 있지만 드물다.
-- 캡처할 때마다 센다. 세션의 값(설정)으로 세지 않는다. 에이전트가 적을 수 있는 값이어서, 낮추면 상한을 넘어 표지 없이
-- 남기지 않게 되고 높이면 캡처가 꺼진다. 되돌려진 쓰기의 묶음은 보이지 않으므로 세지 않는다. 그 하위 트랜잭션도 자리를
-- 돌려주었다.
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
  own           boolean := false;  -- Stet의 계정이 접속한 세션이다
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
  several       boolean := false;  -- UPDATE가 한 키의 행을 여러 번 바꿨다
  versions      text;
  line_count    bigint;
  key_count     bigint;
  after_count   bigint;
  twice         text;
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
      own := true;
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

    -- 3. 캡처할 계정이 아니면 아무것도 하지 않는다. Stet의 계정이 접속한 세션은 roles로 거르지 않는다. 승인된 문장은 그
    --    세션에서 에이전트의 역할로 도는데, 그 문장이 부른 함수는 역할을 none으로 돌려 쓰고 다시 맞춰 둘 수 있다. roles로
    --    거르면 그 쓰기가 묶음도 표지도 없이 커밋된다. 그래서 그 세션의 쓰기는 그때의 역할로, 역할이 none이면 Stet의
    --    계정으로 남긴다(2).
    IF capture_roles IS NOT NULL AND NOT own AND (agent = ANY (capture_roles)) IS NOT TRUE THEN
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
      -- 세는 문장과 값을 읽는 문장은 따로 이름을 찾지만, 그 사이에도 고객의 코드는 돌지 않는다. 한 행이 여러 번 바뀌었는지
      -- 보는 문장과 그 판들을 줄이는 문장도 전이 테이블만 읽고, 글자와 jsonb를 내장 연산자로 견준다.
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

    -- 6. 값을 남길 문장의 재료. 값은 jsonb_object(컬럼 이름의 배열, 값의 글자의 배열)이다. 지워진 컬럼은 뺀다.
    IF skip IS NULL THEN
      SELECT array_agg(a.attname::text ORDER BY a.attnum),
             'jsonb_object($2, ARRAY[' || string_agg(format(text_of, a.attname), ', ' ORDER BY a.attnum) || '])'
        INTO names, img
        FROM pg_catalog.pg_attribute a
       WHERE a.attrelid = TG_RELID AND a.attnum > 0 AND NOT a.attisdropped;
      keyval := format(text_of, TG_ARGV[1]);
      -- 한 문장이 한 행을 여러 번 바꾸면(같은 문장이 지운 행들을 그 행의 외래 키 여럿이 가리켜 SET NULL이 잇달아 돈다 등)
      -- 전이 테이블에 그 키의 판마다 한 줄이 있다. 한 행만 바꾼 문장에는 그런 키가 없으므로 묻지 않는다. 있으면 그 문장이
      -- 남길 줄을 먼저 센다(아래의 versions). rows는 남기는 줄로 센다: 뒤의 값이 있는 줄의 수이고, 여러 번 바뀐 행도 한 번이다.
      IF TG_OP = 'UPDATE' AND changed > 1 THEN
        EXECUTE format('SELECT EXISTS (SELECT FROM stet_old r GROUP BY %1$s HAVING count(*) > 1)
                            OR EXISTS (SELECT FROM stet_new r GROUP BY %1$s HAVING count(*) > 1)', keyval)
           INTO several;
      END IF;
      IF several THEN
        -- b와 a는 이전 행들과 이후 행들, m은 어느 한쪽에 두 번 이상 나오는 키다. m에 없는 키는 아래의 보통 길과 똑같이
        -- 키로 묶는다. m의 키는 키마다, 이전 그림들에서 이후 그림들을 하나씩 지우고(EXCEPT ALL) 이후 그림들에서 이전
        -- 그림들을 하나씩 지운다. 사이의 판은 한 번은 이후로, 한 번은 이전으로 나오므로 사라지고, 남는 것이 문장 전의
        -- 행과 문장 뒤의 행이다. 바뀌었다가 돌아온 키는 아무것도 남지 않아 줄이 없다. 한쪽에 둘 이상 남는 키는 한 행의
        -- 판들이 아니다(키 컬럼이 유일하지 않다). 그때는 에러를 내어 표지를 남긴다. 키가 NULL인 행은 m에 넣지 않고
        -- 보통 길로 보내, 지금처럼 줄을 넣을 때 실패하게 한다.
        versions := format(
          'WITH b AS MATERIALIZED (SELECT %1$s AS k, %2$s AS img FROM stet_old r),
                a AS MATERIALIZED (SELECT %1$s AS k, %2$s AS img FROM stet_new r),
                m AS MATERIALIZED (SELECT b.k FROM b WHERE b.k IS NOT NULL GROUP BY b.k HAVING count(*) > 1
                                   UNION
                                   SELECT a.k FROM a WHERE a.k IS NOT NULL GROUP BY a.k HAVING count(*) > 1),
                l AS MATERIALIZED (
                  SELECT k, b1.img AS before, a1.img AS after
                    FROM (SELECT b.k, b.img FROM b WHERE b.k IS NULL OR b.k NOT IN (SELECT m.k FROM m)) b1
                    FULL JOIN (SELECT a.k, a.img FROM a WHERE a.k IS NULL OR a.k NOT IN (SELECT m.k FROM m)) a1 USING (k)
                  UNION ALL
                  SELECT k, b2.img, a2.img
                    FROM (SELECT b.k, b.img FROM b WHERE b.k IN (SELECT m.k FROM m)
                          EXCEPT ALL
                          SELECT a.k, a.img FROM a WHERE a.k IN (SELECT m.k FROM m)) b2
                    FULL JOIN (SELECT a.k, a.img FROM a WHERE a.k IN (SELECT m.k FROM m)
                               EXCEPT ALL
                               SELECT b.k, b.img FROM b WHERE b.k IN (SELECT m.k FROM m)) a2 USING (k)) ', keyval, img);
        EXECUTE versions || 'SELECT count(*), count(DISTINCT l.k) + count(*) FILTER (WHERE l.k IS NULL),
                                    count(*) FILTER (WHERE l.after IS NOT NULL),
                                    (SELECT d.k FROM l d WHERE d.k IS NOT NULL GROUP BY d.k HAVING count(*) > 1 LIMIT 1)
                               FROM l'
           INTO line_count, key_count, after_count, twice USING set_id, names;
        IF line_count <> key_count THEN
          RAISE EXCEPTION 'the statement changed several rows of %.% that have % as their %, so what it recorded would not be one row before and one after for that key',
            TG_TABLE_SCHEMA, TG_TABLE_NAME, quote_literal(twice), quote_ident(TG_ARGV[1]) USING ERRCODE = 'cardinality_violation';
        END IF;
        IF line_count = 0 THEN
          RETURN NULL;
        END IF;
        changed := after_count;
      END IF;
    END IF;

    -- 6. 묶음 하나
    INSERT INTO stet.change_sets (at, stmt_at, txid, pid, login_role, agent_role, app, held_write,
                                  target_table, key_column, relid, op, query, rows, skipped)
    VALUES (clock_timestamp(), statement_timestamp(), txid_current(), pg_backend_pid(), session_user, agent,
            current_setting('application_name'), held,
            TG_ARGV[0], TG_ARGV[1], TG_RELID, lower(TG_OP), current_query(), changed, skip)
    RETURNING id INTO set_id;

    -- 6. 키마다 한 줄.
    IF skip IS NULL THEN
      IF TG_OP = 'INSERT' THEN
        EXECUTE format('INSERT INTO stet.changes (set_id, key, after) SELECT $1, %s, %s FROM stet_new r', keyval, img)
          USING set_id, names;
      ELSIF TG_OP = 'DELETE' THEN
        EXECUTE format('INSERT INTO stet.changes (set_id, key, before) SELECT $1, %s, %s FROM stet_old r', keyval, img)
          USING set_id, names;
      ELSIF several THEN
        EXECUTE versions || 'INSERT INTO stet.changes (set_id, key, before, after) SELECT $1, l.k, l.before, l.after FROM l'
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
-- 기록을 지키는 트리거의 함수는 아무에게도 주지 않는다. 트리거는 함수의 권한 없이도 불린다. 판단 기록과 보류 건을 지키는
-- 두 함수도 같다. 그래서 stet 스키마에는 PUBLIC이 부를 수 있는 함수가 없다.
REVOKE EXECUTE ON FUNCTION stet.changes_guard() FROM PUBLIC;
REVOKE EXECUTE ON FUNCTION stet.decisions_append_only(), stet.held_writes_guard() FROM PUBLIC;

-- 되돌리기의 기록 (docs/spec-v0.2-undo.md 7장 "기록"). stet.undo가 부를 때마다 stet.undos에 한 줄, 결과의 줄마다
-- stet.undo_items에 한 줄, 되돌린 변경마다 stet.undone_changes에 한 줄을 검토자의 트랜잭션 안에서 남긴다
-- (stet.undo_record). 롤백하면 함께 사라진다. 아래의 stet.history가 읽으므로 뷰보다 먼저 만든다. 적는 함수는 아래의
-- "되돌리기: stet.undo" 절에 있다.
-- 세 표는 stet.change_sets와 stet.changes를 외래 키로 가리키지 않는다. 보존 기간(9장)이 오래된 변경을 지울 때 이 표들을
-- 확인하거나 잠그지 않고, 지운 변경의 되돌리기 기록도 남는다. change_ids와 change_id는 기록할 때의 변경 id일 뿐이다.
-- 소유자(Stet의 계정) 말고는 아무 권한도 없다. 검토자는 stet.history로 본다.
CREATE TABLE IF NOT EXISTS stet.undos (
  id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
  at          timestamptz NOT NULL,          -- 기록한 때. clock_timestamp()
  by          text NOT NULL,                 -- 부른 접속 계정(session_user). 인자로 받지 않는다
  agent_role  text NOT NULL,                 -- 고른 조건: 계정, 시간 범위, 테이블
  since       timestamptz NOT NULL,
  until       timestamptz,
  tables      text[],
  note        text,                          -- 검토자가 적은 말
  restored    integer NOT NULL,              -- restored와 restored_differs의 수
  skipped     integer NOT NULL,              -- skipped와 not_captured의 수
  failed      integer NOT NULL
);
-- 결과의 줄 하나. line은 결과에서의 차례(1부터)다
CREATE TABLE IF NOT EXISTS stet.undo_items (
  undo_id       bigint NOT NULL REFERENCES stet.undos (id) ON DELETE CASCADE,
  line          integer NOT NULL,
  target_table  text NOT NULL,
  key           text,
  action        text CHECK (action IN ('delete','insert','update')),
  outcome       text NOT NULL CHECK (outcome IN ('restored','restored_differs','skipped','failed','not_captured')),
  reason        text,
  warning       text,
  change_ids    bigint[],
  PRIMARY KEY (undo_id, line)
);
-- 되돌린 변경. 되돌린 줄(restored, restored_differs)의 change_ids를 변경 하나에 한 줄로 펼친 것이고 stet.undo_record가
-- 함께 적는다. stet.history가 변경마다 그것을 되돌린 마지막 되돌리기를 기본 키(change_id, undo_id)의 한 번의 역방향 탐색으로
-- 찾게 하려는 것이다. undo_items의 change_ids에 GIN 인덱스를 두면 플래너가 ORDER BY undo_id DESC LIMIT 1 때문에
-- undo_items의 기본 키를 거꾸로 훑으며 줄마다 change_ids를 거르는 계획을 골라, 변경마다 줄을 거의 다 읽었다(테스트 서버).
CREATE TABLE IF NOT EXISTS stet.undone_changes (
  change_id  bigint NOT NULL,
  undo_id    bigint NOT NULL REFERENCES stet.undos (id) ON DELETE CASCADE,
  PRIMARY KEY (change_id, undo_id)
);

-- 검토자가 보는 것 (docs/spec-v0.2-undo.md 6장). 캡처한 변경마다 한 줄이고, 값을 남기지 않은 묶음(skipped)은 key 없이
-- 한 줄이다. 변경을 그 쓰기를 판단한 기록에 잇는다(5장의 "잇는 규칙").
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
-- undo_id와 undone_at은 그 변경을 restored나 restored_differs로 되돌린 마지막 되돌리기(7장)의 id와 기록한 때다. 변경마다
-- stet.undone_changes의 기본 키로 그 change_id의 가장 큰 undo_id 하나를 찾는다(LATERAL). 건너뛰거나 실패한 줄은 되돌린
-- 것이 아니므로 거기에 없다. 값을 남기지 않은 묶음의 줄(change_id가 NULL)은 아무것도 찾지 않는다.
-- CREATE OR REPLACE VIEW는 컬럼을 끝에 더하는 것만 받으므로 컬럼의 순서와 타입은 6장의 표 그대로 둔다.
CREATE OR REPLACE VIEW stet.history AS
SELECT c.id AS change_id, s.id AS set_id, s.at,
       s.agent_role, s.login_role, s.app,
       s.target_table, s.op, c.key, c.before, c.after,
       s.rows, s.skipped, s.detail, s.query,
       s.held_write,
       d.id AS decision_id, link.candidates AS decision_candidates,
       d.verdict, d.reasons,
       ud.id AS undo_id, ud.at AS undone_at
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
LEFT JOIN stet.decisions d ON d.id = link.decision_id
LEFT JOIN LATERAL (
  SELECT u.id, u.at
    FROM stet.undone_changes uc
    JOIN stet.undos u ON u.id = uc.undo_id
   WHERE uc.change_id = c.id
   ORDER BY uc.undo_id DESC
   LIMIT 1
) ud ON true;

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

-- 거절 (7장 "거절하는 경우"와 "READ COMMITTED에서만"). stet.undo_plan, stet.undo_selection, stet.undo가 먼저 부른다.
-- SQLSTATE는 ST006이다. agent_role이나 since가 NULL이다. until이 since보다 앞이거나 같다(NULL이면 지금까지다).
-- session_user가 agent_role이다: 자기 쓰기를 되돌리거나 그 기록을 읽을 수 없다. 트랜잭션의 격리 수준이 READ COMMITTED가
-- 아니다: REPEATABLE READ와 SERIALIZABLE에서는 카탈로그를 SQL로 읽으면 트랜잭션의 스냅숏을 따르고 SQL의 이름은 지금의
-- 카탈로그에서 풀려서, 스냅숏 뒤에 이름을 바꾸거나 다시 만든 테이블과 바뀐 컬럼을 맞추는 확인마다 빈틈이 남았다. READ
-- COMMITTED에서는 함수 안의 문장마다 시작할 때의 스냅숏으로 읽는다. 설정값이 read committed가 아니면 모두 거절한다
-- (PostgreSQL이 READ COMMITTED처럼 다루는 READ UNCOMMITTED도). 인자가 없어 이 함수를 부를 수 없는 stet.undo_lock(stet.undo_row와
-- stet.undo_statement가 부른다), stet.undo_judge, stet.undo_referenced도 같은 말로 이 확인을 한다.
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
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'undo runs only in a READ COMMITTED transaction; this one is %',
      upper(current_setting('transaction_isolation'))
      USING ERRCODE = 'ST006', HINT = 'Call it inside BEGIN ISOLATION LEVEL READ COMMITTED; ... COMMIT.';
  END IF;
END
$$;

-- 고르기 (7장 "고르기"). 검토자는 stet.change_sets와 stet.changes를 읽을 권한이 없으므로, 기록을 읽는 것은 이 함수뿐이고
-- 소유자(Stet의 계정)의 권한으로 돈다(SECURITY DEFINER). 그래서 이 함수로 읽을 수 있는 것을 좁힌다.
-- - 먼저 stet.undo_refuse로 다섯 거절을 한다. 에이전트가 이 함수의 권한을 받았어도 자기 기록은 읽지 못한다.
-- - 인자로 고른 것만 돌려준다: agent_role의 묶음 가운데 at이 since 이상 until 미만(until이 NULL이면 지금까지)이고, tables가
--   있으면 target_table이 그 안에 있는 것. 돌려주는 컬럼은 아래의 열셋뿐이다. 부른 쪽의 식이나 이름을 평가하지 않는다.
-- - 부른 세션이 읽을 수 없는 테이블은 키를 하나도 보이지 않는다(7장 "고르기"). 키도 데이터다(이메일 같은 자연 키).
--   그 (target_table, relid)는 key 없는 한 줄이고, 키 컬럼, 값, 변경, 표시, also_changed가 모두 NULL이며 withheld가
--   'SQLSTATE 42501: permission denied for table <이름>'이다. 읽을 수 있다는 것은 session_user나 session_user가 SET ROLE로
--   될 수 있는 역할 가운데 하나가 그 테이블의 SELECT를 갖고 그 역할에게 행 수준 보안이 적용되지 않는 것이다. 곧 테이블의
--   행 수준 보안이 꺼져 있거나, 그 역할이 superuser나 BYPASSRLS이거나, 테이블 소유자의 권한을 갖고 테이블이 FORCE ROW
--   LEVEL SECURITY가 아니다. 정책이 실제로 무엇을 숨기는지는 보지 않는다. 켜져 있으면 숨길 수 있다고 본다. SET ROLE로 될
--   수 있는 역할까지 보는 것은, SECURITY DEFINER 안에서는 부른 쪽의 current_user를 알 수 없기 때문이다. 그 세션이 스스로
--   얻을 수 있는 권한이다. 이 판단은 이 함수의 문장이 시작할 때의 카탈로그로 한다(READ COMMITTED만 받는다).
--   그 뒤에 켜진 행 수준 보안과, 판단한 역할이 아닌 지금의 역할에게 적용되는 행 수준 보안은 stet.undo_lock이 그 테이블을
--   잠근 뒤에 42501로 거절한다(stet.undo_judge와 stet.undo_row가 부른다. stet.undo_row는 행을 읽은 뒤에 한 번 더 본다).
-- - relid가 더는 보통 테이블을 가리키지 않으면 그 (target_table, relid)도 key 없는 한 줄이고 withheld가 'table_gone'이다.
--   지켜 줄 테이블의 권한이 없으므로 키를 보이지 않는다.
-- 읽을 수 있는 테이블의 줄은 이렇다. withheld는 NULL이다.
-- - 값을 남긴 묶음의 줄은 (target_table, relid, key_column, key)마다 한 줄이다. 남긴 순서(변경의 id 순)로 본다.
--   b는 고른 첫 변경의 before, a는 마지막 변경의 after다(그 두 변경만 id로 읽는다). change_ids는 그 키의 변경들,
--   first_change는 첫 변경의 id다. key_changed는 고른 변경 가운데 키를 바꾼 UPDATE의 묶음에 든 것이 있는가다. 키를 바꾼
--   UPDATE의 묶음은 줄 가운데 before나 after가 없는 것이 하나라도 있는 update 묶음이고, 그 묶음의 줄은 양쪽이 다 있는
--   것까지 모두 그렇게 본다. 키를 하나씩 민 UPDATE(id = id + 1)에서는 가운데 키의 줄에 before와 after가 다 있지만 둘은
--   서로 다른 행이다. 그 키만 되돌리면 한 행의 값이 둘이 되고 다른 행의 값은 사라진다.
--   interleaved는 이어진 두 변경에서 앞의 after와 뒤의 before가 다른가다(행이 없는 것끼리는 같다).
-- - 값을 남기지 않은 묶음(skipped)은 묶음마다 key 없이 한 줄이다. 값과 변경과 두 표시는 NULL이고 skipped가 그 이유다.
-- - also_changed는 같은 문장(pid, txid, stmt_at이 같은 이 계정의 묶음)이 바꾼 테이블 가운데 tables가 고르지 않은 것의
--   target_table이다. 시각으로 거르지 않는다. 정렬해 돌려주고, 없으면 NULL이다.
-- 줄의 순서. 이 함수는 줄을 결과의 순서로 돌려준다. 부르는 쪽은 WITH ORDINALITY로 그 순서를 지킨다. target_table(바이트
--   순서, COLLATE "C"), 그다음 자리 순이다. 키의 줄의 자리는 first_change다. 값을 남기지 않은 묶음의 자리는 그 묶음 뒤에
--   처음 남은 변경, 곧 그 묶음보다 id가 큰 묶음 가운데 줄이 있는 첫 묶음의 가장 작은 변경 id다(그런 변경이 없으면 맨
--   뒤). 보이지 않는 테이블의 한 줄의 자리는 그 테이블의 줄들이 가졌을 자리 가운데 가장 앞이다. 자리가 같으면 key 없는
--   줄이 먼저이고, 그것끼리는 묶음의 id 순이다. 그래서 key 없는 줄은 자기 테이블의 줄 가운데 그 묶음이 남은 때에 선다.
-- 이 단계의 앞선 판을 실행한 데이터베이스에는 withheld가 없는 이 함수가 남아 있다. CREATE OR REPLACE는 돌려주는 컬럼을
-- 바꾸지 못하므로 그 모양일 때만 지운다. 그 밖에는 지우지 않아 운영자가 준 EXECUTE가 남는다. 이 문장을 빼면 그런
-- 데이터베이스에서 이 파일이 실패한다.
DO $$
BEGIN
  IF EXISTS (SELECT FROM pg_catalog.pg_proc p
              WHERE p.oid = pg_catalog.to_regprocedure('stet.undo_selection(text, timestamptz, timestamptz, text[])')
                AND ('withheld' = ANY (p.proargnames)) IS NOT TRUE) THEN
    DROP FUNCTION stet.undo_selection(text, timestamptz, timestamptz, text[]);
  END IF;
END
$$;

CREATE OR REPLACE FUNCTION stet.undo_selection(agent_role text, since timestamptz, until timestamptz, tables text[])
RETURNS TABLE (target_table text, relid oid, key_column text, key text, b jsonb, a jsonb, change_ids bigint[],
               first_change bigint, key_changed boolean, interleaved boolean, skipped text, also_changed text[],
               withheld text)
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
  -- 테이블마다: 없어졌으면 table_gone, 부른 세션이 읽을 수 없으면 42501, 아니면 NULL
  tabs AS (
    SELECT r.rel,
           CASE
             WHEN NOT EXISTS (SELECT FROM pg_catalog.pg_class c WHERE c.oid = r.rel AND c.relkind = 'r') THEN 'table_gone'
             WHEN NOT EXISTS (
                    SELECT FROM pg_catalog.pg_class c, pg_catalog.pg_roles ro
                     WHERE c.oid = r.rel
                       AND pg_catalog.pg_has_role(session_user, ro.oid, 'MEMBER')
                       AND pg_catalog.has_table_privilege(ro.oid, r.rel, 'SELECT')
                       AND (ro.rolsuper OR ro.rolbypassrls
                            OR NOT c.relrowsecurity
                            OR (pg_catalog.pg_has_role(ro.oid, c.relowner, 'USAGE') AND NOT c.relforcerowsecurity)))
               THEN 'SQLSTATE 42501: permission denied for table '
                    || (SELECT c.relname::text FROM pg_catalog.pg_class c WHERE c.oid = r.rel)
           END AS why
      FROM (SELECT DISTINCT sel.rel FROM sel) r
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
  -- 키를 바꾼 UPDATE의 묶음: 줄 가운데 before나 after가 없는 것이 있다
  rekeyed AS (
    SELECT DISTINCT sel.set_id
      FROM sel JOIN stet.changes ch ON ch.set_id = sel.set_id
     WHERE sel.op = 'update' AND sel.skip IS NULL AND (ch.before IS NULL OR ch.after IS NULL)
  ),
  lines AS (
    SELECT sel.set_id, sel.tt, sel.rel, sel.kc, ch.key AS k, ch.id AS change_id,
           sel.set_id IN (SELECT rk.set_id FROM rekeyed rk) AS key_change,
           row_number() OVER w > 1 AND ch.before IS DISTINCT FROM lag(ch.after) OVER w AS broken
      FROM sel JOIN stet.changes ch ON ch.set_id = sel.set_id
     WHERE sel.skip IS NULL
    WINDOW w AS (PARTITION BY sel.tt, sel.rel, sel.kc, ch.key ORDER BY ch.id)
  ),
  keyed AS (
    SELECT l.tt, l.rel, l.kc, l.k,
           array_agg(l.change_id ORDER BY l.change_id) AS ids,
           min(l.change_id) AS first_id,
           max(l.change_id) AS last_id,
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
    SELECT kd.tt, kd.rel, kd.kc, kd.k, kd.ids, kd.first_id, kd.last_id, kd.kchg, kd.brk, NULL::text AS skip, ku.names,
           kd.first_id AS pos, 1 AS kind, NULL::bigint AS sid
      FROM keyed kd
      LEFT JOIN keyed_unselected ku ON ku.tt = kd.tt AND ku.rel = kd.rel AND ku.kc = kd.kc AND ku.k = kd.k
    UNION ALL
    SELECT sel.tt, sel.rel, sel.kc, NULL::text, NULL::bigint[], NULL::bigint, NULL::bigint, NULL::boolean, NULL::boolean,
           sel.skip, u.names,
           (SELECT min(c2.id) FROM stet.changes c2
             WHERE c2.set_id = (SELECT min(c3.set_id) FROM stet.changes c3 WHERE c3.set_id > sel.set_id)),
           0, sel.set_id
      FROM sel LEFT JOIN unselected u ON u.set_id = sel.set_id
     WHERE sel.skip IS NOT NULL
  ),
  shown AS (
    SELECT rs.tt, rs.rel, rs.kc, rs.k, fb.before AS b0, la.after AS a0, rs.ids, rs.first_id, rs.kchg, rs.brk,
           rs.skip, rs.names, NULL::text AS why, rs.pos, rs.kind, rs.sid
      FROM result rs
      JOIN tabs tb ON tb.rel = rs.rel AND tb.why IS NULL
      LEFT JOIN stet.changes fb ON fb.id = rs.first_id
      LEFT JOIN stet.changes la ON la.id = rs.last_id
    UNION ALL
    SELECT rs.tt, rs.rel, NULL::text, NULL::text, NULL::jsonb, NULL::jsonb, NULL::bigint[], NULL::bigint,
           NULL::boolean, NULL::boolean, NULL::text, NULL::text[], tb.why, min(rs.pos), 0, min(rs.sid)
      FROM result rs
      JOIN tabs tb ON tb.rel = rs.rel AND tb.why IS NOT NULL
     GROUP BY rs.tt, rs.rel, tb.why
  )
  SELECT sh.tt, sh.rel, sh.kc, sh.k, sh.b0, sh.a0, sh.ids, sh.first_id, sh.kchg, sh.brk, sh.skip,
         (SELECT array_agg(x.name ORDER BY x.name COLLATE pg_catalog."C") FROM unnest(sh.names) AS x(name)),
         sh.why
    FROM shown sh
   ORDER BY sh.tt COLLATE pg_catalog."C", sh.pos NULLS LAST, sh.kind, sh.sid;
END
$$;

-- 잠그고 컬럼을 읽기. stet.undo_judge와 stet.undo_row가 테이블을 잠그고 그 컬럼을 읽는 한 곳이다. 둘의 비교는 모두 이
-- 함수가 잠근 뒤에 읽은 컬럼으로 한다. 부른 계정의 권한으로 돈다(SECURITY INVOKER). READ COMMITTED 트랜잭션에서만 돈다
-- (stet.undo_refuse와 같은 ST006). 돌려주는 것은 한 줄이다: table_name(지금의 이름, SQL에 그대로 쓸 수 있게
-- 인용된 것)과 column_names(지워지지 않은 컬럼, attnum 순).
-- 순서. READ COMMITTED에서는 문장마다 그 문장이 시작할 때의 카탈로그를 읽는다. 다른 세션이 두 문장 사이에 테이블의 이름을
-- 바꾸거나, 테이블을 지우고 다시 만들거나, 컬럼을 바꿀 수 있으므로:
-- 1. relid가 보통 테이블이 아니면 undefined_table이다.
-- 2. 이름은 relid의 regclass 출력이다(바꾼 이름도 따라간다). 1 뒤에 테이블이 지워졌으면 출력이 oid의 숫자이므로
--    undefined_table이다.
-- 3. 그 이름으로 테이블을 ACCESS SHARE로 잠근다(SELECT 권한이 없으면 여기서 42501이다). 잠금을 기다리는 동안 이름이 다른
--    테이블을 가리키게 됐으면 PostgreSQL은 그 테이블을 잠그므로, 잠근 뒤 이름이 아직 relid인지 본다. 다르면
--    object_not_in_prerequisite_state다. 잠근 뒤에는 트랜잭션이 끝날 때까지 그 테이블의 이름, 컬럼, 행 수준 보안을 바꾸는
--    ALTER TABLE이 기다린다.
-- 4. 행 수준 보안이 부른 역할에게 적용되면 42501이다. 숨긴 행을 "없는 행"으로 읽으면 insert가 될 수 있다.
--    stet.undo_selection은 고를 때의 카탈로그와, 지금의 역할이 아니라 세션이 될 수 있는 역할로 판단하기 때문이다.
-- 5. 파티션이면 feature_not_supported다. 파티션의 캡처는 받지 않는다(4장). 파티션 테이블을 가리키는 외래 키는 파티션마다
--    복제되어(conparentid가 있다) stet.undo_referenced가 보지 않고, 파티션 테이블에 단 트리거와 판단도 파티션을 따로 보지
--    않는다. 잠근 뒤에 보므로 잠금을 기다리는 동안 붙은(ATTACH PARTITION) 테이블도 거절한다.
-- 6. 잠근 뒤에 시작하는 문장으로 컬럼을 읽는다. 그래서 지금의 컬럼이고, 트랜잭션이 끝날 때까지 바뀌지 않는다.
CREATE OR REPLACE FUNCTION stet.undo_lock(relid oid) RETURNS TABLE (table_name text, column_names text[])
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
  tbl    text;
  parent text;
BEGIN
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'undo runs only in a READ COMMITTED transaction; this one is %',
      upper(current_setting('transaction_isolation'))
      USING ERRCODE = 'ST006', HINT = 'Call it inside BEGIN ISOLATION LEVEL READ COMMITTED; ... COMMIT.';
  END IF;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_class cl WHERE cl.oid = undo_lock.relid AND cl.relkind = 'r') THEN
    RAISE EXCEPTION 'no table has the oid %', undo_lock.relid USING ERRCODE = 'undefined_table';
  END IF;
  tbl := format('%s', undo_lock.relid::pg_catalog.regclass);
  IF tbl ~ '^[0-9]+$' THEN
    RAISE EXCEPTION 'no table has the oid %', undo_lock.relid USING ERRCODE = 'undefined_table';
  END IF;
  EXECUTE format('LOCK TABLE ONLY %s IN ACCESS SHARE MODE', tbl);
  IF pg_catalog.to_regclass(tbl) IS DISTINCT FROM undo_lock.relid THEN
    RAISE EXCEPTION 'the table with oid % was renamed or replaced while it was read', undo_lock.relid
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  IF pg_catalog.row_security_active(undo_lock.relid) THEN
    RAISE EXCEPTION 'row-level security on % applies to %', tbl, current_user USING ERRCODE = 'insufficient_privilege';
  END IF;
  SELECT format('%s', inh.inhparent::pg_catalog.regclass) INTO parent
    FROM pg_catalog.pg_class cl
    JOIN pg_catalog.pg_inherits inh ON inh.inhrelid = cl.oid
   WHERE cl.oid = undo_lock.relid AND cl.relispartition;
  IF parent IS NOT NULL THEN
    RAISE EXCEPTION '% is a partition of %; undo does not restore partitions', tbl, parent
      USING ERRCODE = 'feature_not_supported';
  END IF;
  table_name := tbl;
  SELECT array_agg(att.attname::text ORDER BY att.attnum) INTO column_names
    FROM pg_catalog.pg_attribute att
   WHERE att.attrelid = undo_lock.relid AND att.attnum > 0 AND NOT att.attisdropped;
  RETURN NEXT;
END
$$;

-- 지금 그 키의 행 (7장 "비교는 남길 때와 같은 방법으로 만든 글자끼리 한다"). 행이 없으면 NULL이다. lock이면 FOR UPDATE로
-- 읽고(stet.undo), 아니면 행을 잠그지 않는다(stet.undo_plan). 부른 계정의 권한으로 읽는다(SECURITY INVOKER). 먼저
-- stet.undo_lock으로 테이블을 잠그고 컬럼을 읽는다(그래서 READ COMMITTED에서만 돈다).
-- 그림은 stet.capture()와 같은 식, 같은 일곱 설정으로 만든다. 컬럼은 stet.undo_lock이 잠근 뒤에 읽은 것(attnum 순, 지워진
-- 컬럼은 뺀다)이고, 값은 CASE WHEN num_nulls(v) = 0 THEN format('%s', v) END다. 타입의 출력 함수(C, superuser만 만든다)이고
-- 캐스트가 아니다(4장). 그래서 같은 행이 캡처가 남긴 것과 같은 jsonb가 된다. 캡처의 함수와 코드를 나누지 않으므로
-- 둘이 같다는 것은 테스트(TestUndoRowMatchesCapture)가 지킨다. 한쪽을 바꾸면 다른 쪽도 바꾼다. 컬럼 이름은 그대로 SQL에
-- 들어가므로(r.<이름>) 컬럼이 아닌 이름은 r을 받는 함수로 읽힐 것이나, 잠근 뒤에 읽은 이름이고 잠근 뒤에는 바뀌지 않는다.
-- 행은 키로 찾는다. 키 컬럼의 값이 키의 글자와 OPERATOR(pg_catalog.=)로 같은 행이다. 키의 글자는 타입이 정해지지 않은
-- 리터럴로 넣으므로 PostgreSQL이 그 컬럼의 지금 타입(도메인이면 기본 타입)의 입력 함수로 읽는다. 타입 이름을 적지 않고, 글자에서
-- 그 타입으로 가는 캐스트도 도메인의 CHECK도 돌지 않는다. 키의 인덱스를 쓰고, 유일 제약과 복구 문장이 보는 것과 같은
-- 같음이다. 글자는 다르지만 타입으로는 같은 값의 행(키 1.50 자리에 1.5)도 그 키의 행이다. 그 행의 그림은 키 컬럼의 글자가
-- a와 달라 changed_since가 된다. 두 행 이상이 나오면 키 컬럼이 유일하지 않다는 에러다(cardinality_violation).
-- 행은 tableoid가 relid인 것만 읽고, 읽은 뒤에 이름과 행 수준 보안을 다시 본다. 스키마의 이름은 테이블을 잠가도 바뀔 수
-- 있으므로, 잠근 뒤와 행을 읽는 문장 사이에 이름이 다른 테이블을 가리켰다면 그 테이블의 행은 돌려주지도 잠그지도 않고
-- 에러다. 역할의 속성이나 소유자의 멤버십이 그 사이에 바뀌어 행 수준 보안이 적용됐다면 있는 행을 "없는 행"으로 읽었을 수
-- 있으므로 42501이다.
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
  hit       record;
  hits      integer := 0;
  found_row jsonb;
BEGIN
  SELECT lk.table_name, lk.column_names INTO tbl, names FROM stet.undo_lock(undo_row.relid) lk;
  IF (undo_row.key_column = ANY (names)) IS NOT TRUE THEN
    RAISE EXCEPTION 'table % has no column %', tbl, quote_ident(undo_row.key_column) USING ERRCODE = 'undefined_column';
  END IF;
  SELECT 'jsonb_object($1, ARRAY[' || string_agg(format(text_of, c.name), ', ' ORDER BY c.n) || '])' INTO img
    FROM unnest(names) WITH ORDINALITY AS c(name, n);

  FOR hit IN EXECUTE format(
      'SELECT %s AS img FROM ONLY %s r WHERE r.%I OPERATOR(pg_catalog.=) %L AND r.tableoid OPERATOR(pg_catalog.=) $2'
      ' LIMIT 2%s',
      img, tbl, undo_row.key_column, undo_row.key, CASE WHEN undo_row.lock THEN ' FOR UPDATE' ELSE '' END)
    USING names, undo_row.relid
  LOOP
    hits := hits + 1;
    IF hits > 1 THEN
      RAISE EXCEPTION 'more than one row of % has % as its %', tbl, quote_literal(undo_row.key), quote_ident(undo_row.key_column)
        USING ERRCODE = 'cardinality_violation';
    END IF;
    found_row := hit.img;
  END LOOP;
  IF pg_catalog.to_regclass(tbl) IS DISTINCT FROM undo_row.relid THEN
    RAISE EXCEPTION 'the table with oid % was renamed or replaced while it was read', undo_row.relid
      USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  IF pg_catalog.row_security_active(undo_row.relid) THEN
    RAISE EXCEPTION 'row-level security on % applies to %', tbl, current_user USING ERRCODE = 'insufficient_privilege';
  END IF;
  RETURN found_row;
END
$$;

-- 키마다 하나의 결정 (7장 "키마다 하나의 결정"). 스펙의 여섯 확인을 그 순서로 하고 처음 걸린 것을 reason으로 돌려준다:
-- table_gone(relid가 보통 테이블을 가리키지 않는다), columns_changed(b나 a의 컬럼 이름들이 지금 테이블의 컬럼들과 다르거나
-- 키 컬럼이 없다), key_changed, interleaved(둘은 stet.undo_selection이 남은 변경으로 정한다),
-- changed_since(지금 행이 a와 다르다. 없는 행은 NULL), no_net_change(b와 a가 같다). 하나도 걸리지 않으면 reason은 NULL이고
-- action이 b가 없으면 delete, a가 없으면 insert, 둘 다 있으면 update다. 걸리면 action은 NULL이다.
-- 부른 계정의 권한으로 돈다(SECURITY INVOKER). READ COMMITTED 트랜잭션에서만 돈다(stet.undo_refuse와 같은 ST006).
-- 순서:
-- 1. 거절. 2. table_gone: relid가 이 문장의 카탈로그에서 보통 테이블이 아니다. 잠글 테이블이 없으므로 잠그기 전이다.
-- 3. stet.undo_lock으로 테이블을 잠그고 컬럼을 읽는다. 그 뒤로 이 트랜잭션이 끝날 때까지 컬럼은 바뀌지 않는다. 2와 3 사이에
--    테이블이 지워지거나 이름이 다른 테이블로 넘어갔으면 stet.undo_lock의 에러가 그대로 나간다(계획에서는 skipped).
-- 4. columns_changed는 3이 읽은 컬럼과 비교한다. 잠그기 전에 읽은 컬럼과 비교하면, 잠금을 기다리는 동안 커밋된 컬럼의 변경을
--    놓쳐 지워진 행(a가 NULL)을 옛 컬럼으로 insert하라고 할 수 있다.
-- 5. key_changed, interleaved는 행을 읽지 않지만 4가 스펙의 순서에서 앞서므로 잠근 뒤에 본다.
-- 6. changed_since는 stet.undo_row(같은 잠금과 같은 컬럼, lock이면 FOR UPDATE)로 지금 행을 읽어 a와 비교한다. 7. no_net_change.
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
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'undo runs only in a READ COMMITTED transaction; this one is %',
      upper(current_setting('transaction_isolation'))
      USING ERRCODE = 'ST006', HINT = 'Call it inside BEGIN ISOLATION LEVEL READ COMMITTED; ... COMMIT.';
  END IF;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_class cl WHERE cl.oid = undo_judge.relid AND cl.relkind = 'r') THEN
    reason := 'table_gone';
  ELSE
    SELECT lk.column_names INTO cols FROM stet.undo_lock(undo_judge.relid) lk;
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

-- 계획 (7장 "stet.undo_plan"). 무엇이 되돌려지고 무엇이 건너뛰어질지 말하고, 아무것도 바꾸거나 적지 않으며 행을 잠그지
-- 않는다(stet.undo_judge에 lock = false). 판단하는 테이블은 stet.undo_lock이 ACCESS SHARE로 잠그고, 에러 없이 판단한 키의
-- 그 잠금은 부른 트랜잭션이 끝날 때까지 남는다(그동안 그 테이블의 ALTER TABLE이 기다린다. 쓰기는 기다리지 않는다). 부른 계정의 권한으로
-- 고객의 테이블을 읽는다(SECURITY INVOKER). 기록은 stet.undo_selection으로만 읽는다.
-- 줄은 stet.undo_selection의 순서(target_table, 그 키의 첫 변경 id)다. outcome은 would_restore, skipped, not_captured다.
-- 값을 남기지 않은 묶음은 key와 action이 NULL이고 reason이 그 skipped다. 부른 세션이 읽을 수 없는 테이블과 없어진 테이블은
-- key, action, change_ids가 NULL인 한 줄이고 outcome이 skipped, reason이 stet.undo_selection의 withheld다.
-- 경고는 되돌릴 줄(would_restore)에만 붙는다.
-- 에러를 내는 것은 다섯 거절(ST006)과, 이 함수와 stet.undo_refuse, stet.undo_selection의 EXECUTE가 없는 것(42501)뿐이다.
-- stet.undo_judge, stet.undo_lock, stet.undo_row, stet.undo_warning의 EXECUTE가 없으면 그 에러는 키마다의 블록 안에서
-- 나므로 키들이 skipped와 SQLSTATE 42501이 된다. 키마다의 판단은 블록 안에서 하고,
-- 거기서 난 에러(지금의 역할에 그 테이블의 SELECT가 없거나 행 수준 보안이 적용된다, 키가 유일하지 않다, 판단하는 동안
-- 테이블의 이름이 다른 테이블을 가리키게 됐다 등)는 그 키를
-- skipped로, reason을 'SQLSTATE <코드>: <메시지>'로 돌려준다. 블록은 하위 트랜잭션이지만 아무것도 쓰지 않으므로 트랜잭션
-- id를 받지 않는다. 시간 초과와 취소는 잡지 않는다.
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
    IF s.withheld IS NOT NULL THEN
      outcome := 'skipped';
      reason := s.withheld;
    ELSIF s.skipped IS NOT NULL THEN
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

-- 되돌리기: stet.undo (docs/spec-v0.2-undo.md 7장 "stet.undo"). 고른 변경을 검토자의 트랜잭션 안에서, 검토자의 권한으로
-- 되돌린다. 모델을 부르지 않는다(규칙 1). 캡처가 남긴 값과 키는 에이전트가 쓴 데이터다(규칙 5). 위의 도우미(고르기,
-- 잠그기, 지금 행, 판단, 경고)를 그대로 쓰고, 아래의 도우미를 더한다.
-- 복구 문장은 부른 세션의 설정으로 돈다. 그래서 stet.undo에는 SET 절이 없고, 고객의 트리거가 평소의 search_path,
-- TimeZone 따위로 돈다. 대신 stet.undo의 몸통은 이름을 푸는 일을 하지 않는다: 연산자를 쓰지 않고, 함수는 스키마까지
-- 적고(stet., pg_catalog.), 변수의 타입도 pg_catalog.까지 적는다. 부른 세션의 임시 스키마는 릴레이션과 타입의 이름을
-- pg_catalog보다 먼저 찾으므로, 예를 들어 text나 record라는 임시 테이블이 있으면 스키마 없는 text와 record는 그 행
-- 타입이 된다(테스트 서버에서 확인했다). 판단과 글자 만들기는 모두 일곱 설정을 고정한 도우미가 한다. 몸통이 하는 일은
-- 도우미를 부르고, 도우미가 돌려준 문장을 EXECUTE하고, 키마다의 블록에서 에러를 잡는 것뿐이다.
-- 복구 문장의 값(7장 "stet.undo"의 3). 값은 타입을 적지 않은 글자 리터럴(%L)이다. INSERT의 VALUES와 UPDATE의 SET에서 타입이
-- 정해지지 않은 리터럴은 PostgreSQL이 그 컬럼의 타입(도메인이면 기본 타입의 입력 뒤 도메인의 제약)의 입력 함수로 바로
-- 읽는다. 글자에서 그 타입으로 가는 캐스트(테이블 소유자가 만든 함수일 수 있다)는 돌지 않는다. %L::<타입>으로 적지 않는
-- 것은, format_type이 pg_catalog의 타입을 스키마 없이(text, date) 적어 그 이름이 부른 세션의 search_path와 임시 테이블로
-- 풀리기 때문이다. 또 'abcd'::varchar(3)은 말없이 자르지만 타입 없는 리터럴은 컬럼에 맞지 않으면 에러다.
-- 컬럼 이름은 지금의 카탈로그에서(판단이 남긴 값의 컬럼과 같음을 이미 확인했다) %I로, 테이블은 relid의 regclass 출력을
-- 고정한 search_path 아래에서 얻어(스키마까지) 적는다. 행은 키 컬럼과 tableoid로 찾고, 연산자는 OPERATOR(pg_catalog.=)다.

-- 고른 줄들 (stet.undo_selection의 결과, 그 순서대로). stet.undo가 한 번 읽어 jsonb 배열로 들고 다닌다. 다시 해 보는
-- 바퀴마다 다시 고르지 않는다: 그 사이에 커밋된 캡처나 보존 기간이 고른 것을 바꾸지 못하게 하려는 것이다. 줄 하나는
-- 고르기의 열세 컬럼을 그 이름으로 가진 객체다. 행이 없는 쪽(b, a)은 JSON null이다(남긴 그림은 언제나 객체다).
-- 부른 계정의 권한으로 돈다. 기록은 SECURITY DEFINER인 stet.undo_selection으로만 읽는다(그것이 거절도 한다).
CREATE OR REPLACE FUNCTION stet.undo_lines(agent_role text, since timestamptz, until timestamptz, tables text[])
RETURNS jsonb
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
  RETURN (
    SELECT coalesce(jsonb_agg(jsonb_build_object(
             'target_table', s.target_table, 'relid', s.relid, 'key_column', s.key_column, 'key', s.key,
             'b', s.b, 'a', s.a, 'change_ids', to_jsonb(s.change_ids), 'first_change', s.first_change,
             'key_changed', s.key_changed, 'interleaved', s.interleaved, 'skipped', s.skipped,
             'also_changed', to_jsonb(s.also_changed), 'withheld', s.withheld) ORDER BY s.ordinality), '[]'::jsonb)
      FROM stet.undo_selection(undo_lines.agent_role, undo_lines.since, undo_lines.until, undo_lines.tables)
           WITH ORDINALITY AS s);
END
$$;

-- 이번 바퀴에 해 볼 키들 (7장 "순서와 다시 해 보기"). lines는 stet.undo_lines의 것이고, tried, outcomes, codes, reasons는
-- 지금까지 해 본 차례대로의 키의 자리(lines에서 1부터), 결과, 실패의 SQLSTATE, 이유다. done은 지난 바퀴 전까지 해 본 수다.
-- - 아직 아무것도 해 보지 않았으면 첫 바퀴다. key가 있는 줄을 모두 돌려준다. 순서는 delete(b가 없다)를 그 키의 첫 변경이
--   나중인 것부터, 그다음 나머지를 결과의 순서대로, insert(a가 없다)를 그 키의 마지막 변경이 앞선 것부터다. 함께 만든
--   부모와 자식은 자식부터 지우고, 연쇄로 함께 지워진 부모와 자식은 부모부터 넣는다(부모의 트리거가 먼저 불려 부모의 변경
--   id가 작다). 다시 해 보기를 줄이려는 것이고, 맞는지는 다시 해 보기가 보장한다.
-- - 아니면 지난 바퀴(done 다음부터)를 본다. 그 바퀴에서 restored나 restored_differs가 하나도 없으면 아무것도 돌려주지 않고
--   끝난다. 있으면 그 바퀴에서 23503이나 23505로 failed가 된 키와 referenced로 skipped가 된 키를 그 바퀴의 순서대로
--   돌려준다. 바퀴마다 해 볼 키가 줄어들므로 끝난다.
-- 줄마다 판단에 넘길 값을 타입을 갖춰 돌려준다. 고정한 설정 아래의 계산뿐이고 아무것도 읽지 않는다.
CREATE OR REPLACE FUNCTION stet.undo_next(lines jsonb, tried integer[], outcomes text[], codes text[], reasons text[],
                                          done integer)
RETURNS TABLE (pos integer, relid oid, key_column text, key text, b jsonb, a jsonb, key_changed boolean,
               interleaved boolean, also_changed text[])
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
  todo integer[];
BEGIN
  IF coalesce(cardinality(undo_next.tried), 0) = 0 THEN
    SELECT array_agg(l.i ORDER BY l.grp, CASE WHEN l.grp = 1 THEN l.first_id END DESC,
                                         CASE WHEN l.grp = 3 THEN l.last_id END, l.i)
      INTO todo
      FROM (SELECT e.i::integer AS i,
                   CASE WHEN e.x->'b' = 'null' AND e.x->'a' <> 'null' THEN 1
                        WHEN e.x->'a' = 'null' AND e.x->'b' <> 'null' THEN 3
                        ELSE 2 END AS grp,
                   (e.x->'change_ids'->>0)::bigint AS first_id,
                   (e.x->'change_ids'->>-1)::bigint AS last_id
              FROM jsonb_array_elements(undo_next.lines) WITH ORDINALITY AS e(x, i)
             WHERE jsonb_typeof(e.x->'key') = 'string') l;
  ELSIF EXISTS (SELECT FROM unnest(undo_next.tried, undo_next.outcomes) WITH ORDINALITY AS t(p, o, j)
                 WHERE t.j > coalesce(undo_next.done, 0) AND t.o IN ('restored', 'restored_differs')) THEN
    SELECT array_agg(t.p ORDER BY t.j) INTO todo
      FROM unnest(undo_next.tried, undo_next.outcomes, undo_next.codes, undo_next.reasons) WITH ORDINALITY AS t(p, o, c, r, j)
     WHERE t.j > coalesce(undo_next.done, 0)
       AND ((t.o = 'failed' AND t.c IN ('23503', '23505'))
            OR (t.o = 'skipped' AND starts_with(t.r, 'referenced: ')));
  END IF;
  RETURN QUERY
  SELECT d.p, (d.x->>'relid')::oid, d.x->>'key_column', d.x->>'key', nullif(d.x->'b', 'null'), nullif(d.x->'a', 'null'),
         (d.x->>'key_changed')::boolean, (d.x->>'interleaved')::boolean,
         CASE WHEN jsonb_typeof(d.x->'also_changed') = 'array'
              THEN ARRAY(SELECT n.v FROM jsonb_array_elements_text(d.x->'also_changed') WITH ORDINALITY AS n(v, o)
                          ORDER BY n.o) END
    FROM (SELECT t.p, t.n, undo_next.lines -> (t.p - 1) AS x FROM unnest(todo) WITH ORDINALITY AS t(p, n)) d
   ORDER BY d.n;
END
$$;

-- 되돌리기는 연쇄를 일으키지 않는다 (7장 "referenced"). 복구 문장이 없애거나 바꾸는 값을,
-- 연쇄하는 외래 키로 가리키는 행이 있는 테이블들이다. 없으면 NULL이고, 있으면 'referenced: <테이블들>'이다(스키마까지
-- %I.%I, 바이트 순, ", "로 잇는다). stet.undo가 키의 행을 FOR UPDATE로 잠그고 지금 행이 a임을 확인한 뒤, 복구 문장 앞에 부른다.
-- 1. 거절(READ COMMITTED만). insert는 아무것도 없애거나 바꾸지 않으므로 NULL이다. update에서 SET할 컬럼이 없으면
--    (stet.undo_statement가 NULL) 아무것도 바뀌지 않으므로 NULL이다.
-- 2. 복구할 테이블을 ROW EXCLUSIVE로 잠근 뒤에 외래 키를 읽는다. 그 테이블을 가리키는 외래 키를 더하는 ALTER TABLE은 그
--    테이블에 SHARE ROW EXCLUSIVE를 잡아야 하는데, 그것은 ROW EXCLUSIVE를 기다린다. 그래서 외래 키를 읽은 뒤 복구 문장이 돌
--    때까지 보지 못한 연쇄 외래 키가 생기지 않는다. 잠금은 복구 문장 자체를 EXPLAIN으로 계획해서 잡는다(ANALYZE 없이).
--    PostgreSQL은 문장을 분석할 때 대상 테이블에 ROW EXCLUSIVE를 잡아 트랜잭션이 끝날 때까지 쥐고, 계획을 시작할 때 그 문장의
--    권한(컬럼 권한까지)을 확인하며, 실행하지 않으므로 트리거도 돌지 않는다. LOCK TABLE ... IN ROW EXCLUSIVE MODE는 테이블
--    단위의 INSERT, UPDATE, DELETE, TRUNCATE 권한을 요구해서, 고칠 컬럼의 UPDATE만 가진 검토자의 복구까지 막았다. 권한이
--    없으면 42501이고, stet.undo의 키마다의 블록 안이므로 그 키는 보통의 failed 줄이다(복구 문장도 같은 이유로 실패했을
--    것이다). 계획(stet.undo_plan)은 이 함수를 부르지 않으므로 ACCESS SHARE만 잡고 읽기만 한다.
--    EXPLAIN 뒤에 이 백엔드가 relid에 ROW EXCLUSIVE를 쥐었는지 pg_locks로 본다. 문장 안의 이름은 잠금을
--    기다리는 동안 다른 테이블을 가리키게 될 수 있고(ALTER SCHEMA ... RENAME은 테이블을 잠그지 않는다), 그러면 PostgreSQL은
--    이름을 다시 풀어 그 테이블을 잠그고 relid의 잠금은 놓는다. 쥐지 않았으면 object_not_in_prerequisite_state이고 그 키는
--    failed다. 앞의 키를 복구해 트랜잭션이 이미 relid에 그 잠금을 쥐고 있으면 그것으로 외래 키의 목록은 지켜진다.
-- 3. relid를 가리키는 외래 키 가운데 delete면 ON DELETE가, update면 ON UPDATE가 CASCADE, SET NULL, SET DEFAULT인 것을 본다.
--    파티션마다 복제된 외래 키(conparentid가 있다)는 보지 않는다. NO ACTION과 RESTRICT는 서버가 23503으로 막고, stet.undo가
--    다시 해 본다.
-- 4. update는 그 외래 키가 가리키는 컬럼 가운데 복구 문장이 실제로 바꾸는 것이 있을 때만 본다. stet.undo_statement가 SET하는
--    컬럼(생성 컬럼과 GENERATED ALWAYS AS IDENTITY 컬럼이 아니고 a와 b에서 값이 다른 것)과, a와 b에서 값이 다른 생성 컬럼(SET한
--    컬럼에서 다시 계산된다)이다. IDENTITY ALWAYS 컬럼은 SET하지 않으므로 바뀌지 않는다. 고객의 BEFORE 트리거가 바꾸는 컬럼은
--    알 수 없다.
-- 5. 따라가지 않는 외래 키. 그 외래 키를 불러일으킬 복구는 feature_not_supported(0A000)이고 그 키는 failed다. 메시지가 제약과
--    자식 테이블을 말한다.
--    - 컬럼 짝의 타입이 도메인을 벗긴 기본 타입에서 다르다(char와 text, timestamptz와 timestamp, integer와 bigint).
--      PostgreSQL의 연쇄는 그런 비교에 캐스트를 더하고, 그 결과는 세션의 설정(TimeZone 등)에 따를 수 있다. typmod는 보지
--      않는다(varchar(10)과 varchar(20)).
--    - 비교에 드는 콜레이션 넷 가운데 하나라도 비결정적이다: 부모 컬럼의 타입의 것(연쇄의 매개변수가 이것을
--      갖는다), 부모 컬럼의 것(비결정적이고 자식과 다르면 연쇄가 이것으로 비교한다), 자식 컬럼의 것, 자식 컬럼의 타입의 것
--      (자식을 받아들이는 확인의 매개변수가 이것을 갖고, 새 자식이 어느 부모 행을 잠그는지를 정한다). 비결정적인 비교에서는
--      연쇄가 닿는 자식이 이 행이 아닌 다른 부모 행을 가리킬 수 있어, 그런 자식은 조사와 복구 사이에 생길 수 있다. 모두
--      같은 비결정적 콜레이션인 경우도 거절한다.
--    - 연쇄가 쓸 콜레이션을 정할 수 없다: 부모 컬럼의 타입의 것과 자식 컬럼의 것이 둘 다 기본이 아니고 서로 다르다(연쇄
--      자체가 콜레이션 충돌로 실패한다).
--    - 제약의 같음 연산자가 pg_catalog 밖에 있고 받는 타입 가운데 의사 타입(anyarray, anyelement 등)이 있다. 6의 비교는
--      피연산자를 연산자가 받는 타입으로 캐스트해 그 연산자 하나로만 풀리게 하는데, 의사 타입으로의 캐스트는 피연산자의
--      타입을 바꾸지 않는다. 그러면 OPERATOR(스키마.이름)은 컬럼의 실제 타입으로 풀리고, 그 스키마에 그 타입끼리의 같은
--      이름의 연산자가 있으면 제약의 것이 아니라 그것이 골라져 검토자의 권한으로 돈다(테스트 서버에서 확인했다). 그 스키마에
--      만들 수 있는 계정이면 누구나 더할 수 있다. pg_catalog에는 superuser만 연산자를 더한다.
-- 6. 자식이 있는지는 PostgreSQL의 연쇄가 쓰는 비교 그대로 본다(ri_triggers.c의 ri_GenerateQual, ruleutils.c의
--    generate_operator_clause와 같은 글자). 짝마다 p.<부모 컬럼>[::<연산자의 왼쪽 타입>] OPERATOR(<스키마>.<conpfeqop의 이름>)
--    x.<자식 컬럼>[::<연산자의 오른쪽 타입>] COLLATE <연쇄의 콜레이션>이다. 캐스트는 컬럼의 타입(도메인이면 도메인 자체)이
--    연산자가 받는 타입과 다를 때만 붙이고, 타입은 pg_type의 스키마와 이름을 %I.%I로 적는다. 연산자가 받는 타입이 보통
--    타입이면 두 피연산자가 그 타입 그대로이므로 OPERATOR(스키마.이름)은 conpfeqop 하나로만 풀린다(같은 스키마, 이름, 입력
--    타입의 연산자는 둘일 수 없다). 그래서 에이전트가 그 스키마에 컬럼의 타입끼리의 같은 이름의 연산자를 더해도 고르지
--    않는다. 받는 타입이 의사 타입이면 그렇지 않다. 캐스트가 피연산자의 타입을 바꾸지 않아 컬럼의 타입끼리의 연산자가
--    먼저 골라진다. pg_catalog의 연산자(배열의 같음 등)에서는 superuser만 그런 연산자를 더할 수 있으므로 그대로 따라가고,
--    pg_catalog 밖의 것은 5에서 거절한다. 콜레이션은 연쇄가
--    쓰는 것, 곧 부모 컬럼의 타입의 콜레이션과 자식 컬럼의 콜레이션을 PostgreSQL이 두 암묵 콜레이션을 합치는 대로 합친
--    것이다(같으면 그것, 한쪽이 기본이면 다른 쪽). 스키마까지 적고 명시한다. 콜레이션이 없는 타입에는 붙이지 않는다.
--    부모의 값은 a의 글자를 다시 읽지 않고 그 키의 행(이미 잠근 행, 연쇄가 OLD로 볼 행)에서 조인으로 가져온다:
--    SELECT EXISTS (SELECT FROM ONLY <부모> p, [ONLY] <자식> x WHERE <키의 행> AND <짝마다의 비교> [AND x.ctid <> p.ctid]).
--    그 행을 찾지 못하면 object_not_in_prerequisite_state다. 값 가운데 NULL이 있으면 비교가 참이 아니어서 아무 행도 맞지
--    않는다(PostgreSQL도 NULL이 든 키에서는 연쇄하지 않는다. MATCH SIMPLE에서 NULL이 든 자식은 아무것도 가리키지 않는다).
--    자기를 가리키는 외래 키에서는 그 행 자신을 ctid로 뺀다. 자식은 파티션 테이블이면 파티션까지, 아니면 ONLY로 읽는다.
--    자식 테이블은 이름으로 적는다(conrelid의 regclass 출력). 이름은 조사가 자식의 잠금을 기다리는 동안 다른 테이블을
--    가리키게 될 수 있다(ALTER SCHEMA ... RENAME은 테이블을 잠그지 않는다). 그러면 PostgreSQL은 이름을 다시 풀어 그 테이블을
--    읽고, 조사는 자식이 없다고 답하며, 복구는 진짜 자식으로 연쇄한다. 그래서 조사 뒤에 두 가지를 본다. 조사에 적은 바로 그
--    이름이 아직 conrelid를 가리키고(to_regclass), 이 백엔드가 conrelid에 ACCESS SHARE를 쥐고 있다(pg_locks, 부모의 2와
--    같다). 하나라도 아니면 object_not_in_prerequisite_state이고 그 키는 failed다. 잠금만 보면 모자란다. 같은 되돌리기의
--    앞의 키가 조사하며 잡은 잠금이 남아 있을 수 있다. 이름만 봐도 모자란다. 이름이 조사 뒤에 돌아왔을 수 있다. 남는 것은
--    조사가 이름을 푼 때와 이 확인 사이에 이름이 다른 테이블로 갔다가 돌아오고(두 번 바뀌고), 앞의 키가 잡은 잠금이 남아
--    있는 경우다(스펙 11장).
-- 7. 자식 테이블은 부른 계정의 권한으로 읽는다(SECURITY INVOKER). 읽을 권한이 없으면 42501이다. 행 수준 보안이 부른 역할에게
--    적용되면 숨은 자식을 놓칠 수 있으므로(연쇄는 행 수준 보안을 지나간다) 42501이다. 이것을 조사 앞뒤로 본다. 조사가 자식
--    테이블의 잠금을 기다리는 동안 행 수준 보안이 켜질 수 있다.
-- 잠근 뒤에 보므로 그 사이에 새 자식이 생기지 않는다. 외래 키의 등호는 부모의 유일 인덱스의 것이고 비교가 결정적이므로,
-- 이 행의 값과 같은 부모 행은 이 행뿐이고 그런 자식은 이 행을 가리킨다. 그런 자식을 넣거나 이 행을 가리키게 고치는 쪽은 이
-- 행에 FOR KEY SHARE를 잡아야 하는데, 그것은 FOR UPDATE를 기다린다. 이미 그 잠금을 쥔 쪽이 있었다면 FOR UPDATE가 그쪽을
-- 기다렸다.
-- 이 단계의 앞선 판을 실행한 데이터베이스에는 key_column과 key 없이 네 인자의 이 함수가 남아 있다. 인자가 바뀌면 CREATE
-- OR REPLACE는 새 함수를 더하므로 그 모양을 지운다. 남겨 두면 PUBLIC이 부를 수 있는 옛 함수가 남고, stet undo-sql
-- -grant가 그것까지 검토자에게 준다.
DROP FUNCTION IF EXISTS stet.undo_referenced(oid, text, jsonb, jsonb);
CREATE OR REPLACE FUNCTION stet.undo_referenced(relid oid, key_column text, key text, action text, b jsonb, a jsonb)
RETURNS text
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
  restore   text;
  tbl       text;
  keyed     text;
  fk        record;
  pair      record;
  pbase     oid;
  cbase     oid;
  ttype     "char";
  tbase     oid;
  dflt      oid;
  eff       oid;
  coll      text;
  nd        text;
  cond      text;
  kid       text;
  found_row boolean;
  hit       boolean;
  children  text[] := '{}';
BEGIN
  IF current_setting('transaction_isolation') <> 'read committed' THEN
    RAISE EXCEPTION 'undo runs only in a READ COMMITTED transaction; this one is %',
      upper(current_setting('transaction_isolation'))
      USING ERRCODE = 'ST006', HINT = 'Call it inside BEGIN ISOLATION LEVEL READ COMMITTED; ... COMMIT.';
  END IF;
  IF undo_referenced.action IS NULL OR undo_referenced.action NOT IN ('delete', 'update') THEN
    RETURN NULL;
  END IF;
  restore := stet.undo_statement(undo_referenced.relid, undo_referenced.key_column, undo_referenced.key,
                                 undo_referenced.action, undo_referenced.b, undo_referenced.a);
  IF restore IS NULL THEN
    RETURN NULL;
  END IF;
  EXECUTE 'EXPLAIN ' || restore;
  IF NOT EXISTS (SELECT FROM pg_catalog.pg_locks l
                  WHERE l.locktype = 'relation'
                    AND l.database = (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = current_database())
                    AND l.relation = undo_referenced.relid AND l.pid = pg_backend_pid()
                    AND l.mode = 'RowExclusiveLock' AND l.granted) THEN
    RAISE EXCEPTION 'the restore statement for the table with oid % locked another table: its name was given to another table while it waited for the lock',
      undo_referenced.relid USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  SELECT lk.table_name INTO tbl FROM stet.undo_lock(undo_referenced.relid) lk;
  dflt := 'pg_catalog."default"'::regcollation;
  keyed := format('p.%I OPERATOR(pg_catalog.=) %L AND p.tableoid OPERATOR(pg_catalog.=) %L',
                  undo_referenced.key_column, undo_referenced.key, undo_referenced.relid);
  FOR fk IN
    SELECT con.conname, con.conrelid, con.confkey, con.conkey, con.conpfeqop, cl.relkind,
           format('%I.%I', ns.nspname, cl.relname) AS name
      FROM pg_catalog.pg_constraint con
      JOIN pg_catalog.pg_class cl ON cl.oid = con.conrelid
      JOIN pg_catalog.pg_namespace ns ON ns.oid = cl.relnamespace
     WHERE con.contype = 'f' AND con.confrelid = undo_referenced.relid AND con.conparentid = 0
       AND CASE undo_referenced.action WHEN 'delete' THEN con.confdeltype ELSE con.confupdtype END IN ('c', 'n', 'd')
     ORDER BY format('%I.%I', ns.nspname, cl.relname) COLLATE pg_catalog."C", con.oid
  LOOP
    IF undo_referenced.action = 'update'
       AND NOT EXISTS (SELECT FROM unnest(fk.confkey) AS k(pk)
                         JOIN pg_catalog.pg_attribute pa ON pa.attrelid = undo_referenced.relid AND pa.attnum = k.pk
                        WHERE (pa.attgenerated <> '' OR pa.attidentity <> 'a')
                          AND (undo_referenced.a -> pa.attname::text) IS DISTINCT FROM (undo_referenced.b -> pa.attname::text)) THEN
      CONTINUE;
    END IF;
    cond := NULL;
    FOR pair IN
      SELECT pa.attname AS pcol, ca.attname AS ccol, pa.atttypid AS ptype, ca.atttypid AS ctype,
             format('OPERATOR(%I.%s)', opn.nspname, op.oprname) AS op,
             CASE WHEN pa.atttypid <> op.oprleft THEN format('::%I.%I', ln.nspname, lt.typname) ELSE '' END AS lcast,
             CASE WHEN ca.atttypid <> op.oprright THEN format('::%I.%I', rn.nspname, rt.typname) ELSE '' END AS rcast,
             pt.typcollation AS pcoll, pa.attcollation AS pacoll, ca.attcollation AS fcoll, ct.typcollation AS qcoll,
             opn.nspname <> 'pg_catalog' AND (lt.typtype = 'p' OR rt.typtype = 'p') AS loose,
             format('%I.%s(%s, %s)', opn.nspname, op.oprname, format_type(op.oprleft, NULL), format_type(op.oprright, NULL)) AS opname
        FROM unnest(fk.confkey, fk.conkey, fk.conpfeqop) WITH ORDINALITY AS k(pk, ck, eqop, n)
        JOIN pg_catalog.pg_attribute pa ON pa.attrelid = undo_referenced.relid AND pa.attnum = k.pk
        JOIN pg_catalog.pg_attribute ca ON ca.attrelid = fk.conrelid AND ca.attnum = k.ck
        JOIN pg_catalog.pg_type pt ON pt.oid = pa.atttypid
        JOIN pg_catalog.pg_type ct ON ct.oid = ca.atttypid
        JOIN pg_catalog.pg_operator op ON op.oid = k.eqop
        JOIN pg_catalog.pg_namespace opn ON opn.oid = op.oprnamespace
        JOIN pg_catalog.pg_type lt ON lt.oid = op.oprleft
        JOIN pg_catalog.pg_namespace ln ON ln.oid = lt.typnamespace
        JOIN pg_catalog.pg_type rt ON rt.oid = op.oprright
        JOIN pg_catalog.pg_namespace rn ON rn.oid = rt.typnamespace
       ORDER BY k.n
    LOOP
      IF pair.loose THEN
        RAISE EXCEPTION 'the cascading foreign key % of % compares with the operator %, which is not in pg_catalog and takes a pseudo-type; undo does not follow such a cascade',
          quote_ident(fk.conname), fk.name, pair.opname USING ERRCODE = 'feature_not_supported';
      END IF;
      pbase := pair.ptype;
      LOOP
        SELECT ty.typtype, ty.typbasetype INTO ttype, tbase FROM pg_catalog.pg_type ty WHERE ty.oid = pbase;
        EXIT WHEN ttype IS DISTINCT FROM 'd';
        pbase := tbase;
      END LOOP;
      cbase := pair.ctype;
      LOOP
        SELECT ty.typtype, ty.typbasetype INTO ttype, tbase FROM pg_catalog.pg_type ty WHERE ty.oid = cbase;
        EXIT WHEN ttype IS DISTINCT FROM 'd';
        cbase := tbase;
      END LOOP;
      IF pbase IS DISTINCT FROM cbase THEN
        RAISE EXCEPTION 'the cascading foreign key % of % compares % with %; undo does not follow a cascade between columns of different types',
          quote_ident(fk.conname), fk.name, format_type(pbase, NULL), format_type(cbase, NULL)
          USING ERRCODE = 'feature_not_supported';
      END IF;
      SELECT format('%I.%I', cn.nspname, c.collname) INTO nd
        FROM unnest(ARRAY[pair.pcoll, pair.pacoll, pair.fcoll, pair.qcoll]) WITH ORDINALITY AS u(coll, n)
        JOIN pg_catalog.pg_collation c ON c.oid = u.coll
        JOIN pg_catalog.pg_namespace cn ON cn.oid = c.collnamespace
       WHERE NOT c.collisdeterministic
       ORDER BY u.n
       LIMIT 1;
      IF nd IS NOT NULL THEN
        RAISE EXCEPTION 'the cascading foreign key % of % compares under the nondeterministic collation %; undo does not follow a cascade under a nondeterministic collation',
          quote_ident(fk.conname), fk.name, nd USING ERRCODE = 'feature_not_supported';
      END IF;
      IF pair.pcoll = 0 OR pair.fcoll = 0 THEN
        coll := '';
      ELSE
        eff := CASE WHEN pair.pcoll = pair.fcoll THEN pair.pcoll
                    WHEN pair.pcoll = dflt THEN pair.fcoll
                    WHEN pair.fcoll = dflt THEN pair.pcoll END;
        IF eff IS NULL THEN
          RAISE EXCEPTION 'the cascading foreign key % of % compares under the collations % and %, between which PostgreSQL cannot choose; undo does not follow such a cascade',
            quote_ident(fk.conname), fk.name,
            (SELECT format('%I.%I', cn.nspname, c.collname) FROM pg_catalog.pg_collation c
               JOIN pg_catalog.pg_namespace cn ON cn.oid = c.collnamespace WHERE c.oid = pair.pcoll),
            (SELECT format('%I.%I', cn.nspname, c.collname) FROM pg_catalog.pg_collation c
               JOIN pg_catalog.pg_namespace cn ON cn.oid = c.collnamespace WHERE c.oid = pair.fcoll)
            USING ERRCODE = 'feature_not_supported';
        END IF;
        SELECT format(' COLLATE %I.%I', cn.nspname, c.collname) INTO coll
          FROM pg_catalog.pg_collation c JOIN pg_catalog.pg_namespace cn ON cn.oid = c.collnamespace
         WHERE c.oid = eff;
      END IF;
      cond := concat_ws(' AND ', cond,
                        format('p.%I%s %s x.%I%s%s', pair.pcol, pair.lcast, pair.op, pair.ccol, pair.rcast, coll));
    END LOOP;
    IF fk.conrelid = undo_referenced.relid THEN
      cond := cond || ' AND x.ctid OPERATOR(pg_catalog.<>) p.ctid';
    END IF;
    IF row_security_active(fk.conrelid) THEN
      RAISE EXCEPTION 'row-level security on % applies to %', fk.name, current_user USING ERRCODE = 'insufficient_privilege';
    END IF;
    kid := format('%s', fk.conrelid::pg_catalog.regclass);
    EXECUTE format('SELECT EXISTS (SELECT FROM ONLY %1$s p WHERE %2$s), '
                   'EXISTS (SELECT FROM ONLY %1$s p, %3$s%4$s x WHERE %2$s AND %5$s)',
                   tbl, keyed, CASE WHEN fk.relkind = 'p' THEN '' ELSE 'ONLY ' END, kid, cond)
      INTO found_row, hit;
    IF pg_catalog.to_regclass(kid) IS DISTINCT FROM fk.conrelid THEN
      RAISE EXCEPTION 'the table with oid %, which references %, is no longer named %: its name was given to another table while it was read',
        fk.conrelid, tbl, kid USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF NOT EXISTS (SELECT FROM pg_catalog.pg_locks l
                    WHERE l.locktype = 'relation'
                      AND l.database = (SELECT d.oid FROM pg_catalog.pg_database d WHERE d.datname = current_database())
                      AND l.relation = fk.conrelid AND l.pid = pg_backend_pid()
                      AND l.mode = 'AccessShareLock' AND l.granted) THEN
      RAISE EXCEPTION 'reading %, which references %, locked another table than the one with oid %: its name was given to another table while it waited for the lock',
        kid, tbl, fk.conrelid USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF row_security_active(fk.conrelid) THEN
      RAISE EXCEPTION 'row-level security on % applies to %', fk.name, current_user USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF NOT found_row THEN
      RAISE EXCEPTION 'no row of % has % as its %', tbl, quote_literal(undo_referenced.key), quote_ident(undo_referenced.key_column)
        USING ERRCODE = 'object_not_in_prerequisite_state';
    END IF;
    IF hit AND NOT fk.name = ANY (children) THEN
      children := children || fk.name;
    END IF;
  END LOOP;
  IF cardinality(children) = 0 THEN
    RETURN NULL;
  END IF;
  RETURN 'referenced: ' || array_to_string(children, ', ');
END
$$;

-- 복구 문장의 글자 (7장 "stet.undo"의 3). action이 delete면 그 키의 행을 지우고, update면 a와 b가 다른 컬럼만 b의 값으로
-- 바꾸고, insert면 b의 행을 넣는다. 값을 적는 법과 이름을 적는 법은 이 절의 머리 주석에 있다.
-- - 생성 컬럼(GENERATED ALWAYS AS ... STORED)에는 값을 주지 않는다. GENERATED ALWAYS AS IDENTITY 컬럼은 UPDATE로는 값을 줄
--   수 없으므로 insert에서만 주고, 그래서 insert는 OVERRIDING SYSTEM VALUE를 쓴다(테이블 소유자가 아니어도 된다).
--   update에서 바꿀 컬럼이 그런 것뿐이면 문장이 없다(NULL). 그때 그 행은 b가 될 수 없어 restored_differs가 된다.
-- - delete와 update는 ONLY로, 키 컬럼과 tableoid로 그 행만 고른다. 세 문장 모두 RETURNING r.tableoid로 바꾼 행이 어느
--   테이블의 것인지 돌려준다. stet.undo가 그 문장, 바꾼 행 수와 함께 stet.undo_outcome에 넘긴다.
-- 먼저 stet.undo_lock으로 테이블을 잠그고 이름을 확인한다(이미 판단이 잠갔으므로 대개 아무것도 기다리지 않는다). 컬럼은
-- 그 뒤에 지금의 카탈로그에서 읽는다. 글자를 만들 뿐 아무것도 실행하지 않는다. 부른 계정의 권한으로 돈다.
CREATE OR REPLACE FUNCTION stet.undo_statement(relid oid, key_column text, key text, action text, b jsonb, a jsonb)
RETURNS text
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
  tbl    text;
  target text;
  sets   text;
  cols   text;
  vals   text;
BEGIN
  SELECT lk.table_name INTO tbl FROM stet.undo_lock(undo_statement.relid) lk;
  target := format('r.%I OPERATOR(pg_catalog.=) %L AND r.tableoid OPERATOR(pg_catalog.=) %L',
                   undo_statement.key_column, undo_statement.key, undo_statement.relid);
  IF undo_statement.action = 'delete' THEN
    RETURN format('DELETE FROM ONLY %s r WHERE %s RETURNING r.tableoid', tbl, target);
  ELSIF undo_statement.action = 'update' THEN
    SELECT string_agg(format('%I = %L', att.attname, undo_statement.b ->> att.attname::text), ', ' ORDER BY att.attnum)
      INTO sets
      FROM pg_catalog.pg_attribute att
     WHERE att.attrelid = undo_statement.relid AND att.attnum > 0 AND NOT att.attisdropped
       AND att.attgenerated = '' AND att.attidentity <> 'a'
       AND (undo_statement.b -> att.attname::text) IS DISTINCT FROM (undo_statement.a -> att.attname::text);
    IF sets IS NULL THEN
      RETURN NULL;
    END IF;
    RETURN format('UPDATE ONLY %s r SET %s WHERE %s RETURNING r.tableoid', tbl, sets, target);
  ELSIF undo_statement.action = 'insert' THEN
    SELECT string_agg(format('%I', att.attname), ', ' ORDER BY att.attnum),
           string_agg(format('%L', undo_statement.b ->> att.attname::text), ', ' ORDER BY att.attnum)
      INTO cols, vals
      FROM pg_catalog.pg_attribute att
     WHERE att.attrelid = undo_statement.relid AND att.attnum > 0 AND NOT att.attisdropped AND att.attgenerated = '';
    RETURN format('INSERT INTO %s AS r (%s) OVERRIDING SYSTEM VALUE VALUES (%s) RETURNING r.tableoid', tbl, cols, vals);
  END IF;
  RAISE EXCEPTION 'action must be delete, insert or update, not %', coalesce(quote_literal(undo_statement.action), 'NULL')
    USING ERRCODE = 'invalid_parameter_value';
END
$$;

-- 복구한 뒤의 결과 (7장 "stet.undo"의 4). 그 키의 지금 행을 stet.undo_row로 다시 읽어(같은 그림, 같은 설정) b와 같으면
-- restored, 다르면 restored_differs다(고객의 트리거나 기본값이 값을 바꿨다, 값을 줄 수 없는 컬럼이 b와 달랐다, 트리거가
-- 쓰기를 막아 delete인데 행이 남았다). statement는 stet.undo가 실행한 복구 문장이고(없었으면 NULL), changed와 written은 그
-- 문장이 바꾼 행 수와 그 행의 tableoid다. 다음이면 에러를 내어 stet.undo가 그 키의 블록을 되돌리게 한다(그 키는 failed다):
-- - 두 행 이상을 바꿨다(cardinality_violation). 키 컬럼이 유일하지 않은데 판단과 문장 사이에 같은 키의 행이 생겼다.
-- - 한 행을 다른 테이블에 썼다(object_not_in_prerequisite_state). insert의 이름이 그 사이에 다른 테이블을 가리켰다.
-- - statement가 지금 stet.undo_statement가 만들 문장과 다르다(object_not_in_prerequisite_state). 테이블을 잠가도 스키마의
--   이름은 바뀔 수 있다(ALTER SCHEMA ... RENAME은 테이블을 잠그지 않는다. 테스트 서버에서 확인했다). 문장을 만든 뒤 실행하기
--   전에 그 이름이 다른 테이블을 가리키게 됐으면, update와 delete는 tableoid로 relid의 행만 고르므로 그 테이블의 행을 바꾸지
--   않고 relid의 행도 바꾸지 못한다(바꾼 행이 0이다). 이것을 재지 않으면 relid의 행은 a 그대로라 restored_differs가 되고,
--   아무것도 쓰지 않았는데 되돌렸다고 기록된다. 문장을 지금의 이름으로 다시 만들어 같은지 본다. 컬럼은 잠근 뒤라 바뀌지
--   않으므로, 다르면 이름이 바뀐 것이다. 이름이 바뀌었다가 그 사이에 돌아온 경우는 가려내지 못한다(그때도 다른 테이블의 행은
--   바꾸지 않는다).
-- 이 단계의 앞선 판을 실행한 데이터베이스에는 statement 없이 여섯 인자의 stet.undo_outcome이 남아 있다. 인자가 바뀌면
-- CREATE OR REPLACE는 새 함수를 더하므로 그 모양을 지운다. 남겨 두면 stet undo-sql -grant가 그것까지 검토자에게 준다.
DROP FUNCTION IF EXISTS stet.undo_outcome(oid, text, text, jsonb, bigint, oid);
CREATE OR REPLACE FUNCTION stet.undo_outcome(relid oid, key_column text, key text, action text, b jsonb, a jsonb,
                                             statement text, changed bigint, written oid)
RETURNS text
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
  IF undo_outcome.changed > 1 THEN
    RAISE EXCEPTION 'restoring % of % changed % rows, not one', quote_literal(undo_outcome.key), undo_outcome.relid::regclass,
      undo_outcome.changed USING ERRCODE = 'cardinality_violation';
  END IF;
  IF undo_outcome.changed = 1 AND undo_outcome.written IS DISTINCT FROM undo_outcome.relid THEN
    RAISE EXCEPTION 'restoring % of the table with oid % wrote to the table with oid %', quote_literal(undo_outcome.key),
      undo_outcome.relid, undo_outcome.written USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  IF undo_outcome.statement IS DISTINCT FROM stet.undo_statement(undo_outcome.relid, undo_outcome.key_column,
                                                                  undo_outcome.key, undo_outcome.action, undo_outcome.b,
                                                                  undo_outcome.a) THEN
    RAISE EXCEPTION 'the name of the table with oid % changed between making and running its restore statement',
      undo_outcome.relid USING ERRCODE = 'object_not_in_prerequisite_state';
  END IF;
  IF stet.undo_row(undo_outcome.relid, undo_outcome.key_column, undo_outcome.key, false) IS NOT DISTINCT FROM undo_outcome.b THEN
    RETURN 'restored';
  END IF;
  RETURN 'restored_differs';
END
$$;

-- 끝에서 다시 보기 (7장 "stet.undo"의 4). 결과의 줄은 stet.undo가 돌려줄 때 참이어야 한다. 뒤의 키를 복구할 때 고객의
-- 트리거가 앞에서 restored가 된 행을 바꾸거나, 지운 행을 다시 넣을 수 있다. 그래서 모든 바퀴가 끝난 뒤 키마다 마지막
-- 결과가 restored인 것을 stet.undo_row로 다시 읽어, 지금 행이 b가 아니면(delete면 행이 다시 있으면) restored_differs로
-- 바꾼다. 다시 읽다가 에러가 나면(예를 들어 그 사이에 역할의 속성이 바뀌어 행 수준 보안이 적용된다) 확인하지 못했으므로
-- restored_differs로 바꾸고 reason에 그 에러를 적는다. 키마다 블록 하나이고 아무것도 쓰지 않는다. tried, outcomes,
-- reasons는 stet.undo가 해 본 차례대로의 것이고, 같은 길이의 두 배열을 돌려준다.
CREATE OR REPLACE FUNCTION stet.undo_recheck(lines jsonb, tried integer[], outcomes text[], reasons text[])
RETURNS TABLE (checked text[], why text[])
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
  last    record;
  x       jsonb;
  code    text;
  message text;
BEGIN
  checked := undo_recheck.outcomes;
  why := undo_recheck.reasons;
  FOR last IN SELECT DISTINCT ON (t.p) t.p, t.j, t.o
                FROM unnest(undo_recheck.tried, undo_recheck.outcomes) WITH ORDINALITY AS t(p, o, j)
               ORDER BY t.p, t.j DESC LOOP
    CONTINUE WHEN last.o IS DISTINCT FROM 'restored';
    x := undo_recheck.lines -> (last.p - 1);
    BEGIN
      IF stet.undo_row((x->>'relid')::oid, x->>'key_column', x->>'key', false) IS DISTINCT FROM nullif(x->'b', 'null') THEN
        checked[last.j] := 'restored_differs';
      END IF;
    EXCEPTION WHEN OTHERS THEN
      GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, message = MESSAGE_TEXT;
      checked[last.j] := 'restored_differs';
      why[last.j] := stet.undo_failure(code, message);
    END;
  END LOOP;
  RETURN NEXT;
END
$$;

-- 실패의 이유 (7장 "stet.undo"의 5): 'SQLSTATE <코드>: <메시지>'.
CREATE OR REPLACE FUNCTION stet.undo_failure(sqlstate text, message text) RETURNS text
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
  RETURN 'SQLSTATE ' || undo_failure.sqlstate || ': ' || undo_failure.message;
END
$$;

-- 결과의 줄들. lines(stet.undo_lines)의 순서, 곧 계획의 순서로 한 줄씩이다. key가 있는 줄은 그 키를 마지막으로 해 본 결과다
-- (tried, actions, outcomes, reasons, warnings는 해 본 차례대로다). 보이지 않거나 없어진 테이블의 줄은 skipped와 그
-- withheld, 값을 남기지 않은 묶음의 줄은 not_captured와 그 skipped다. 줄마다 stet.undo의 일곱 컬럼을 그 이름으로 가진
-- 객체이고, 결과는 그 배열이다. stet.undo_record에 그대로 넘기고, stet.undo_unpack으로 결과의 행이 된다.
CREATE OR REPLACE FUNCTION stet.undo_result(lines jsonb, tried integer[], actions text[], outcomes text[], reasons text[],
                                            warnings text[])
RETURNS jsonb
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
  RETURN (
    SELECT coalesce(jsonb_agg(
             CASE WHEN jsonb_typeof(e.x->'withheld') = 'string' THEN
                    jsonb_build_object('target_table', e.x->'target_table', 'key', NULL, 'action', NULL,
                                       'outcome', 'skipped', 'reason', e.x->'withheld', 'warning', NULL, 'change_ids', NULL)
                  WHEN jsonb_typeof(e.x->'skipped') = 'string' THEN
                    jsonb_build_object('target_table', e.x->'target_table', 'key', NULL, 'action', NULL,
                                       'outcome', 'not_captured', 'reason', e.x->'skipped', 'warning', NULL,
                                       'change_ids', NULL)
                  ELSE
                    jsonb_build_object('target_table', e.x->'target_table', 'key', e.x->'key', 'action', l.act,
                                       'outcome', l.outc, 'reason', l.why, 'warning', l.wrn,
                                       'change_ids', e.x->'change_ids')
             END ORDER BY e.i), '[]'::jsonb)
      FROM jsonb_array_elements(undo_result.lines) WITH ORDINALITY AS e(x, i)
      LEFT JOIN (SELECT DISTINCT ON (t.p) t.p, t.act, t.outc, t.why, t.wrn
                   FROM unnest(undo_result.tried, undo_result.actions, undo_result.outcomes, undo_result.reasons,
                               undo_result.warnings) WITH ORDINALITY AS t(p, act, outc, why, wrn, j)
                  ORDER BY t.p, t.j DESC) l ON l.p = e.i);
END
$$;

-- 결과의 줄들을 행으로 (stet.undo의 결과, stet.undo_record의 기록). items가 stet.undo_result가 만드는 꼴이 아니면 무엇이든
-- 22023으로 거절한다: JSON 배열이고, 줄마다 정확히 일곱 키(target_table, key, action, outcome, reason, warning,
-- change_ids)를 가진 객체이며, target_table은 글자, key, reason, warning은 글자나 null, action은 null이나 delete, insert,
-- update, outcome은 restored, restored_differs, skipped, failed, not_captured, change_ids는 null이나 bigint의 범위에 드는
-- 정수의 배열이다. stet.undo_record(SECURITY DEFINER)가 받은 것을 믿지 않고 이 함수로 확인한다. 식이나 이름을 평가하지 않는다.
CREATE OR REPLACE FUNCTION stet.undo_unpack(items jsonb)
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
  e   jsonb;
  n   bigint;
  bad boolean;
BEGIN
  IF jsonb_typeof(undo_unpack.items) IS DISTINCT FROM 'array' THEN
    RAISE EXCEPTION 'the lines of an undo must be a JSON array' USING ERRCODE = 'invalid_parameter_value';
  END IF;
  FOR e, n IN SELECT x.v, x.n FROM jsonb_array_elements(undo_unpack.items) WITH ORDINALITY AS x(v, n) ORDER BY x.n LOOP
    bad := jsonb_typeof(e) <> 'object';
    IF NOT bad THEN
      bad := (SELECT array_agg(k.name ORDER BY k.name COLLATE pg_catalog."C") FROM jsonb_object_keys(e) AS k(name))
             IS DISTINCT FROM '{action,change_ids,key,outcome,reason,target_table,warning}'::text[];
    END IF;
    IF NOT bad THEN
      bad := jsonb_typeof(e->'target_table') <> 'string'
             OR jsonb_typeof(e->'key') NOT IN ('string', 'null')
             OR e->'action' NOT IN ('null', '"delete"', '"insert"', '"update"')
             OR e->'outcome' NOT IN ('"restored"', '"restored_differs"', '"skipped"', '"failed"', '"not_captured"')
             OR jsonb_typeof(e->'reason') NOT IN ('string', 'null')
             OR jsonb_typeof(e->'warning') NOT IN ('string', 'null')
             OR jsonb_typeof(e->'change_ids') NOT IN ('array', 'null');
    END IF;
    IF NOT bad AND jsonb_typeof(e->'change_ids') = 'array' THEN
      bad := EXISTS (SELECT FROM jsonb_array_elements(e->'change_ids') AS c(v)
                      WHERE jsonb_typeof(c.v) <> 'number' OR c.v::text !~ '^-?[0-9]{1,19}$'
                         OR c.v::text::numeric NOT BETWEEN -9223372036854775808 AND 9223372036854775807);
    END IF;
    IF bad THEN
      RAISE EXCEPTION 'line % of the undo is not a line stet.undo makes: %', n, left(e::text, 200)
        USING ERRCODE = 'invalid_parameter_value';
    END IF;
    target_table := e->>'target_table';
    key := e->>'key';
    action := e->>'action';
    outcome := e->>'outcome';
    reason := e->>'reason';
    warning := e->>'warning';
    change_ids := CASE WHEN jsonb_typeof(e->'change_ids') = 'array'
                       THEN ARRAY(SELECT (c.v #>> '{}')::bigint FROM jsonb_array_elements(e->'change_ids') WITH ORDINALITY AS c(v, o)
                                   ORDER BY c.o) END;
    RETURN NEXT;
  END LOOP;
END
$$;

-- 기록 (7장 "기록"). stet.undos에 한 줄, stet.undo_items에 줄마다 한 줄, stet.undone_changes에 되돌린 줄(restored,
-- restored_differs)의 변경마다 한 줄을 부른 쪽의 트랜잭션 안에서 적고 그 id를 돌려준다.
-- 검토자는 세 표의 권한이 없으므로 소유자(Stet의 계정)의 권한으로 돈다(SECURITY DEFINER). 그래서 받은 것을 믿지 않는다.
-- - by는 session_user다. 인자로 받지 않는다. SET ROLE이나 남의 SECURITY DEFINER 함수를 거쳐도 접속 계정이 남는다. 남의
--   이름으로 기록을 만들 수 없다. 자기 이름으로 지어낸 기록은 만들 수 있다(스펙 11장의 한계).
-- - stet.undo_refuse의 다섯 거절을 똑같이 한다(자기 쓰기의 되돌리기를 기록할 수 없다).
-- - items는 stet.undo_unpack으로 꼴을 확인하고 행으로 읽는다(22023). 세 수는 이 함수가 줄에서 센다: restored는 restored와
--   restored_differs, skipped는 skipped와 not_captured, failed는 failed다.
-- - 줄은 받은 그대로 적지만, stet.undone_changes(stet.history의 undo_id, undone_at)에는 되돌린 줄의 change_ids 가운데
--   고른 조건에 드는 변경만 적는다: agent_role의 묶음의 변경이고, at이 since 이상 until 미만이고(until이 NULL이면 지금까지),
--   tables가 있으면 target_table이 그 안에 있고, 그 줄의 target_table과 key의 변경이다. stet.undo의 줄은 언제나 그렇다.
--   그래서 직접 부른 검토자도 남의 계정, 다른 테이블이나 시간, 없는 변경을 되돌렸다고 보이게 할 수 없다.
--   변경은 id로 하나씩 찾는다: 변경의 id에서 stet.changes의 기본 키로, 거기서 그 묶음의 기본 키로 간다. 두 읽기를
--   저마다 LATERAL 안에 두고 OFFSET 0으로 막는다. 그 안에는 기본 키의 같음만 있고, 고른 조건은 밖에서 거른다(OFFSET이
--   있는 하위 질의 안으로는 조건이 들어가지 않는다). 막지 않으면 플래너가 줄과 묶음을 target_table로 먼저 묶거나 묶음을
--   (agent_role, at)의 인덱스로 훑어, 변경마다 그 테이블이나 그 계정의 묶음을 모두 읽는다. 묶음이 많은 되돌리기에서
--   기록이 변경 수와 묶음 수의 곱만큼 걸렸다.
-- 부른 쪽의 식이나 이름을 평가하지 않고, 동적 SQL이 없으며, 고객의 코드를 부르지 않는다. 쓰는 곳은 세 표뿐이다.
CREATE OR REPLACE FUNCTION stet.undo_record(agent_role text, since timestamptz, until timestamptz, tables text[],
                                            note text, items jsonb)
RETURNS bigint
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
  new_id bigint;
BEGIN
  PERFORM stet.undo_refuse(undo_record.agent_role, undo_record.since, undo_record.until);
  INSERT INTO stet.undos AS u (at, by, agent_role, since, until, tables, note, restored, skipped, failed)
  SELECT clock_timestamp(), session_user, undo_record.agent_role, undo_record.since, undo_record.until,
         undo_record.tables, undo_record.note,
         count(*) FILTER (WHERE r.outcome IN ('restored', 'restored_differs')),
         count(*) FILTER (WHERE r.outcome IN ('skipped', 'not_captured')),
         count(*) FILTER (WHERE r.outcome = 'failed')
    FROM stet.undo_unpack(undo_record.items) AS r
  RETURNING u.id INTO new_id;
  INSERT INTO stet.undo_items (undo_id, line, target_table, key, action, outcome, reason, warning, change_ids)
  SELECT new_id, r.n, r.target_table, r.key, r.action, r.outcome, r.reason, r.warning, r.change_ids
    FROM stet.undo_unpack(undo_record.items) WITH ORDINALITY AS r(target_table, key, action, outcome, reason, warning,
                                                                  change_ids, n);
  INSERT INTO stet.undone_changes (change_id, undo_id)
  SELECT DISTINCT ch.id, new_id
    FROM stet.undo_unpack(undo_record.items) AS r
   CROSS JOIN LATERAL unnest(r.change_ids) AS c(id)
   CROSS JOIN LATERAL (SELECT x.id, x.key, x.set_id FROM stet.changes x WHERE x.id = c.id OFFSET 0) ch
   CROSS JOIN LATERAL (SELECT y.target_table, y.agent_role, y.at FROM stet.change_sets y WHERE y.id = ch.set_id OFFSET 0) cs
   WHERE r.outcome IN ('restored', 'restored_differs')
     AND cs.agent_role = undo_record.agent_role
     AND cs.at >= undo_record.since
     AND (undo_record.until IS NULL OR cs.at < undo_record.until)
     AND (undo_record.tables IS NULL OR (cs.target_table = ANY (undo_record.tables)) IS TRUE)
     AND cs.target_table = r.target_table AND ch.key = r.key;
  RETURN new_id;
END
$$;

-- 되돌리기 (7장 "stet.undo"). 결과는 stet.undo_plan과 같은 일곱 컬럼, 같은 순서(target_table, 그 키의 첫 변경)의 줄이다.
-- outcome은 restored, restored_differs, skipped, failed, not_captured다. action은 stet.undo가 하기로 정한 것이고, 건너뛴
-- 줄과 정하기 전에 실패한 줄은 NULL이다. warning은 되돌린 줄(restored, restored_differs)에만 붙는다.
-- 부른 계정의 권한으로 돌고(SECURITY INVOKER) SET 절이 없다. 몸통에 대해서는 이 절의 머리 주석을 본다.
-- 1. 거절(stet.undo_refuse, ST006): 인자가 틀렸거나 트랜잭션이 READ COMMITTED가 아니다. 에러를 내는 것은
--    이것과, 이 함수와 키마다의 블록 밖에서 부르는 도우미의 EXECUTE가 없는 것(42501), 시간 초과와 취소(57014), 읽기
--    전용 트랜잭션(25006, 기록을 적을 때. 그 전에 모든 키가 failed다)뿐이다. 미룬 제약(DEFERRABLE INITIALLY DEFERRED의
--    외래 키와 유일 제약, 제약 트리거, 검토자의 SET CONSTRAINTS)은 커밋할 때 확인되므로 그 실패는 검토자의 COMMIT에서
--    나오고 되돌리기 전체가 롤백된다.
-- 2. 고른 줄들을 한 번 읽는다(stet.undo_lines).
-- 3. 바퀴마다 stet.undo_next가 정한 키들을 차례로 해 본다. 키 하나는 예외 블록(하위 트랜잭션) 하나다:
--    a. stet.undo_judge(..., lock => true): 테이블을 잠그고(ACCESS SHARE) 그 행을 FOR UPDATE로 잠근 뒤 여섯 확인을 다시
--       한다. 걸리면 건너뛴다(skipped와 그 이유).
--    b. stet.undo_referenced: 연쇄하는 외래 키로 가리키는 행이 있으면 건너뛴다(skipped와 'referenced: <테이블들>').
--    c. stet.undo_statement가 돌려준 문장을 EXECUTE한다. 바꾼 행 수(ROW_COUNT)와 그 행의 tableoid를 받는다.
--    d. stet.undo_outcome: 실행한 문장이 지금의 이름으로 만들 문장과 같은지, 한 행만 relid에 썼는지 보고, 그 키의 행을
--       다시 읽어 B와 비교한다. restored 또는 restored_differs이고, 경고(stet.undo_warning)를 붙인다.
--    건너뛰는 키는 그 키의 블록을 되돌리고 나온다. 블록 안에서 SQLSTATE ST007을 내고 그것을 잡는 처리기에서 skipped로
--    적는다(변수의 값은 되돌려지지 않으므로 이유가 남는다). 그래서 a에서 잡은 행 잠금과 테이블 잠금, 하위 트랜잭션 id가
--    검토자가 커밋하기를 기다리지 않고 풀린다. 다시 해 볼 때는 판단이 다시 잠근다. ST007은 Stet이 이 일에만 쓰는 값이지만
--    고객의 트리거도 낼 수 있으므로, 처리기는 그 직전에 세운 표시(skipping)로 가린다. 표시를 세운 다음 문장이 바로 그
--    RAISE이고 그 사이에 고객의 코드가 돌지 않는다. 표시가 없는 ST007은 다른 에러와 같이 failed다.
--    블록 안의 다른 에러는 그 키의 블록만 되돌리고(잡은 행 잠금과 쓴 것이 함께 풀린다) failed와 'SQLSTATE <코드>:
--    <메시지>'다. WHEN OTHERS가 잡지 않는 assert_failure(고객 트리거의 ASSERT)도 잡는다. 잡지 않는 것은 취소와 시간
--    초과(57014)뿐이고, 그때는 이 함수가 에러로 끝나 아무것도 기록되지 않는다. READ COMMITTED에서는 교착(40P01)도 직렬화
--    실패(40001, 이 격리 수준에서는 고객의 코드만 낸다)도 하위 트랜잭션을 되돌리면 트랜잭션이 계속 쓸 수 있으므로 failed로
--    적는다. 교착이면 서버가 그 키의 잠금 대기를 끝낸 것이다. 그 키의 블록이 잡은 것은 함께 풀리고, 앞서 되돌린 키의 잠금을
--    기다리던 상대는 검토자의 트랜잭션이 끝나면 나아간다. 23503, 23505, referenced 말고는 다시 해 보지 않는다.
-- 4. 바퀴는 stet.undo_next가 더 해 볼 키를 내지 않을 때 끝난다.
-- 5. restored인 키를 모두 다시 읽어, 그 사이에 뒤의 키의 복구가 바꾼 것을 restored_differs로 고친다(stet.undo_recheck).
--    그래서 돌려주는 줄은 돌려줄 때 참이다.
-- 6. 결과의 줄을 만들고(stet.undo_result), 기록하고(stet.undo_record), 돌려준다(stet.undo_unpack).
CREATE OR REPLACE FUNCTION stet.undo(agent_role text, since timestamptz, until timestamptz DEFAULT NULL,
                                     tables text[] DEFAULT NULL, note text DEFAULT NULL)
RETURNS TABLE (target_table text, key text, action text, outcome text, reason text, warning text, change_ids bigint[])
LANGUAGE plpgsql
AS $$
DECLARE
  lines    pg_catalog.jsonb;
  k        pg_catalog.record;
  tried    pg_catalog.int4[] := '{}';
  actions  pg_catalog.text[] := '{}';
  outcomes pg_catalog.text[] := '{}';
  codes    pg_catalog.text[] := '{}';
  reasons  pg_catalog.text[] := '{}';
  warnings pg_catalog.text[] := '{}';
  done     pg_catalog.int4 := 0;
  started  pg_catalog.int4;
  act      pg_catalog.text;
  outc     pg_catalog.text;
  why      pg_catalog.text;
  wrn      pg_catalog.text;
  code     pg_catalog.text;
  msg      pg_catalog.text;
  stmt     pg_catalog.text;
  written  pg_catalog.oid;
  changed  pg_catalog.int8;
  skipping pg_catalog.bool;
  items    pg_catalog.jsonb;
BEGIN
  PERFORM stet.undo_refuse(undo.agent_role, undo.since, undo.until);
  lines := stet.undo_lines(undo.agent_role, undo.since, undo.until, undo.tables);
  LOOP
    started := pg_catalog.cardinality(tried);
    FOR k IN SELECT nx.* FROM stet.undo_next(lines, tried, outcomes, codes, reasons, done) AS nx LOOP
      act := NULL;
      outc := NULL;
      why := NULL;
      wrn := NULL;
      code := NULL;
      skipping := false;
      BEGIN
        SELECT j.action, j.reason INTO act, why
          FROM stet.undo_judge(k.relid, k.key_column, k.key, k.b, k.a, k.key_changed, k.interleaved, true) AS j;
        IF why IS NULL THEN
          why := stet.undo_referenced(k.relid, k.key_column, k.key, act, k.b, k.a);
        END IF;
        IF why IS NOT NULL THEN
          skipping := true;
          RAISE EXCEPTION 'stet.undo skips this key' USING ERRCODE = 'ST007';
        END IF;
        stmt := stet.undo_statement(k.relid, k.key_column, k.key, act, k.b, k.a);
        written := NULL;
        changed := NULL;
        IF stmt IS NOT NULL THEN
          EXECUTE stmt INTO written;
          GET DIAGNOSTICS changed = ROW_COUNT;
        END IF;
        outc := stet.undo_outcome(k.relid, k.key_column, k.key, act, k.b, k.a, stmt, changed, written);
        wrn := stet.undo_warning(k.relid, act, k.also_changed);
      EXCEPTION
        WHEN SQLSTATE 'ST007' THEN
          GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, msg = MESSAGE_TEXT;
          IF skipping THEN
            act := NULL;
            outc := 'skipped';
            code := NULL;
          ELSE
            outc := 'failed';
            why := stet.undo_failure(code, msg);
          END IF;
          wrn := NULL;
        WHEN OTHERS OR assert_failure THEN
          GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, msg = MESSAGE_TEXT;
          outc := 'failed';
          why := stet.undo_failure(code, msg);
          wrn := NULL;
      END;
      tried := pg_catalog.array_append(tried, k.pos);
      actions := pg_catalog.array_append(actions, act);
      outcomes := pg_catalog.array_append(outcomes, outc);
      codes := pg_catalog.array_append(codes, code);
      reasons := pg_catalog.array_append(reasons, why);
      warnings := pg_catalog.array_append(warnings, wrn);
    END LOOP;
    EXIT WHEN NOT FOUND;
    done := started;
  END LOOP;
  SELECT rc.checked, rc.why INTO outcomes, reasons
    FROM stet.undo_recheck(lines, tried, outcomes, reasons) AS rc;
  items := stet.undo_result(lines, tried, actions, outcomes, reasons, warnings);
  PERFORM stet.undo_record(undo.agent_role, undo.since, undo.until, undo.tables, undo.note, items);
  RETURN QUERY SELECT r.* FROM stet.undo_unpack(items) AS r;
END
$$;

-- 되돌리기의 함수들은 PUBLIC이 부를 수 없다(7장 "권한"). 테이블 소유자도 stet 스키마의 USAGE를 가지므로(4장) 열어 두면
-- stet.undo_selection으로 남긴 값을 읽을 수 있다. 누가 계획을 볼 수 있는지는 운영자가 정한다. stet.undo_plan을 쓰는
-- 검토자에게는 이것을 준다. 도우미는 계획이 검토자의 권한으로 부르므로 그 EXECUTE도 있어야 한다:
--   GRANT USAGE ON SCHEMA stet TO reviewer;
--   GRANT EXECUTE ON FUNCTION stet.undo_plan(text, timestamptz, timestamptz, text[]),
--                             stet.undo_selection(text, timestamptz, timestamptz, text[]),
--                             stet.undo_lock(oid),
--                             stet.undo_row(oid, text, text, boolean),
--                             stet.undo_judge(oid, text, text, jsonb, jsonb, boolean, boolean, boolean),
--                             stet.undo_warning(oid, text, text[]),
--                             stet.undo_refuse(text, timestamptz, timestamptz) TO reviewer;
-- 그리고 되돌릴 테이블마다 SELECT다(GRANT SELECT ON public.deals TO reviewer). SELECT가 없거나 행 수준 보안이 검토자에게
-- 적용되는 테이블은 키 없는 한 줄로, SQLSTATE 42501로 건너뛴다. 읽을 수 있는지는 session_user와 그것이 SET ROLE로 될 수
-- 있는 역할들로 판단한다. 그래서 여러 역할로 바꿀 수 있는 로그인은 그 가운데 어느 역할이든 읽을 수 있는 것을 본다.
-- 계획과 도우미는 READ COMMITTED 트랜잭션에서만 돈다(ST006).
-- stet.change_sets와 stet.changes의 권한은 주지 않는다. 이 함수들은 아무것도 바꾸거나 적지 않는다.
-- 되돌릴 수도 있는 검토자에게는 위의 것에 더해 이것을 준다. stet.undo가 검토자의 권한으로 이 도우미들을 부른다:
--   GRANT EXECUTE ON FUNCTION stet.undo(text, timestamptz, timestamptz, text[], text),
--                             stet.undo_lines(text, timestamptz, timestamptz, text[]),
--                             stet.undo_next(jsonb, integer[], text[], text[], text[], integer),
--                             stet.undo_referenced(oid, text, text, text, jsonb, jsonb),
--                             stet.undo_statement(oid, text, text, text, jsonb, jsonb),
--                             stet.undo_outcome(oid, text, text, text, jsonb, jsonb, text, bigint, oid),
--                             stet.undo_recheck(jsonb, integer[], text[], text[]),
--                             stet.undo_failure(text, text),
--                             stet.undo_result(jsonb, integer[], text[], text[], text[], text[]),
--                             stet.undo_unpack(jsonb),
--                             stet.undo_record(text, timestamptz, timestamptz, text[], text, jsonb) TO reviewer;
-- 그리고 되돌릴 테이블마다 SELECT, INSERT, UPDATE, DELETE다(GRANT SELECT, INSERT, UPDATE, DELETE ON public.deals TO
-- reviewer). 행을 FOR UPDATE로 잠그는 데 UPDATE가 든다. 없는 것이 있으면 그 키는 failed(42501)다. 그 테이블을 ON DELETE나
-- ON UPDATE의 CASCADE, SET NULL, SET DEFAULT로 가리키는 자식 테이블에는 SELECT가 든다(referenced를 보려고 읽는다). 없으면
-- 그런 키도 failed(42501)다. 고객의 트리거가 쓰는 다른 테이블의 권한도 검토자의 것으로 확인된다.
-- 기록은 stet.undo_record(SECURITY DEFINER)만 적는다. stet.undos, stet.undo_items, stet.undone_changes의 권한은 주지
-- 않는다. 기록은
-- stet.history의 undo_id와 undone_at으로 보인다. stet.undo_plan의 것만 받은 계정은 stet.undo도 stet.undo_record도 부를 수
-- 없으므로 아무것도 바꾸거나 적지 못한다. stet.undo_record를 직접 부르면 자기 이름(session_user)의 기록만 만들 수 있다.
-- 되돌리기도 READ COMMITTED 트랜잭션에서만 돈다:
--   BEGIN ISOLATION LEVEL READ COMMITTED; SELECT * FROM stet.undo('agent', '2026-11-18 09:00+09'); ... COMMIT;
REVOKE EXECUTE ON FUNCTION stet.undo_plan(text, timestamptz, timestamptz, text[]),
                           stet.undo_selection(text, timestamptz, timestamptz, text[]),
                           stet.undo_lock(oid),
                           stet.undo_row(oid, text, text, boolean),
                           stet.undo_judge(oid, text, text, jsonb, jsonb, boolean, boolean, boolean),
                           stet.undo_warning(oid, text, text[]),
                           stet.undo_refuse(text, timestamptz, timestamptz),
                           stet.undo(text, timestamptz, timestamptz, text[], text),
                           stet.undo_lines(text, timestamptz, timestamptz, text[]),
                           stet.undo_next(jsonb, integer[], text[], text[], text[], integer),
                           stet.undo_referenced(oid, text, text, text, jsonb, jsonb),
                           stet.undo_statement(oid, text, text, text, jsonb, jsonb),
                           stet.undo_outcome(oid, text, text, text, jsonb, jsonb, text, bigint, oid),
                           stet.undo_recheck(jsonb, integer[], text[], text[]),
                           stet.undo_failure(text, text),
                           stet.undo_result(jsonb, integer[], text[], text[], text[], text[]),
                           stet.undo_unpack(jsonb),
                           stet.undo_record(text, timestamptz, timestamptz, text[], text, jsonb) FROM PUBLIC;
