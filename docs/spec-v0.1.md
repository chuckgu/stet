# Stet v0.1 기술 스펙

작성: 2026-10-28 · 상태: 초안

## 1. 목표와 비목표

**목표**

- 기존 Postgres 앞에 프록시를 두는 것만으로, 지정한 테이블에 대한 에이전트 쓰기를 판단 모델로 판단한다.
- 모든 판단을 재추론 없이 재구성할 수 있게 기록한다.
- 섀도 모드로 위험 없이 설치하고, 실제 차단(enforce) 모드로 전환할 수 있다.
- 검증 실험(docs/experiment.md)을 돌릴 수 있는 수준까지 만든다.

**비목표 (v0.1)**

- 새 스토리지 엔진, Postgres 확장
- 읽기 측 판단 (쿼리 안의 분류·필터는 pg-jev 등 기존 도구가 있음)
- 의존성 기반 무효화·재평가 (v0.2)
- RLS 기반 권한 위임 강제 (v0.1은 기록만)
- 자체 판단 모델 학습

## 2. 아키텍처

```
에이전트 / 앱 ──pgwire──▶ Stet 프록시 ──pgwire──▶ Postgres
                              │
                              ├─ 1. 파싱: 게이트 대상 쓰기인가?
                              ├─ 2. 상태 조립: 현재 행, 후보, 최근 변경
                              ├─ 3. 판단: 모델 어댑터 호출 (트랜잭션 밖)
                              ├─ 4. 정책: 결정론적 규칙으로 verdict 산출
                              ├─ 5. 실행: commit / hold / escalate / reject
                              └─ 6. 기록: stet.decisions
```

- 게이트 대상이 아닌 문장(읽기, 다른 테이블 쓰기)은 투명하게 통과시킨다.
- 구현: Go (decisions.md D12).
- 중계 방식: 메시지를 프레임(타입 1바이트 + 길이)으로만 나눠 원본 바이트 그대로 전달하고, 검사할 메시지만 `pgx/v5/pgproto3`로 디코딩한다. 프록시가 모르는 메시지도 바뀌지 않고 지나간다.
- flush 규칙: 입력 버퍼에 다음 메시지 헤더가 이미 있으면 모아서 쓰고, 다음 읽기가 블로킹될 수 있으면 먼저 flush한다. Sync 없이 Flush만 보내는 파이프라인 클라이언트나 LISTEN 중인 유휴 클라이언트도 응답을 바로 받는다.
- 시작 단계: SSLRequest와 GSSENCRequest는 `N`으로 거절해 평문으로 진행시키고, CancelRequest는 새 연결로 업스트림에 그대로 전달한다. 인증은 메시지 중계만 하므로 업스트림이 직접 처리한다(SCRAM, MD5, cleartext, trust 확인).

## 3. 쓰기 가로채기

**v0.1이 지원하는 게이트 대상 문장**

| 문장 | 조건 |
|---|---|
| `INSERT` | 단일 행, `VALUES` 리터럴 또는 바인딩 파라미터 |
| `UPDATE` | `WHERE <pk> = <값>` 형태로 단일 행 지정 |
| `DELETE` | `WHERE <pk> = <값>` 형태로 단일 행 지정 |

Simple Query와 Extended Query(Parse/Bind/Execute) 둘 다 처리한다. 바인딩 파라미터는 Bind 메시지에서 값을 채운다.

**지원하지 않는 형태**: 게이트 대상 테이블에 쓰지만 v0.1이 판단하지 않는 문장이다. 괄호 안은 기록할 reason이다.

