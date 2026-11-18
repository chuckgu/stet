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
CREATE INDEX IF NOT EXISTS decisions_backend_idx ON stet.decisions (backend_pid, sent_after);

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
-- 실패할 수 없는 것만 한다. 블록 안에서 에러가 나면 블록이 한 일(묶음, 줄, 세는 값)이 되돌려지고, 묶음 하나를
-- skipped = 'capture_failed'와 detail의 SQLSTATE, 메시지로 남긴다. 그것도 실패하면 서버 로그(RAISE LOG)에만 남긴다.
-- client_min_messages가 warning이므로 클라이언트에게는 아무것도 가지 않는다. WHEN OTHERS는 시간 초과와 취소(57014)를
-- 잡지 않으므로 그때는 쓰기가 실패한다. lock_timeout(55P03)은 잡는다.
-- 한 트랜잭션의 상한. EXCEPTION 블록은 하위 트랜잭션이고, 표에 쓸 때만 자기 트랜잭션 id를 받는다. 커밋된 하위 트랜잭션의
-- id는 트랜잭션이 끝날 때까지 백엔드의 캐시(64개)에 남고, 넘치면 서버 전체의 스냅숏 확인이 느려진다. 되돌려진 하위
-- 트랜잭션은 자리를 돌려준다. 표에 쓰는 캡처 하나는 id 하나만 남긴다: 값이나 표지를 남기면 큰 블록의 것, 실패하면 큰 블록은
-- 되돌려지고 표지를 쓴 작은 블록의 것이다. 그래서 한 트랜잭션에서 표에 쓰는 것은 32번까지다. 쓴 횟수는 트랜잭션 동안의
-- 설정 stet.captures에 센다(계획의 보정 U9). 표에 쓸 때 같은 블록 안에서 세므로 되돌려진 쓰기는 세지 않는다. 31번 썼으면
-- 다음에 쓰는 것은 too_many_statements 표지 하나이고, 그 뒤로는 아무것도 쓰지 않는다. 표에 쓰지 않는 길(캡처하지 않는 세션,
-- 0행)은 세지 않고 쓰지 않는다.
-- 그 설정은 어느 세션이나 적을 수 있다. 그래서 쓴 횟수(done)는 큰 블록의 맨 앞, 실패할 수 있는 다른 어느 것보다 먼저 한
-- 곳에서 정한다. 아홉 자리까지의 숫자가 아니면 0으로 보고, 31보다 작으면 그대로 쓴다. 31 이상이라고 하면 믿지 않고 이
-- 트랜잭션의 id로 남은 묶음을 센다. 에이전트가 큰 값을 적어 자기 쓰기의 캡처를 끄지 못하게 하려는 것이다. 작은 값을 적어
-- 상한을 피하는 것은 막지 않는다(11장). 묶음은 시각이 아니라 트랜잭션 id로 고른다. 프로시저가 COMMIT한 뒤의 트랜잭션에서는
-- 문장의 statement_timestamp()가 그 트랜잭션의 now()보다 이르다(같은 Sync 아래 뒤따르는 문장도 그 트랜잭션에 든다).
-- 묶음의 txid는 txid_current()다. 트랜잭션이 id를 받기 전에 쓰는 표지(0행 문장의 거절)도 그 id를 남기고 세어진다.
-- 큰 블록이 done을 정하기 전에 실패했으면 done은 NULL이고, 표지를 쓰는 작은 블록이 같은 방법으로 센다.
-- 블록 밖에서는 세션의 값을 읽지 않는다. 설정을 읽고 고르는 것도 큰 블록 안이다. 정규식은 앞의 열 글자에만 건다(정규식은
-- 글자마다 4바이트로 넓혀 맞추므로, 아주 긴 값은 그것만으로 메모리 요청이 실패한다).
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
  counted       text;
  claimed       integer := 0;   -- stet.captures가 말하는 횟수
  done          integer;        -- 이 트랜잭션에서 캡처가 표에 쓴 횟수. 정하기 전에는 NULL
  tx            bigint;
  failure       text;
  code          text;
  message       text;
BEGIN
  BEGIN
    -- 한 트랜잭션의 상한. 쓴 횟수를 정한다(위의 주석). 셀 때는 읽기만 하므로 트랜잭션 id를 받지 않는다.
    counted := current_setting('stet.captures', true);
    IF left(counted, 10) ~ '^[0-9]{1,9}$' THEN
      claimed := counted::integer;
    END IF;
    IF claimed < 31 THEN
      done := claimed;
    ELSE
      tx := txid_current_if_assigned();
      SELECT count(*) INTO done FROM stet.change_sets s WHERE s.txid = tx;
    END IF;
    IF done >= 32 THEN
      RETURN NULL;
    END IF;

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

    -- 표에 쓴 것을 센다. 같은 블록 안이므로 블록이 되돌려지면 이것도 되돌려진다.
    PERFORM set_config('stet.captures', (done + 1)::text, true);
    RETURN NULL;
  EXCEPTION WHEN OTHERS THEN
    GET STACKED DIAGNOSTICS code = RETURNED_SQLSTATE, message = MESSAGE_TEXT;
    failure := code || ': ' || message;
  END;

  -- 큰 블록 안의 길은 모두 RETURN으로 끝난다. 여기에 오는 것은 큰 블록이 실패했을 때뿐이다. 새 길을 더할 때도 RETURN으로
  -- 끝낸다. 그렇지 않은 길이 생겨도 detail 없는 표지를 남기지 않게 failure를 본다.
  IF failure IS NULL THEN
    RETURN NULL;
  END IF;
  -- 캡처가 실패했다. 블록 안의 변수(agent, held, changed, done)는 되돌려지지 않고 남아 있다. done을 정하기 전에 실패했으면
  -- 여기서 센다. 상한에 닿았으면 쓰지 않고, 32번째면 too_many_statements 표지를 쓴다. 이 표지를 쓰는 작은 블록도 하위
  -- 트랜잭션이고, 실패하면 자리를 돌려준다.
  BEGIN
    IF done IS NULL THEN
      tx := txid_current_if_assigned();
      SELECT count(*) INTO done FROM stet.change_sets s WHERE s.txid = tx;
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
    PERFORM set_config('stet.captures', (done + 1)::text, true);
  EXCEPTION WHEN OTHERS THEN
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
