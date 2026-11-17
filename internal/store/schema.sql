-- 판단 기록. docs/spec-v0.1.md 8장. Stet이 시작할 때 매번 실행하므로 여러 번 실행해도 안전해야 한다.

CREATE SCHEMA IF NOT EXISTS stet;

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

-- 승인과 거부 (docs/spec-v0.2-approve.md 4장).
-- Stet이 시작할 때마다 돌므로, 스키마가 이미 맞으면 stet.held_writes를 잠그지 않는다. ALTER TABLE과 DROP TRIGGER는
-- 아무것도 바꾸지 않아도 AccessExclusiveLock을 잡아서, 실행부가 줄을 잠근 동안 다른 Stet이 시작하지 못한다.
-- 그래서 ALTER는 빠진 것이 있을 때만 돌고, 트리거는 없을 때만 만든다. 컬럼을 더하면 아래의 이름 목록에도 더한다.
DO $$
BEGIN
  IF EXISTS (SELECT FROM unnest('{expires_at,settings,row_absent,in_transaction,planned_rows,rows_exact,
                                  resolved_by,resolved_at,note,max_rows,applied_at,rows_affected,error}'::name[]) AS c(name)
             WHERE NOT EXISTS (SELECT FROM pg_attribute
                               WHERE attrelid = 'stet.held_writes'::regclass AND attname = c.name AND NOT attisdropped))
     OR (SELECT pg_get_expr(d.adbin, d.adrelid)
         FROM pg_attrdef d JOIN pg_attribute a ON a.attrelid = d.adrelid AND a.attnum = d.adnum
         WHERE d.adrelid = 'stet.held_writes'::regclass AND a.attname = 'status') IS DISTINCT FROM '''unconfirmed''::text'
  THEN
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
  END IF;
END
$$;
CREATE INDEX IF NOT EXISTS held_writes_approved ON stet.held_writes (resolved_at) WHERE status = 'approved';

-- 보류된 것은 바뀌지 않는다 (docs/spec-v0.2-approve.md 4장, 계획의 보정 B4). 역할 권한과 별개로 DB가 직접 막는다.
-- free의 아홉 컬럼만 바뀔 수 있고, 나머지는 나중에 더해지는 컬럼까지 모두 고정이다. 아홉 가운데 둘에는 규칙이 있다.
-- in_transaction은 unconfirmed에서 pending이 되는 그 갱신에서만 바뀐다. status는 3장의 화살표로만 바뀐다.
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
  RETURN NEW;
END
$$;

-- 트리거는 없을 때만 만든다. 그래서 트리거의 정의(언제, 무엇에)를 바꾸려면 따로 옮겨야 한다.
-- 함수의 본문은 위의 CREATE OR REPLACE가 매번 새로 한다.
DO $$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_trigger WHERE tgrelid = 'stet.held_writes'::regclass AND tgname = 'held_writes_guard') THEN
    CREATE TRIGGER held_writes_guard
      BEFORE UPDATE ON stet.held_writes
      FOR EACH ROW EXECUTE FUNCTION stet.held_writes_guard();
  END IF;
END
$$;