| 형태 | reason |
|---|---|
| 다중 행 `INSERT ... VALUES (...), (...)` | `multi_row` |
| `INSERT ... SELECT` | `insert_select` |
| `INSERT ... ON CONFLICT` (upsert) | `on_conflict` |
| PK 등호 하나가 아닌 `WHERE` (범위, 추가 조건, `WHERE` 없음, `CURRENT OF`, `= NULL`) | `not_single_row` |
| `UPDATE ... FROM`, `DELETE ... USING` | `join` |
| `SET (a, b) = (...)` | `multi_assign` |
| `SET tags[1] = ...` 같은 부분 갱신 | `indirection` |
| 값·조건 안의 서브쿼리 | `subquery` |
| 게이트 대상 쓰기 앞의 `WITH` | `with` |
| `COPY ... FROM`, `TRUNCATE`, `MERGE` | `copy` / `truncate` / `merge` |
| 다른 문장 안에 들어간 쓰기 (데이터 변경 CTE, `EXPLAIN ANALYZE`, `PREPARE`) | `nested_write` |
| Query 메시지 하나에 여러 문장이 있고 그중 하나라도 게이트 대상 쓰기 | `multi_statement` |

- 섀도 모드: 통과시키고 `verdict = 'unsupported'`로 기록한다.
- enforce 모드: 테이블별 설정 `unsupported: pass | reject`를 따른다. 기본값은 `reject`다.

**판별 규칙**

- 쓰기는 문장 트리 전체에서 찾는다. 최상위가 `SELECT`여도 CTE 안에서 게이트 대상 테이블을 바꾸면 대상이다.
- 테이블 이름: 설정 키가 스키마 없는 이름이면 모든 스키마의 같은 이름 테이블에 걸린다. 스키마를 붙인 키(`crm.contacts`)는 같은 스키마의 참조와 스키마 없는 참조에 걸린다. search_path는 파싱 시점에 알 수 없으므로 넓게 잡는다. 넓게 잡으면 판단할 쓰기가 늘어날 뿐이다.
- 값: 리터럴과 `$n` 파라미터는 값으로, `'x'::text` 같은 캐스트는 안쪽 값으로 기록한다. `now()`, `lower($1)` 같은 표현식은 SQL 텍스트로 기록한다.
- 한계: 문장에 드러난 쓰기만 본다. 함수, `DO` 블록, 트리거, 룰 안에서 일어나는 쓰기는 보지 못한다. 이런 경로는 Postgres 권한(결정론적 제약)으로 막는다.
- 문자열 사전 필터(SQL에 테이블 이름이 있는지 먼저 보는 방식)는 쓰지 않는다. 따옴표 식별자나 유니코드 이스케이프로 우회할 수 있다. 파싱 비용은 문장당 10~22µs다.

**트랜잭션**

- v0.1에서 게이트 대상 쓰기는 autocommit 단일 문장이어야 한다.
- 명시적 트랜잭션(`BEGIN` 이후) 안의 게이트 대상 쓰기:
  - 섀도 모드: 통과시키고 기록한다.
  - enforce 모드: `ST002`로 거부한다.
- 이유: 판단을 트랜잭션 밖에서 하려면 판단과 실행 사이에 트랜잭션이 열려 있으면 안 된다. 다중 문장 트랜잭션 지원은 v0.2에서 제안–확정 방식으로 푼다.
- 판별: 프록시는 ReadyForQuery 하나로 끝나는 단위(Query 하나, 또는 Sync까지의 확장 프로토콜 메시지)로 메시지를 묶는다. 다음 중 하나면 트랜잭션 안의 쓰기로 본다.
  - 단위 앞이나 뒤의 ReadyForQuery 상태가 `I`가 아니다(명시적 트랜잭션).
  - 한 Sync 안에 Execute가 둘 이상 있다(암묵적 트랜잭션. pgx 배치, 파이프라인).
- 실행 결과도 같은 단위에서 정한다. Execute마다 CommandComplete를 세고, ErrorResponse가 몇 번째 Execute에서 났는지 보고 각 쓰기의 `executed`와 `exec_error`를 정한다. 앞 문장이 실패해 건너뛴 쓰기는 `not executed: an earlier statement in the same Sync failed`로 남는다. `executed`는 문장이 실행됐다는 뜻이고 커밋 여부는 아니다.

