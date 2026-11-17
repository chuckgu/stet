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

CREATE INDEX IF NOT EXISTS decisions_target_idx ON stet.decisions (target_table, target_pk, created_at DESC);
CREATE INDEX IF NOT EXISTS decisions_verdict_idx ON stet.decisions (verdict, created_at DESC);
CREATE INDEX IF NOT EXISTS decisions_agent_idx ON stet.decisions (agent_id, created_at DESC);

-- 규칙 3: append-only. 사후 라벨 컬럼만 바꿀 수 있다. 역할 권한과 별개로 DB가 직접 막는다.
CREATE OR REPLACE FUNCTION stet.decisions_append_only() RETURNS trigger
LANGUAGE plpgsql AS $$
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
LANGUAGE plpgsql AS $$
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

-- 새로 만든 함수는 PUBLIC이 실행할 수 있다. 이 파일을 실행할 때마다 거둔다. Migrate는 한 트랜잭션이므로 PUBLIC이
-- 부를 수 있는 틈이 없고, CREATE OR REPLACE는 이미 준 권한을 지우지 않는다. 누가 승인할 수 있는지는 운영자가 정한다:
--   GRANT USAGE ON SCHEMA stet TO reviewer;
--   GRANT SELECT ON stet.review TO reviewer;
--   GRANT EXECUTE ON FUNCTION stet.approve(uuid, text, bigint), stet.reject(uuid, text) TO reviewer;
REVOKE EXECUTE ON FUNCTION stet.approve(uuid, text, bigint), stet.reject(uuid, text) FROM PUBLIC;