**에이전트 식별**

- `agent_id`: 접속 시작 파라미터 `application_name`. enforce 모드에서는 필수.
- `on_behalf_of`: `SET stet.on_behalf_of = '...'`. Postgres는 점이 들어간 사용자 설정을 그대로 받으므로 문장은 그대로 통과시키고, 프록시는 문장을 보고 세션 값으로 보관한다. `RESET`, `SET ... TO DEFAULT`, `RESET ALL`은 값을 지운다. `set_config()` 함수 호출은 보지 못한다.
- v0.1은 이 값들을 기록만 하고 권한 강제에는 쓰지 않는다.

## 4. 상태 조립

판단 품질의 절반은 상태가 결정한다. Jev류 모델은 낡은 상태 요약을 받으면 그대로 따르고, 입력에서 물려받은 오류는 confidence로 잡지 못한다. 그래서 상태는 매번 원천에서 새로 조립한다.

```json
{
  "table": "contacts",
  "entity": "B2B CRM contact",
  "operation": "update",
  "proposed": { "...": "에이전트가 쓰려는 값" },
  "current": { "...": "대상 행의 현재 값 (insert면 null)" },
  "candidates": [ { "...": "중복·관련 후보 행, 최대 10개" } ],
  "recent_changes": [
    { "at": "...", "by_agent": "...", "fields": { "...": "..." } }
  ],
  "policy": "테이블 정책 자연어",
  "provenance": {
    "proposed": "untrusted_agent_input",
    "current": "database",
    "candidates": "database",
    "recent_changes": "earlier_agent_writes"
  }
}
```

- 시점: 상태는 쓰기 메시지(Query 또는 Execute)를 업스트림에 보내기 직전에, Stet 자신의 연결로 읽는다. 쓰기 전 값을 보기 위해서다. 클라이언트는 이 동안만 기다린다(상한 500ms). 넘거나 실패하면 쓰기는 그대로 통과하고 기록에 `state_error`가 남는다.
- `proposed`: 리터럴과 바인딩 값은 텍스트로 넣는다. 바이너리 바인딩 값은 Postgres가 문장의 파라미터 타입으로 직접 텍스트로 바꾼다. `DEFAULT`는 `{"default": true}`, 표현식은 바인딩 값을 넣은 SQL 텍스트로 `{"expr": "lower('A@B.com')"}`처럼 넣는다. 컬럼 목록 없는 insert는 테이블 컬럼 순서로 이름을 붙인다.
- `current`: 대상 행을 PK로 조회한다. 이때 행 버전(`xmin`)을 함께 읽어 `state_version`으로 보관한다.
- `candidates`: 테이블 설정의 후보 조회 SQL을 실행한다. `:name` 파라미터는 쓰기 후의 행(현재 행에 proposed를 덮은 값)으로 채운다. 대상 행 자신은 뺀다. 후보 SQL이 고른 컬럼이 그대로 모델로 가므로, 후보 SQL이 곧 후보의 허용 목록이다. duplicate 질문의 선택지를 만들려면 PK 컬럼을 골라야 한다.
- `recent_changes`: 같은 행(테이블 + PK)에 대한, 실행된 최근 쓰기다(`recent_window` 이내, 기본 30분, 최대 10개). 기록된 판단과 아직 판단 중인 쓰기(프로세스 메모리)를 합친다. 판단이 비동기라서 메모리 쪽이 없으면 방금 끝난 다른 에이전트의 쓰기를 놓친다.
- 필드 허용 목록(`send_fields`)에 있는 컬럼만 모델로 보낸다. `proposed`, `current`, `recent_changes`에 적용한다. 판단 모델 API는 외부 서비스라 데이터가 조직 밖으로 나간다. 그래서 `send_fields`는 필수 설정이다.
- 상태 크기에 상한을 둔다. 넘으면 후보 수부터 줄인다. (미구현)

## 5. 판단

**모델 어댑터 인터페이스**

```go
type Decider interface {
    Decide(ctx context.Context, state json.RawMessage, qs []Question) (Answers, error)
    ModelID() string       // 예: "jev"
    ModelVersion() string  // 고정 버전. "latest" 금지
}

type Question struct { ID, Text string; Choices []string }
type Answer   struct { Choice string; Confidence float64; Distribution map[string]float64 }
type Answers  struct { ByQuestion map[string]Answer; CostUSD float64 }
```

질문은 `choice`와 `score` 두 타입이다. 선택지마다 뜻을 적은 설명을 함께 보낸다. policy는 `"1"`~`"5"` 단계의 score 질문이고, `Expected`(확률 가중 평균 단계)가 있으면 6장 규칙이 그 값으로 임계값을 비교한다. 질문과 설명은 상태 필드를 백틱으로 이름 붙여 가리킨다(`proposed`, `candidates` 등).

비교 실험용으로 LLM 어댑터(JSON 출력)를 같은 인터페이스로 만든다(미구현).

**구현된 어댑터**

- `none`: 모델을 부르지 않고 상태, 쓰기, 실행 결과만 기록한다(verdict `commit`, reason `no_model`). 버전 설정이 필요 없다. 실험 1단계("섀도 모드로 쓰기 스트림 기록")에 쓴다.
- `jev`: TypeSafe API `POST https://api.typesafe.ai/v1/systemone` (internal/decide/jev). 근거는 docs.typesafe.ai의 OpenAPI 스펙(v0.2.0)이다.
  - 키는 환경 변수 `TYPESAFE_API_KEY`에서만 읽는다. 설정 파일에 두지 않는다. `TYPESAFE_BASE_URL`로 주소를 바꿀 수 있다.
  - 버전은 `jev-X.Y.Z` 형식만 받는다. `jev-latest`, `jev-preview`, `jev-1.14`는 거부한다. 응답의 `model`이 요청한 버전과 다르면 그 답을 버리고 모델 오류로 처리한다(규칙 2).
  - choice 질문은 Jev Choice로 보낸다. criteria는 선택지 이름과 설명의 맵이다. score 질문은 Jev Score로 보낸다. criteria는 단계 설명의 배열이고, Jev의 0부터 시작하는 단계를 질문의 척도(1~5)로 옮긴다. `Choice`는 확률이 가장 높은 단계, `Expected`는 Jev의 `score`(기댓값)다.
  - 429·529는 한 번만 재시도한다. `Retry-After`를 따르되 마감 시간 안에 들어올 때만 기다린다.
  - 비용은 입력 토큰당 $42/10억(출력 무료, 2026-10-28 기준 가격)으로 계산해 `cost_usd`에 남긴다.
  - Jev의 알려진 약점(TypeSafe 문서의 jev-1.13 jaggedness)은 다음과 같다.
    - 날짜·시각 비교를 잘 못한다. 그래서 `recent_changes`에 `minutes_before_this_write`를 코드로 계산해 넣는다.
    - 첫 번째 선택지 쪽으로 기운다.
    - state 안의 적대적 문구에 흔들린다.
    - 영어가 아닌 언어(CJK 포함)는 정확도가 낮다.

OpenRouter 경유(`typesafe/jev-1.14`)는 쓰지 않는다. 버전 고정과 응답 모델 확인을 TypeSafe API에서 바로 할 수 있기 때문이다.

**기본 질문 세트**

| id | 유형 | 질문 | 선택지 |
|---|---|---|---|
| `duplicate` | choice | Does the proposed record refer to the same real-world entity as one of the candidates? | 후보 id들 + `none` |
| `conflict` | choice | Does the proposed change contradict information in the current state or recent changes that is more recent or more authoritative? | `yes` / `no` |
| `policy` | score 1–5 | How strongly does the proposed change violate the table policy? | 1 위반 없음 ~ 5 명백한 위반 |

- 한 요청에 질문을 묶어 보낸다. 상태는 한 번만 읽힌다.
- `duplicate`는 insert에만, `conflict`는 update/delete에만 묻는다.
- 타임아웃은 설정값(`timeout_ms`)을 따르고, 실패 시 `on_failure` 정책을 적용한다.

## 6. 정책: 판단을 verdict로 바꾸기

모델은 판단만 하고, 무엇을 할지는 결정론적 코드가 정한다. 위에서부터 처음 걸리는 규칙을 적용한다.

| 순서 | 조건 | verdict | reason |
|---|---|---|---|
| 1 | 미지원 문장, 트랜잭션 내 쓰기 | `unsupported` | 형태별 reason(3장) / `in_transaction` |
| 2 | 상태 조립 실패, 모델 호출 실패, 모델 없음 | `on_failure` 설정값 (`pass`면 `commit`) | `state_error` / `model_error` / `no_model` |
| 3 | 어떤 답이든 confidence < `escalate_below` | `escalate` | `low_confidence:<q>` |
| 4 | duplicate ≠ none, confidence ≥ τ_dup | `hold` | `duplicate:<id>` |
| 5 | conflict = yes, confidence ≥ τ_conflict | `hold` | `conflict` |
| 6 | policy score ≥ τ_policy | `hold` | `policy` |
| 7 | 그 외 | `commit` | |

- 1번 규칙의 verdict는 `unsupported`로 기록한다. enforce 모드에서 실제로 어떻게 할지는 테이블 설정 `unsupported`가 정하고, `in_transaction`은 항상 `ST002`다. 1·2번 규칙에 걸리면 모델을 부르지 않는다.
- 물은 질문에 답이 없으면 confidence가 낮은 것으로 보고 3번 규칙(`escalate`)에 건다.
- 물을 질문이 없으면(후보 없는 insert에 정책도 없는 경우 등) 모델을 부르지 않고 `commit`이다.
- v0.1에서 `escalate`는 사람 검토용 hold와 같이 처리하되 reason으로 구분한다. LLM 재판단으로 넘기는 캐스케이드는 P1이다.
- 임계값 초기값은 보수적으로 두고, 섀도 모드 데이터로 보정한다. confidence 0.95를 업무 정확도 95%로 해석하지 않는다.
- 정책 버전(`policy_version`)은 설정 파일 해시로 남긴다.

## 7. 실행

**섀도 모드**: verdict와 무관하게 원래 문장을 그대로 실행한다. 판단 기록에 실제 실행 결과(`executed`, `exec_error`)와 "enforce였다면 어땠을지"(verdict)를 남긴다.

1. 쓰기 메시지를 보내기 직전에 상태를 조립한다(동기, 4장).
2. 원래 메시지를 바꾸지 않고 보낸다.
3. 그 단위의 ReadyForQuery가 오면 실행 결과와 트랜잭션 여부를 정한다(3장).
4. 모델 호출, 정책 적용, 기록은 별도 고루틴에서 한다. 클라이언트는 모델을 기다리지 않는다. 동시에 부르는 모델 호출은 8개로 제한한다.
5. 종료할 때는 프록시를 먼저 닫고, 판단 중인 기록이 끝날 때까지 기다린다.

`latency_ms`는 클라이언트가 실제로 기다린 시간이다(섀도 모드에서는 상태 조립 시간).

**enforce 모드**

- `commit`: 원래 문장을 실행하되, update/delete는 상태 조립 시점의 행 버전이 그대로인지 확인한다(`AND xmin = $observed`). 그 사이 행이 바뀌었으면 한 번 다시 판단하고, 또 바뀌었으면 `hold(reason=concurrent_change)`로 보낸다.
- `hold` / `escalate`: 실행하지 않는다. `stet.held_writes`에 넣고 클라이언트에 에러를 돌려준다.
- `reject`: 실행하지 않고 에러를 돌려준다.

**에러 형식**

LLM 에이전트는 에러 메시지를 읽고 행동을 고친다. 그래서 메시지에 이유와 대안을 담는다.

```
SQLSTATE ST001
ERROR:  write held by stet: possible duplicate of contacts.id=4812 (confidence 0.93)
DETAIL: decision=7f3c2a1e held_write=b81d09c4
HINT:   update contacts.id=4812 instead of inserting a new row
```

| SQLSTATE | 의미 |
|---|---|
| `ST001` | hold 또는 escalate (검토 대기) |
| `ST002` | v0.1 미지원 형태 (트랜잭션 내 쓰기 등) |
| `ST003` | 정책에 따른 reject |
| `ST004` | `stet.decisions` append-only 위반 (DB 트리거가 냄) |

**보류 건 처리**: `stet.approve(held_write_id)` / `stet.reject(...)` 함수로 처리한다. 승인 시 원래 문장을 버전 확인과 함께 실행한다. 검토 UI는 매니지드 영역이다.

## 8. 판단 기록 스키마

`stet.decisions`의 정본은 [`internal/store/schema.sql`](../internal/store/schema.sql)이다. Stet은 시작할 때마다 이 파일을 실행한다(advisory lock으로 직렬화, 여러 번 실행해도 안전). 처음 초안에서 바뀐 점은 다음과 같다.

- `params jsonb` 추가: 바인딩 값의 텍스트 배열. 실험에서 쓰기 스트림을 재생하려면 문장과 값이 둘 다 있어야 한다.
- `op`에 `merge`, `copy`, `truncate` 추가: 미지원 형태도 기록하기 때문이다.
- `proposed`, `state`, `questions`, `answers`는 NULL 허용: 판단하지 않은 쓰기(미지원, 상태 조립 실패, 모델을 부르지 않은 경우)가 있기 때문이다.
- `proposed`는 send_fields로 거르기 전의 전체 값이다(같은 DB 안에 남는다). 모델에 실제로 보낸 것은 `state`다.
- `id`는 Stet이 만든다. 아직 판단 중인 쓰기를 최근 변경 목록에서 중복 없이 합치기 위해서다.
- `created_at`은 쓰기가 들어온 시각이다(판단이 끝난 시각이 아니다).
- append-only를 트리거로 강제한다. 라벨 컬럼(`label`, `label_by`, `labeled_at`) 외의 UPDATE, DELETE, TRUNCATE는 `ST004`로 거부된다.

`held_writes`는 enforce 모드에서 만든다(미구현).

```sql
CREATE TABLE stet.held_writes (
  id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  decision_id  uuid NOT NULL REFERENCES stet.decisions(id),
  status       text NOT NULL DEFAULT 'held'
               CHECK (status IN ('held','approved','rejected','expired')),
  resolved_by  text,
  resolved_at  timestamptz,
  applied      boolean NOT NULL DEFAULT false
);
```

- `decisions`는 append-only다. 트리거와 별개로, 프록시 역할에는 `INSERT`와 라벨 컬럼 `UPDATE`만 허용하는 것을 권한다.
- `state`에는 업무 데이터(개인정보 포함 가능)가 들어간다. 보존 기간 설정(`retention`)을 둔다.
- 이 테이블이 학습 루프의 원재료다. 상태–선택지–판단–실행–결과–사람 라벨이 한 행에 묶인다.

## 9. 설정 예

```yaml
mode: shadow                 # shadow | enforce
listen: 0.0.0.0:6432
upstream: postgres://app@db:5432/crm

model:
  adapter: jev
  version: jev-1.14.0        # 고정 필수. jev-X.Y.Z 형식만 (latest·preview 거부)
  timeout_ms: 800
  on_failure: pass           # pass | hold

retention: 90d

tables:
  contacts:
    pk: id
    entity: "B2B CRM contact"
    send_fields: [name, email, company_id, title, status, notes]
    candidates:              # pg_trgm 필요
      sql: |
        SELECT id, name, email, company_id, title, updated_at
        FROM contacts
        WHERE lower(email) = lower(:email)
           OR (company_id = :company_id AND similarity(name, :name) > 0.4)
        LIMIT 10
    recent_window: 30m
    questions: [duplicate, conflict, policy]
    policy: |
      Do not set status to 'churned' while the account has an open deal.
      Do not overwrite a title with an older value from a stale source.
    thresholds:
      duplicate: 0.85
      conflict: 0.80
      policy: 4              # score >= 4 → hold
      escalate_below: 0.60
    unsupported: reject
```

- `send_fields`는 테이블마다 필수다. 모델이 볼 수 있는 컬럼을 명시적으로 고르게 하기 위해서다.
- `recent_window`의 기본값은 30분이다.
- Jev 어댑터 전까지는 `model: {adapter: none}`으로 둔다(5장).
- `upstream` URL의 계정은 Stet 자신의 연결(상태 조회, 판단 기록)에 쓴다. 이 계정은 게이트 대상 테이블 SELECT 권한과 `stet` 스키마를 만들 권한이 있어야 한다. 비밀번호는 URL이나 `PGPASSWORD`·`.pgpass`로 준다.

## 10. 실패 처리와 보안

| 상황 | 동작 |
|---|---|
| 모델 API 장애·타임아웃 | `on_failure`: 섀도 모드 기본 `pass`, enforce 모드는 테이블별 설정 |
| 업스트림 Postgres 장애 | 프록시는 에러를 그대로 전달 |
| 상태 조립 SQL 실패·지연 | 판단 생략, `on_failure` 적용, reason `state_error`로 기록. 쓰기는 그대로 통과한다. 상한 500ms, Stet 연결의 `lock_timeout`은 200ms라서 클라이언트 자신의 열린 트랜잭션이 상태 조회를 오래 막지 못한다 |
| 판단 기록 저장 실패 | 에러 로그만 남긴다. 재시도하지 않는다(v0.1 한계) |
| 프록시 장애 | 클라이언트는 Postgres로 직접 붙는 경로를 운영 측에서 유지 (v0.1은 HA 없음) |
| 업스트림 접속 실패 | 클라이언트에 `FATAL 08001`과 원인을 돌려준다 |

**TLS**: v0.1 프록시는 클라이언트의 SSLRequest를 거절하고 업스트림에도 평문으로 붙는다. 그래서 로컬이나 같은 VPC 안의 Postgres에서만 쓸 수 있다. 매니지드 Postgres(Neon, Supabase, RDS)에 붙이려면 업스트림 TLS가 먼저 필요하다(decisions.md 열린 질문).

**판단 대상이 판단자를 조작하는 문제**

에이전트가 쓴 텍스트로 판단을 움직일 수 있다. 실제로 가짜 컴플라이언스 문구로 Jev가 DENY를 고른 사례가 있고, TypeSafe도 Jev가 state를 기본적으로 적대적으로 보지 않는다고 명시한다. 대응은 다음과 같다.

- 상태에 출처(`provenance`)를 표시하고, 에이전트 입력은 `proposed`에만 둔다.
- 결정론적 제약(UNIQUE, FK, CHECK, 버전 확인)을 판단보다 먼저 적용한다. 판단형 제약은 그 위에 얹는 층이다.
- 주입 사례를 실험 세트에 포함해 뒤집힘 비율을 측정한다.

## 11. 성능 목표 (가설, 1주차 측정 후 확정)

| 항목 | 초기 목표 | 실측 |
|---|---|---|
| 게이트 대상 외 문장의 프록시 추가 지연 | p95 < 2ms | 패스스루 p95 +0.02ms, SQL 분류 문장당 0.01~0.02ms |
| 게이트 대상 쓰기의 추가 지연 (Jev) | p95 < 500ms | Jev 단건 p50 169ms, p95 196ms (`jev-1.14.0`). 섀도 모드에서 클라이언트가 기다리는 건 상태 조립뿐이며, 로컬 실행에서 0~5ms |
| 판단 기록 누락 | 0 | 섀도 모드 테스트에서 누락 없음. 저장 실패 시 재시도는 없다 |

패스스루 지연은 `select 1` 2,000회를 직접 연결과 번갈아 돌려 잰 값이다(2026-10-28, 같은 머신의 Postgres 14). `internal/proxy/latency_test.go`가 같은 측정을 하고 2ms 예산을 넘으면 실패한다.

**Jev 단건 지연** (2026-10-28, 개발 머신에서 `api.typesafe.ai`로 측정, `stet bench-jev`)

측정 방법:
- 게이트의 실제 상태와 같은 모양인 CRM 사례 두 가지를 번갈아 보냈다. 하나는 오래된 직함으로 덮어쓰는 update(conflict와 policy 질문), 다른 하나는 표기만 다른 중복 연락처 insert(duplicate와 policy 질문)다.
- 요청은 한 번에 하나씩, 연결을 재사용해 보냈다.

| 모델 | n | p50 | p95 | 최대 | 처음 호출 (TLS 포함) |
|---|---|---|---|---|---|
| `jev-1.14.0` | 60 | 169ms | 196ms | 213ms | 204ms |
| `jev-1.14.0` (재측정) | 40 | 167ms | 199ms | 211ms | 228ms |
| `jev-1.13.0` | 40 | 186ms | 417ms | 461ms | 205ms |
| 네트워크 하한: `GET /v1/models` (추론 없음) | 20 | 142ms | 231ms | 252ms | |

해석:
- 지연의 대부분(약 140ms)은 네트워크 왕복이다. 추론과 처리 몫은 30ms 안팎이다. TypeSafe와 같은 지역에 Stet을 두면 단건 지연이 크게 줄 것으로 보이지만, 아직 재 보지 않았다.
- 현재 기본 `timeout_ms: 800`을 넘은 호출은 없다.
- 호출당 비용은 약 $0.000032(입력 약 760토큰)다. 1,000건당 약 $0.03이다.
- 같은 상태를 반복해 보내면 선택은 매번 같았지만 confidence는 흔들렸다. insert의 policy 답은 0.08~0.62였다. 기록된 판단을 재생하고 재추론하지 않는 이유(규칙 4)가 여기서도 확인된다.

## 12. 수용 기준

- [x] 일반 드라이버(psql, pgx, psycopg)가 프록시 경유로 코드 변경 없이 동작한다.
- [x] 섀도 모드에서 게이트 대상 쓰기 100%가 기록되고, 원래 동작은 바뀌지 않는다. (프로토콜 테스트 전체를 감시 게이트를 붙여 다시 돌려 확인)
- [ ] enforce 모드에서 hold 시 `ST001`이 반환되고 `held_writes`가 생기며, 승인하면 버전 확인과 함께 적용된다.
- [ ] 판단 기록만으로 모든 결정을 재추론 없이 재구성할 수 있다.
- [ ] 모델 장애 시 `on_failure` 설정대로 동작한다. (섀도 모드의 기록된 verdict는 확인, enforce는 미구현)
- [ ] 실험 하네스로 실험의 네 arm을 같은 쓰기 스트림에 대해 실행할 수 있다.

## 13. 범위 밖 (v0.2 이후)

- 다중 문장 트랜잭션: 제안–확정 2단계 쓰기
- 의존성 기반 무효화·재평가: 논리 복제 구독 사이드카
- 세션 읽기 집합 추적: 에이전트가 본 버전과 쓸 때 버전 비교
- RLS 기반 `on_behalf_of` 권한 강제
- LLM 캐스케이드: escalate를 LLM 재판단으로
- MCP 서버: 의미 카탈로그, 구조화된 쓰기 결과
- 보류 건 검토 UI (매니지드)
