# Stet v0.2 스펙 2: 접속 계정별 누적 한도와 읽기 기록 (섀도)

작성: 2026-11-16 · 상태: 구현됨 (섀도) · 상위 문서: concept-v0.2.md 9장의 1번 단계, decisions.md D30 · 앞 문서: spec-v0.2-impact.md

## 1. 목표와 비목표

**왜 하는가.** 스펙 1은 문장 하나가 건드리는 행 수를 쟀다. 한도를 문장마다만 보면 한도 아래로 쪼개 보내는 것을 막지 못한다. 그리고 지금 Stet은 읽기를 전혀 보지 않는다. 게이트 대상 테이블에 쓰지 않는 문장은 그대로 지나간다. 읽어 간 것은 되돌릴 수 없으므로, 누가 얼마나 읽는지부터 알아야 한다. D30이 이 둘을 1번 단계에 더했다.

둘 다 "어느 접속 계정이 얼마나 했는가"를 세는 일이다. 세는 기준은 접속 계정이다. `application_name`과 `on_behalf_of`는 클라이언트가 스스로 적는 값이라 한도에 쓰지 않는다(D31).

**목표**

- 접속 계정과 테이블별로, 시간 창 안에 쓰기가 건드린 행 수를 세고 한도를 넘으면 그렇게 판정한다.
- 게이트 대상 테이블을 읽는 문장의 수와 클라이언트에게 실제로 나간 행 수를 접속 계정별로 집계해 남긴다. 큰 읽기는 문장을 따로 남긴다.
- 섀도 모드에서만 동작한다. 원래 문장과 그 결과는 바뀌지 않는다(규칙 6).

**비목표 (이 단계)**

- 읽기에 대한 판정과 한도. 기록만 한다(D30).
- 마스킹.
- 쓰기를 막는 것 (2번 단계).
- 여러 Stet 인스턴스가 누적 값을 나누는 것. v0.1에는 고가용성이 없다.
- 실험 도구(`internal/lab`) 변경.

## 2. 접근

논의에서 고른 것은 셋이다.

| 정할 것 | 고른 것 | 고르지 않은 것과 이유 |
|---|---|---|
| 읽기 기록의 단위 | 접속 계정·테이블·분 단위 집계. 기준을 넘는 큰 읽기만 문장을 따로 남긴다 | 읽기마다 한 줄: 읽기는 쓰기보다 훨씬 많아 기록이 읽기로 채워진다. 집계만: 큰 읽기 하나가 어떤 문장이었는지 남지 않는다 |
| 누적 값을 세는 곳 | 프로세스 메모리. 시작 뒤 처음 볼 때 `stet.decisions`에서 채운다 | 쓰기마다 `stet.decisions`를 합산: 쓰기마다 질의가 하나 늘고, 판단 기록이 비동기로 들어가 방금 끝난 쓰기를 놓친다 |
| 단일 행 쓰기가 누적 한도에 걸렸을 때 | 섀도에서는 판단 모델도 부르고 답을 기록한다. verdict는 누적 한도의 것이다 | 부르지 않기: 섀도 모드의 목적이 데이터를 모으는 것인데 그 쓰기의 값 단위 판단이 남지 않는다. enforce에서는 부르지 않는다 (2번 단계) |

설계하면서 하나를 더 골랐다. 읽기의 행 수를 무엇으로 세느냐다.

| 방법 | 장점 | 단점 |
|---|---|---|
| 완료 태그 (`SELECT 48211`) | 프록시를 거의 고치지 않는다 | 태그 없이 나간 행을 놓친다. 행을 내보내다 취소되거나 실패한 문장, 행 수 상한을 걸고 나눠 가져가는 포털이 그렇다 |
| 클라이언트로 나간 행 메시지를 센다 (고른 것) | 실제로 나간 행을 센다 | 프록시의 서버→클라이언트 경로를 고친다 |

읽기를 기록하는 이유가 "읽어 간 것은 되돌릴 수 없다"이다. 큰 `SELECT`를 도중에 취소하면 이미 나간 행이 바로 기록해야 할 대상인데, 태그로 세면 그것이 0이 된다. 그래서 나간 행 메시지를 센다.

## 3. 공통: 기록에 더하는 것

`stet.decisions`에 컬럼 둘을 더한다.

| 컬럼 | 내용 |
|---|---|
| `login_role text` | 접속 시작 파라미터의 `user`. DB가 인증한 계정이다. 프록시가 시작 패킷을 읽지 못한 세션은 NULL이다. 인증이 끝나기 전(서버의 첫 ReadyForQuery 전)에 보낸 문장도 NULL이다 |
| `rows_affected bigint` | 게이트 대상 테이블에서 그 쓰기가 바꾼 행 수. 완료 태그에서 읽는다(`UPDATE 3`의 3). 모르면 NULL이다 |

- 두 컬럼은 `impact` 설정과 무관하게 모든 기록에 채운다. 기존 컬럼의 값은 바뀌지 않는다.
- 프록시는 세션이 인증된 뒤에만 시작 패킷의 `user`를 게이트에 넘긴다. 인증은 서버의 첫 ReadyForQuery로 안다. 그 전에 온 문장은 접속 계정이 없는 것으로 넘긴다. 시작 패킷은 누구나 아무 계정 이름으로 보낼 수 있으므로, 인증을 마치지 않은 연결이 남의 누적 값과 읽기 집계를 늘리거나 그 계정 이름으로 기록을 남기지 못하게 하려는 것이다. 쓰기의 판단 기록에도, 읽기의 집계와 큰 읽기 기록에도 같이 적용한다.
- `rows_affected`는 완료 태그가 그 테이블의 행 수일 때만 채운다. 메시지에 문장이 하나이고 그 최상위 문장이 게이트 대상 테이블에 대한 쓰기일 때다. 다음은 NULL이다.
  - 실행되지 않은 쓰기.
  - 쓰기가 다른 문장 안에 있는 경우(`nested_write`). 데이터 변경 CTE의 태그는 바깥 `SELECT`의 행 수이고, `EXPLAIN ANALYZE`의 태그에는 수가 없다.
  - 한 메시지에 문장이 여럿인 경우(`multi_statement`). 서버는 문장마다 태그를 보내지만 프록시는 그 가운데 어느 것이 게이트 대상 쓰기의 것인지 모른다.
  - `TRUNCATE`. 태그에 수가 없다.
- `impact.actual_rows`는 그대로 둔다. 측정한 쓰기에서는 `rows_affected`와 같은 값이다.
- 누적 값을 채우는 질의를 위해 `(login_role, target_table, created_at DESC)` 인덱스를 더한다.
- 스키마는 스펙 1과 같은 방식으로 `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`로 더한다. `stet.decisions`는 append-only라서 이 컬럼의 뜻은 나중에 바꿀 수 없다.

## 4. 누적 한도

**설정.** `impact` 아래에 둔다. 없으면 꺼져 있고, 그 테이블의 판정은 지금과 같다.

```yaml
tables:
  contacts:
    # ...
    impact:
      max_rows: 100
      max_share: 0.1
      cumulative:            # 없으면 누적 한도는 꺼져 있다
        max_rows: 500        # 필수. 창 안에 한 접속 계정이 건드린 행이 이보다 많아지면 hold
        window: 10m          # 필수. 창의 길이
```

- `cumulative.max_rows`는 1 이상 10억 이하의 정수이고 `impact.max_rows`보다 작을 수 없다. 작으면 문장 하나가 문장별 한도 안에서 누적 한도를 넘는다.
- `window`는 초 단위로 떨어지는 값이고 1초 이상 24시간 이하다. `500ms`와 `1.5s`는 거부한다. 판단 기록에 `window_s`(초)로 남는데, 그 기록은 나중에 고칠 수 없기 때문이다. 기본값은 두지 않는다. 얼마가 적당한지는 업무에 달려 있어서 운영자가 적는다.
- 값 없이 `cumulative:`만 쓰면(`cumulative: ~`도 같다) 설정 검증에서 거부한다. 오류는 키를 밝히고 `max_rows`와 `window`가 필요하다고 알린다. 숫자가 없는 한도는 뜻이 없다.

**세는 단위.** 접속 계정과 테이블(설정의 키)의 쌍이다. 연결 단위로 세지 않는다. 다시 접속하면 초기화되기 때문이다.

**세는 것.** 그 테이블에 대한, Stet이 감시하는 쓰기 전부다. 형태를 가리지 않는다.

| 쓰기 | 보내기 전에 잡아 두는 수 (`planned`) |
|---|---|
| 단일 행 쓰기 | 1 |
| 측정한 다중 행 쓰기 | 측정값. 한도까지만 센 경우는 그 상한값 |
| 측정하지 못했거나 미지원인 쓰기 | 0 |

보내기 직전(`Begin`)에 `planned`를 잡아 두고, 결과가 오면(`End`) 다음과 같이 확정한다.

| 결과 | 확정하는 수 |
|---|---|
| 실행됐고 `rows_affected`를 안다 | 그 값 |
| 실행됐지만 `rows_affected`를 모른다 (3장의 NULL인 경우) | `planned` 그대로 |
| 서버가 실패했다고 답했다, 또는 앞 문장의 실패로 건너뛰었다 | 지운다 |
| 실패라는 답 없이 ReadyForQuery 전에 연결이 끊겼다 | `planned` 그대로. 쓰기가 이미 커밋됐을 수 있다 |

- 서버가 이미 실패했다고 답했거나 앞 문장의 실패로 건너뛴 쓰기는, ReadyForQuery 전에 연결이 끊겨도 실패로 본다(지운다). 그 밖의 쓰기는 ReadyForQuery 전에 연결이 끊기면 모두 연결이 끊긴 것으로 친다. 완료 태그(CommandComplete)가 이미 왔어도 같다. ReadyForQuery가 오지 않으면 커밋됐는지 알 수 없기 때문이다. 이런 쓰기는 `planned`를 그대로 누적 값에 둔다.
- 먼저 잡아 두는 이유: 결과가 온 뒤에만 더하면, 같은 계정이 동시에 여러 연결로 보낸 쓰기가 서로를 보지 못하고 모두 한도 안으로 판정된다.
- 트랜잭션 안의 쓰기도 센다. 뒤에 롤백됐는지는 보지 않는다. 덜 세는 것보다 더 세는 쪽이 안전하다.
- 창은 미끄러진다. 지금부터 `window` 전까지의 항목만 더한다.

**판정.** `used`는 이 쓰기를 뺀, 창 안의 합이다.

```
used + planned > cumulative.max_rows   →   hold, reason impact:cumulative
```

verdict 순서는 스펙 1의 5장 표에 한 줄을 넣은 것이다. 위에서부터 처음 걸리는 것을 적용한다.

| # | 조건 | verdict | reason |
|---|---|---|---|
| 1 | 미지원 형태 | `unsupported` | 지금의 reason |
| 2 | 트랜잭션 안의 쓰기 | `unsupported` | `in_transaction` |
| 3 | 측정 대상인데 측정에 실패했다 | `unsupported` | 지금의 reason |
| 4 | 측정한 행 수가 `max_rows`를 넘는다 | `hold` | `impact:rows` |
| 5 | 측정한 비율이 `max_share`를 넘는다 | `hold` | `impact:share` |
| 6 | **누적 한도를 넘는다** (단일 행 쓰기와 측정한 쓰기) | `hold` | `impact:cumulative` |
| 7 | 측정한 쓰기: 방법이 추정뿐이다 | `escalate` | `impact:uncertain` |
| 8 | 측정한 쓰기: 그 외 | `commit` | |
| 9 | 단일 행 쓰기: 지금의 순서 그대로 (상태·모델 실패, 모델의 답) | 지금과 같다 | 지금과 같다 |

- 1~3번에 걸린 쓰기는 verdict가 바뀌지 않지만 누적 값에는 더해진다.
- 단일 행 쓰기가 6번에 걸려도 섀도 모드에서는 상태를 조립하고 판단 모델을 부른다. 질문과 답은 지금처럼 기록되고, verdict와 reason만 6번의 것이다. 답이 남아 있으므로 한도를 바꿔 다시 채점할 수 있다.

**기록.** `stet.decisions`에 `cumulative jsonb` 컬럼을 더한다. 누적 한도가 켜진 테이블의 쓰기에만 채운다.

```json
{"used": 480, "planned": 20, "limit": 500, "window_s": 600}
```

- `impact` 컬럼에 넣지 않는 이유: `impact`는 측정의 기록이고, 단일 행 쓰기에서는 NULL이다. 그 뜻을 바꾸지 않는다.
- 누적 값을 알 수 없으면 `{"error": "..."}`만 남기고 6번을 건너뛴다(7장).

**세는 곳.** 게이트의 메모리에 (접속 계정, 테이블)별 항목 목록을 둔다. 항목은 판단 기록의 id, 시각, 행 수다.

- 시작한 뒤 어떤 쌍을 처음 볼 때 한 번, `stet.decisions`에서 창 안의 실행된 쓰기를 읽어 채운다(`login_role`, `target_table`, `executed`, `rows_affected`). 재시작해도 창 안의 사용량이 사라지지 않게 하기 위해서다. `rows_affected`가 NULL인 기록은 0으로 친다.
- 그 뒤로는 메모리만 본다. 쓰기마다 질의가 늘지 않는다.
- 창을 벗어난 항목은 볼 때마다 치운다.

## 5. 읽기 기록

**설정.** 테이블별로 켠다. 없으면 그 테이블을 읽는 문장은 지금처럼 보지 않는다.

```yaml
tables:
  contacts:
    # ...
    reads:
      log_above: 1000        # 선택. 한 문장이 이보다 많은 행을 내보내면 그 문장을 따로 남긴다
```

- 블록이 있으면 집계를 한다. 값 없이 `reads:`만 쓴 것도 있는 블록이다. `reads: ~`와, 블록의 줄이 모두 주석인 경우도 같다. `log_above`가 없으면 집계만 한다. `log_above`는 1 이상의 정수다.

**무엇이 읽기인가.** 분류기에 `read`를 더한다. 다음을 모두 만족하는 문장이다.

1. 게이트 대상 테이블에 쓰지 않는다. 쓰는 문장은 지금처럼 쓰기로 분류되고, 그 문장이 다른 게이트 대상 테이블을 읽는 것은 세지 않는다.
2. 문장 어딘가에서 게이트 대상 테이블을 읽는다. `SELECT`의 `FROM`(조인, 서브쿼리, `WITH` 포함), `COPY 테이블 TO`, `TABLE 테이블`, 그리고 다른 테이블에 쓰는 문장의 `FROM`·`USING`·`INSERT ... SELECT`가 여기에 든다.

- `EXPLAIN`과 `CREATE VIEW`는 행을 내보내지 않으므로 읽기가 아니다. `EXPLAIN ANALYZE`는 문장을 실제로 실행하므로, 게이트 대상 테이블을 읽고 그 행을 어딘가에 남기는 문장(`CREATE TABLE AS`, `SELECT INTO`, `INSERT ... SELECT` 등)이면 읽기다. 행을 버리는 `EXPLAIN ANALYZE SELECT`는 읽기가 아니다.
- `CREATE TABLE AS`, `SELECT INTO`, `CREATE MATERIALIZED VIEW`, `PREPARE`는 질의가 읽는 테이블을 읽는다. 나중에 나가는 SQL `EXECUTE`의 행은 세지 않는다.
- 분류 결과에 읽은 테이블의 설정 키 목록을 담는다. 한 문장이 여러 게이트 대상 테이블을 읽을 수 있다.
- 읽은 테이블 가운데 `reads`가 켜진 것이 하나도 없으면 게이트는 그 문장을 보지 않는다. 지금과 완전히 같다.
- `reads`가 켜진 테이블이 설정에 하나도 없으면 분류기는 읽기를 찾지 않는다. 게이트 대상 테이블을 읽는 문장도 이 변경 전처럼 `pass`다. 읽기를 찾으려면 트리를 한 번 더 걸어야 하는데, 그 비용은 섀도 모드에서 모든 세션의 모든 문장에 붙는다. 기록할 테이블이 없으면 그 비용을 치르지 않는다. 커서 문장은 이때도 알아본다. 최상위 문장의 종류만 보면 되기 때문이다.

**행 수.** 그 문장의 응답으로 클라이언트에게 나간 행 메시지의 수다. `SELECT`는 DataRow 메시지를, `COPY ... TO`는 CopyData 메시지를 센다. 메시지의 본문은 읽지 않는다.

- 문장이 실패하거나 취소돼도 그 전에 나간 행은 센다.
- 행 수 상한을 걸고 포털을 나눠 가져가면 Execute마다 나간 행을 센다. 문장 수도 Execute마다 하나씩 는다. `max_rows`와 `log_above`에서는 나눠 가져간 것 전체를 한 읽기로 본다(아래 "나눠 가져간 읽기").
- 한 메시지에 문장이 여럿이면 그 메시지의 응답으로 나간 행 전체를 센다.
- 조인으로 두 게이트 대상 테이블을 읽어 n행이 나가면 두 테이블 각각에 n행으로 센다.
- 트랜잭션 안의 읽기도 센다.
- 실행되지 못한 읽기도 문장 1건에 행 0으로 센다. 같은 Sync 아래에서 앞 문장이 실패해 건너뛴 읽기와, 서버에 더는 없는 커서의 `FETCH`가 그렇다. `statements`는 클라이언트가 보낸 문장의 수다.
- 행은 서버에서 메시지 머리를 읽을 때 센다. 그래서 연결이 끊기면 아직 클라이언트에게 쓰지 못한 행이 (버퍼 하나 분량까지) 들어갈 수 있다.

**커서.** SQL 커서는 읽기를 두 문장으로 나눈다. 테이블 이름은 `DECLARE`에 있고 행은 `FETCH`에서 나간다.

```sql
DECLARE c CURSOR FOR SELECT * FROM contacts;   -- contacts를 읽는다고 선언. 나가는 행은 없다
FETCH 1000 FROM c;                             -- 1,000행이 나간다. 테이블 이름이 없다
```

문장을 하나씩 따로 보면 `FETCH`를 `contacts`의 읽기로 셀 수 없다. 큰 결과를 받을 때 흔히 쓰는 방식이라(Python psycopg의 이름 붙은 커서) 이것을 놓치면 가장 큰 읽기가 기록에서 빠진다. 그래서 세션마다 커서 이름이 어느 테이블을 읽는지를 기억한다.

- `DECLARE 이름 CURSOR FOR <질의>`가 `reads`가 켜진 게이트 대상 테이블을 읽고 성공하면, 그 세션에 커서 이름과 읽는 테이블, 선언한 문장을 적어 둔다. `DECLARE` 자체는 읽기 한 건으로 세지 않는다. 나가는 행이 없다.
- `FETCH ... FROM 이름`이 오면 그 이름을 찾는다. 있으면 그 테이블들의 읽기로 세고 나간 행을 더한다. `FETCH`마다 문장 수가 하나 는다. `max_rows`와 `log_above`에서는 한 커서의 `FETCH` 전체를 한 읽기로 본다(아래 "나눠 가져간 읽기").
- 큰 읽기 기록(`stet.read_log`)의 `statement`에는 `FETCH`가 아니라 커서를 선언한 문장을 남긴다. 무엇을 읽었는지는 거기에 있다.
- 적어 둔 것은 `CLOSE 이름`, `CLOSE ALL`, `DISCARD ALL`, 세션이 끝날 때 지운다. `DISCARD ALL`은 `CLOSE ALL`로 본다(`stet.on_behalf_of`도 `RESET ALL`처럼 초기화한다). 트랜잭션이 끝나면 `WITH HOLD`로 선언하지 않은 커서를 지운다. 같은 이름으로 다시 선언하면 덮어쓴다.
- 실패했거나 건너뛴 `CLOSE`와 `CLOSE ALL`은 커서를 지우지 않는다. 실패한 `DECLARE`도 적어 두지 않고, 같은 이름의 앞선 커서를 그대로 둔다.
- 확장 프로토콜에서 한 Sync 아래의 단위가 실패하면, 실패한 Execute와 그 뒤의 Execute만 실행되지 않은 것으로 보고 취소한다. 그 앞의 Execute는 실행됐다. 그 안의 `DECLARE`는 트랜잭션이 끝날 때의 규칙을 그대로 따른다. 그래서 `begin; declare c cursor with hold ...; commit; select 1/0`을 한 Sync 아래에 보내면 `c`를 기억한다. 그 안의 `CLOSE`는 커서를 지운다. 롤백은 닫은 커서를 되살리지 않는다(서버에서 확인했다).
- 알 수 없을 때는 커서를 계속 기억하는 쪽으로 정한다. 서버가 이미 버린 커서를 기억하면 `FETCH`가 실패하고 문장 1건에 행 0으로 세어질 뿐이다. 서버가 아직 가진 커서를 잊으면 읽기가 가려진다.
- `MOVE`는 행을 내보내지 않으므로 세지 않는다.
- 이름은 파서가 정규화한 값으로 비교한다(따옴표 없는 이름은 소문자).

**문장이 여럿인 메시지 안의 커서.** 범용 "SQL 실행" 도구를 쓰는 에이전트는 `begin; declare c cursor for select * from contacts`를 한 Query로 보낸다. 그런 메시지의 커서 문장도 위와 같이 따라간다.

- 메시지 안의 커서 문장(`DECLARE`, `FETCH`, `CLOSE`, `CLOSE ALL`, `DISCARD ALL`)을 메시지가 도착할 때 순서대로 적용한다. 모두 그 메시지의 응답 하나에 묶인다.
- 메시지가 `reads`가 켜진 테이블을 읽으면 그 안의 `DECLARE`를 적어 두고, 선언한 문장으로 메시지 전체를 남긴다. 그래서 큰 읽기 기록의 `statement`는 메시지 전체다. 그런 테이블을 읽지 않는 메시지의 `DECLARE`는 단일 문장과 같이, 이미 적어 둔 이름이면 그 이름을 지운다.
- 메시지는 많아야 한 번 알린다. 메시지 자체가 `reads`가 켜진 테이블을 읽으면 그 메시지의 읽기로 알린다. 아니면 그 안의 `FETCH`가 적어 둔 커서를 읽을 때, 그 가운데 첫 커서를 선언한 문장의 읽기로 알린다. 행 수는 그 메시지의 응답으로 나간 행 전체다.
- `DECLARE`를 담은 메시지는 행을 내보냈을 때만 그 메시지의 읽기로 알린다. `begin; declare ...`는 행을 내보내지 않는다. 그 읽기는 뒤따르는 `FETCH`가 센다.
- 게이트 대상 테이블에 쓰는 메시지 안의 커서 문장도 같이 적용한다. 그 메시지는 쓰기로 판단되고, 그 안의 `FETCH`는 읽기로 센다.
- 이런 메시지가 실패하면 프록시는 그 안의 어느 문장이 실행됐는지, 그 사이에 커밋이 있었는지 알 수 없다. 그래서 기억하는 쪽으로 정한다. 이름을 지우는 것(`CLOSE`, `CLOSE ALL`, 테이블을 읽지 않는 `DECLARE`)은 취소해 그 이름의 앞선 뜻을 되살리고, 적어 둔 `DECLARE`는 남긴다. 문장이 하나인 메시지가 실패하면 지금처럼 그 문장을 취소한다.

**프록시가 하는 일.** 읽기는 쓰기의 경로를 타지 않는다.

- 쓰기는 보내기 전에 게이트를 기다린다(`Begin`). 읽기는 기다릴 것이 없다. 프록시는 읽기를 보내기 전에 게이트를 부르지 않고, 밀린 메시지를 먼저 내보내지 않으며, 바인딩 값을 복사해 두지 않는다.
- 응답이 끝나면(ReadyForQuery, 또는 연결이 끊길 때) 게이트에 한 번 알린다(`Observe`): 세션 정보, 문장, 나간 행 수, 실패 여부. 세션 정보의 `TxOpen`은 문장이 도착했을 때 명시적 트랜잭션이 열려 있었는지이고, 결과의 `Arrived`는 프록시가 문장을 클라이언트에게서 읽은 시각이다. 세션 정보의 접속 계정은 세션이 인증된 뒤에 도착한 문장에만 담는다(3장).
- 나눠 가져간 읽기의 한 부분이면 결과의 `Before`에 같은 커서나 포털에서 앞의 보고들이 이미 센 행 수를 담는다. 프록시는 그 합계를 커서의 기록(같은 이름의 새 `DECLARE`는 0부터)과 포털(새 Bind는 0부터)에 두고, 응답이 끝날 때 응답의 순서대로 더한다. 응답의 순서가 요청의 순서이기 때문이다. 행마다 하는 일은 늘지 않는다.
- 나간 행은 서버→클라이언트 경로에서 메시지의 종류만 보고 센다. 세션마다 정수 하나를 올리는 일이고, 잠금을 잡지 않는다.

**집계.** 게이트는 (분, 접속 계정, 테이블)별로 문장 수, 행 수, 한 읽기의 최대 행 수를 메모리에 든다. 1분마다, 그리고 종료할 때 `stet.reads`에 쓴다. 분은 문장이 프록시에 들어온 시각(`Arrived`)으로 정한다. 오래 걸린 읽기는 이미 쓴 분에 더해질 수 있고, 쓰는 쪽이 합친다.

```sql
CREATE TABLE IF NOT EXISTS stet.reads (
  bucket        timestamptz NOT NULL,          -- 분의 시작 시각
  login_role    text NOT NULL,                 -- 모르면 ''
  target_table  text NOT NULL,                 -- 설정의 테이블 키
  statements    bigint NOT NULL,
  rows          bigint NOT NULL,               -- 클라이언트에게 나간 행 수
  max_rows      bigint NOT NULL DEFAULT 0,     -- 한 문장이 내보낸 가장 큰 행 수
  PRIMARY KEY (bucket, login_role, target_table)
);
```

- `max_rows`는 한 읽기가 내보낸 가장 큰 행 수다. 나눠 가져간 읽기는 보고마다 그때까지의 합계로 올린다(아래 "나눠 가져간 읽기").
- 같은 키가 이미 있으면 더한다(`ON CONFLICT ... DO UPDATE`). 분 도중에 재시작해도 합쳐진다.
- 쓰기에 실패하면 메모리에 두고 다음 분에 다시 시도한다. 처음 쓰기에 실패한 때부터 한 시간이 지난 것은 버리고 로그를 남긴다(그 분이 끝난 때부터가 아니다).
- 게이트가 닫힌 뒤에 보고된 읽기는 쓰지 않고 세어서 로그에 남긴다. `stet proxy`는 종료할 때 세션이 끝나기를 기다린 뒤에 게이트를 닫는다. 그래서 종료로 끊긴 읽기도 쓰인다.
- 집계의 키에 `application_name`을 넣지 않는다. 클라이언트가 값을 바꿔 가며 키를 끝없이 늘릴 수 있다.

**큰 읽기.** `log_above`가 있고 한 읽기가 내보낸 행 수가 그보다 크면 `stet.read_log`에 한 줄을 남긴다. 판단 기록처럼 별도 고루틴에서 쓴다.

```sql
CREATE TABLE IF NOT EXISTS stet.read_log (
  id            uuid PRIMARY KEY,
  created_at    timestamptz NOT NULL,          -- 문장이 들어온 시각
  login_role    text,                          -- 모르면 NULL (stet.decisions와 같다)
  agent_id      text NOT NULL,                 -- application_name. 스스로 적는 값
  on_behalf_of  text,
  session_id    text,
  statement     text NOT NULL,
  statement_fp  text NOT NULL,
  target_tables text[] NOT NULL,               -- 읽은 게이트 대상 테이블의 설정 키
  rows          bigint NOT NULL,               -- 클라이언트에게 나간 행 수
  exec_error    text                           -- 실패하거나 끊긴 문장이면 그 원인
);
```

- 한 문장이 읽은 테이블이 여럿이면 그중 하나라도 `log_above`를 넘을 때 한 줄을 남긴다.
- 큰 읽기는 길이 1024의 대기열을 거쳐, 분 집계를 쓰는 것과 같은 백그라운드 고루틴이 쓴다. 대기열이 차면 그 줄은 버리고 버린 수를 로그에 남긴다. 집계에는 영향이 없다.
- 바인딩 값은 남기지 않는다. 바이너리 값을 텍스트로 바꾸려면 DB에 물어야 하고, 읽기 경로에 그 왕복을 넣지 않는다.

**나눠 가져간 읽기.** 한 커서의 `FETCH`들과, 한 번 바인딩한 포털을 행 수 상한을 걸고 여러 번 실행한 Execute들은 각각 한 읽기다. 커서로 5백만 행을 2천 행씩 받거나 JDBC의 `setFetchSize`로 나눠 받는 읽기에도, 한 번에 받은 읽기와 같은 기준을 적용하려는 것이다.

- 집계의 `statements`와 `rows`는 지금처럼 보고마다 더한다. `max_rows`는 그 보고까지의 합계(`Before` + 나간 행)로 올린다.
- `stet.read_log`의 줄은 커서나 포털 하나에 한 번만 남긴다. 합계가 처음으로 `log_above`를 넘는 보고에서 남긴다(`Before`가 `log_above` 이하이고 합계가 그보다 크다).
- 그 줄의 `rows`는 그때까지 읽은 행 수다. 끝까지 읽은 행 수가 아니다. 끝까지 읽은 수는 `stet.reads`에 있다. `created_at`은 기준을 넘긴 보고의 문장이 들어온 시각이고, `statement`는 커서를 선언한 문장이나 포털의 문장이다.
- 같은 이름으로 다시 선언한 커서와 다시 바인딩한 포털은 새 읽기다. 0부터 센다.
- 문장이 여럿인 메시지가 커서를 선언하고 그 메시지 자체의 읽기로 알려지면, 그 메시지가 내보낸 행이 그 커서의 첫 행으로 세어진다(`declare c ...; fetch 5 from c` 뒤의 `fetch`는 5부터 센다).
- 한 번에 읽은 문장은 지금과 같다. `Before`는 0이다.

## 6. 패키지별 변경

| 패키지 | 변경 |
|---|---|
| `internal/config` | `impact.cumulative`(`max_rows`, `window`)와 `reads`(`log_above`) 블록과 검증 |
| `internal/sqlparse` | 분류에 `read`를 더하고, 읽은 게이트 대상 테이블의 목록을 결과에 담는다. `reads`가 켜진 테이블이 없으면 읽기를 찾지 않는다. 메시지 안의 `DECLARE ... CURSOR`, `FETCH`, `CLOSE`, `DISCARD ALL`을 문장 순서대로 결과에 담고, 메시지의 문장 수를 담는다 |
| `internal/proxy` | 다섯을 고친다. (1) 게이트가 문장을 "읽기로만 본다"고 표시할 수 있게 하고, 그런 문장은 보내기 전에 게이트를 부르지 않으며 응답이 끝난 뒤 한 번 알린다. (2) 서버→클라이언트 경로에서 나간 행 메시지를 센다. (3) 쓰기의 결과에 "실패라는 답 없이 ReadyForQuery 전에 연결이 끊겼다"를 실패와 구분해 알린다. (4) 세션마다 커서 이름과 그 커서를 선언한 문장을 기억하고, `FETCH`를 그 문장의 읽기로 알린다. 문장이 여럿인 메시지 안의 커서 문장도 따라가고, 나눠 가져간 읽기에는 앞의 보고들이 센 행 수를 담는다. (5) 세션이 인증된 뒤에만 접속 계정을 넘긴다 |
| `internal/gate` | 누적 값 추적(잡아 두기, 확정, 처음 볼 때 채우기). 읽기 집계와 1분 주기의 쓰기, 큰 읽기 기록. `Inspect`가 `reads`가 켜진 테이블의 읽기만 표시하게 한다. 나눠 가져간 읽기를 `max_rows`와 `log_above`에서 한 읽기로 본다 |
| `internal/decide` | `Verdict`의 입력에 누적 값을 더하고 4장의 6번을 구현한다 |
| `internal/store` | `login_role`, `rows_affected`, `cumulative` 컬럼과 인덱스. `stet.reads`, `stet.read_log` 테이블과 쓰기 |
| `stet.example.yaml` | `cumulative`와 `reads` 예시 |

프록시의 `Gate` 인터페이스에 메서드가 하나 는다. 그래서 `internal/proxy`의 테스트용 게이트에도 그 메서드를 더해야 한다.

이 변경 전부터 있던 테스트의 단언은 둘만 일부러 고쳤다. `TestClassifyKinds`의 `select`와 `copy out`은 `Pass`에서 `Read`로 바뀌었고, `TestExampleConfigShowsImpact`의 `Impact` 비교는 구조체 전체 비교에서 필드별 비교로 바뀌었다(누적 한도가 그 구조체에 들어갔다). 분류기 테스트의 테이블 목록은 읽기를 기록하는 것으로 바꿨다. 단언이 아니라 입력이다. 이 변경 안에서 만든 테스트 가운데 뒤의 결정으로 단언이 바뀐 것은 셋이다. `TestReadOfTwoTablesCountsOnlyTheOneTurnedOn`의 `target_tables`는 읽은 게이트 대상 테이블 전부가 되었다. `TestCursorReadsAreCounted`의 `max_rows`는 커서의 합계 7이 되었다(나눠 가져간 읽기). 커서 문장을 결과에 하나만 담던 필드가 목록이 되면서, 분류기와 프록시 테스트의 그 필드는 새 모양으로 옮겼고, 문장이 여럿인 메시지의 기대값은 비어 있던 것에서 커서 문장의 목록으로 바뀌었다.

## 7. 실패 처리

섀도 모드이므로 어떤 실패에서도 원래 문장은 그대로 통과한다.

| 상황 | 동작 |
|---|---|
| 접속 계정을 모른다 | 누적 한도를 적용하지 않고 `cumulative.error`에 남긴다. 읽기는 `login_role = ''`로 집계한다 |
| 처음 볼 때 `stet.decisions`를 읽지 못했다 (실패, 500ms 예산 초과) | 그 쓰기는 누적 한도를 적용하지 않고 `cumulative.error`에 남긴다. 잡아 두는 것은 그대로 한다. 다음 쓰기에서 다시 읽는다 |
| `stet.reads`에 쓰지 못했다 | 메모리에 두고 다음 분에 다시 시도한다. 처음 실패한 때부터 한 시간이 지나면 버리고 로그를 남긴다 |
| `stet.read_log`에 쓰지 못했다 | 로그만 남긴다. 판단 기록과 같다 |
| 큰 읽기 대기열이 찼다 | 그 줄을 버리고 버린 수를 로그에 남긴다. 집계는 그대로다 |
| 게이트가 닫힌 뒤에 읽기가 보고됐다 | 쓰지 않고 센 수를 로그에 남긴다 |
| 프로세스가 죽었다 | 아직 쓰지 않은 집계(최대 1분)가 사라진다. 누적 값은 재시작 뒤 `stet.decisions`에서 다시 채운다 |

## 8. 지연과 부하

- 누적 한도: 쓰기마다 메모리 연산뿐이다. 쌍을 처음 볼 때만 질의 하나가 더해진다.
- 읽기: 보내기 전에 하는 일이 없다. 응답이 끝난 뒤 게이트가 메모리의 집계에 더한다. DB 왕복은 없다.
- 행 메시지를 세는 일은 게이트가 붙은 모든 세션의 모든 결과 행에 걸린다. 행마다 정수 하나를 올리는 것이어야 하고, 본문을 읽거나 복사하거나 잠금을 잡으면 안 된다. 큰 결과 집합을 받는 시간이 눈에 띄게 늘지 않는지 잰다.
- `reads`가 켜진 테이블의 읽기도, 게이트 대상이 아닌 문장도 2ms 예산(p95) 안이어야 한다. `internal/proxy/latency_test.go`를 그대로 통과해야 한다.
- 분류기는 지금도 모든 문장을 파싱한다. 읽기를 찾는 일은 같은 트리를 한 번 더 걷는 것이고, `reads`가 켜진 테이블이 있을 때만 걷는다.
- 나눠 가져간 읽기의 합계는 응답이 끝날 때 읽기마다 한 번 더한다. 행마다 하는 일은 늘지 않는다.
- 메모리: 누적 항목은 (접속 계정, 테이블)마다 창 안의 쓰기 수만큼이다. 집계는 (접속 계정, 테이블)마다 분당 하나다.

## 9. 한계

- 누적 값은 프로세스 하나의 것이다. Stet을 여러 대 띄우면 각자 따로 센다.
- 누적 값은 Stet이 본 쓰기만 센다. 함수, 트리거, Stet을 거치지 않은 연결의 쓰기는 들어가지 않는다.
- 다른 문장 안의 쓰기와 한 메시지에 문장이 여럿인 쓰기는 행 수를 모르므로 누적 값에 0으로 들어간다. 쓰기 뒤에 `; SELECT 1`을 붙이면 누적 값을 피한다. 이런 형태는 `unsupported`로 기록되고, enforce의 기본값은 거부다.
- 롤백된 쓰기도 누적 값에 남는다.
- 처음 볼 때 채우는 값은 `stet.decisions`에 이미 기록된 것뿐이다. 재시작 직전에 판단 중이던 쓰기는 빠질 수 있다.
- 접속 계정을 여러 개 가진 공격자는 계정마다 따로 한도를 받는다. 계정을 주는 것은 운영의 일이다.
- 커서 가운데 세지 못하는 것이 남는다. 함수 안에서 연 커서, Stet이 문장으로 보지 못한 커서, 그리고 프로토콜의 이름 붙은 포털을 `FETCH`로 읽는 경우다(SQL 커서와 포털은 이름 공간이 같다).
- 커서를 선언만 하고 읽지 않으면 기록에 아무것도 남지 않는다.
- 뷰, 함수, 파티션 자식 테이블을 통한 읽기는 이름이 달라 보이지 않는다. 쓰기에 이미 있는 한계다.
- 게이트 대상 테이블에 쓰는 문장이 다른 게이트 대상 테이블을 읽는 것은 읽기로 세지 않는다. `RETURNING`으로 나간 행도 세지 않는다.
- 한 메시지에 문장이 여럿이면 게이트 대상 테이블을 읽지 않는 문장의 행도 함께 세어진다.
- 바이너리 형식의 `COPY ... TO`는 머리와 꼬리 메시지가 있어 행 수가 조금 많게 세어진다.
- 큰 읽기 기록에는 바인딩 값이 없다.
- `WITH HOLD` 커서를 선언한 트랜잭션이 단순 `ROLLBACK`으로 끝났거나 `COMMIT`이 실패해도, 그 커서는 `CLOSE`, `CLOSE ALL`, `DISCARD ALL`, 같은 이름의 새 `DECLARE`, 세션의 끝까지 기억된다.
- 세이브포인트는 따라가지 않는다. `DECLARE` 앞에 잡은 세이브포인트로 `ROLLBACK TO` 하면 서버는 커서를 버리지만 프록시는 기억한다.
- `COMMIT AND CHAIN`으로 커밋한 `WITH HOLD` 아닌 커서는 세션이 다음에 유휴가 될 때까지 기억된다.
- 파이프라인으로 보내면 `FETCH`는 도착했을 때의 커서 표와 맞춘다.
- 확장 프로토콜에서 한 Sync 아래의 단위가 실패하면, 실패 앞에서 실행된 `WITH HOLD` `DECLARE`는 기억한다. 명시적 트랜잭션 밖에서 선언했다면 서버는 실패한 암묵적 트랜잭션과 함께 그 커서를 버렸으므로, 그 이름은 서버에 없는 커서로 남는다.
- 문장이 여럿인 메시지가 실패하면, 그 안의 `CLOSE`가 실행됐어도 커서를 기억하고, 그 안에서 적어 둔 `WITH HOLD` 커서는 그 트랜잭션이 롤백됐어도 기억한다(`close c; select 1/0`, `declare c cursor with hold ...; select 1/0`). 서버에 없는 커서를 기억하면 그 이름의 `FETCH`는 실패하고 문장 1건에 행 0으로 세어진다. 이런 이름은 `CLOSE`, `CLOSE ALL`, `DISCARD ALL`, 같은 이름의 새 `DECLARE`, 세션의 끝까지 남는다.
- 문장이 여럿인 메시지 안의 `DECLARE`는 메시지 전체를 선언한 문장으로 기억한다. 그래서 큰 읽기 기록의 `statement`는 메시지 전체다. 그 메시지가 `reads`가 켜진 테이블을 읽으면 그 안의 `DECLARE`는 모두 그 메시지가 읽는 테이블을 읽는 것으로 기억된다(`declare a cursor for select 1; select * from contacts`의 `a`).
- 한 메시지가 기억한 커서 여럿에서 `FETCH`하면 첫 커서의 읽기로 한 번만 센다. 다른 커서가 읽는 테이블에는 세지 않는다.
- `DECLARE`를 담은 메시지가 행을 내보내지 않으면 그 메시지는 읽기로 세지 않는다. 그 메시지 안에 행 0의 다른 읽기가 있어도 그렇다(`declare c ...; select * from contacts where false`).
- SQL `PREPARE` / `EXECUTE`로 읽은 행은 세지 않는다. `EXECUTE`에는 테이블 이름이 없다.
- `DO` 블록 안의 읽기와, `query_to_xml`, `table_to_xml`처럼 텍스트로 받은 질의를 실행하는 함수를 통한 읽기는 보지 않는다.
- 아직 서버가 답하지 않은 `BEGIN` 바로 뒤에 보낸 읽기의 `TxOpen`은 false다. 쓰기도 같다.
- 설정 키 둘이 스키마만 다르고 관계 이름이 같으면(`a.contacts`, `b.contacts`), 스키마 없이 쓴 이름이 둘 중 어느 것으로 잡히는지는 정해져 있지 않다. 쓰기에 이미 있던 한계이고, 이제 읽기에도 해당한다. 읽기는 다른 키의 집계로 들어갈 수 있다.
- `EXPLAIN ANALYZE SELECT`는 서버에서 질의를 실제로 실행한다. 그 안의 함수도 실행된다. 행이 클라이언트에게 나가지 않으므로 Stet은 읽기로 세지 않는다.
- 인증이 끝나기 전에 보낸 문장은 접속 계정 없이 기록된다. 비밀번호가 필요 없는 계정(trust)으로 시작 패킷 바로 뒤에 문장을 이어 보내면, 서버는 인증한 뒤에 그 문장을 실행하지만 프록시는 첫 ReadyForQuery보다 먼저 그 문장을 볼 수 있다. 그런 문장은 누적 한도를 적용받지 않고 계정 `''`로 집계된다.
- `stet.reads`와 `stet.read_log`에는 append-only 트리거가 없다. 규칙 3은 `stet.decisions`의 것이다. 감사 기록으로 쓰려면 enforce 전에 정해야 한다.

### enforce 전에 닫을 것

이 단계가 남겨 둔 것이다. 섀도 모드에서는 기록이 덜 정확해질 뿐이지만, 쓰기를 막기 시작하면 막아야 할 쓰기를 통과시키거나 기록을 믿을 수 없게 된다.

- 누적 값의 첫 적재가 500ms 안에 끝나지 않으면 그 계정·테이블 쌍의 쓰기마다 다시 시도하고, 그동안 한도는 적용되지 않는다(`internal/gate/usage.go`, `internal/store`의 `Usage`). enforce에서는 DB가 느려질 때 누적 한도가 꺼지고, 다시 시도가 쓰기마다 질의를 더해 DB를 더 느리게 한다. 방향: 초 단위로 묶어 읽고, 실패하면 간격을 둔다.
- 누적 값의 정리(prune)는 모든 쌍이 함께 쓰는 잠금 아래에서 슬라이스 전체를 옮긴다. 24시간 창과 쓰기가 많은 테이블에서 비용이 커지고, 그동안 다른 쌍의 쓰기도 기다린다. 쓰기를 멈춘 쌍의 항목은 메모리에 남는다. enforce에서는 이 기다림이 모든 쓰기의 지연이 된다.
- CommandComplete는 왔지만 ReadyForQuery 전에 연결이 끊긴 쓰기는 실행되지 않은 것으로 기록된다(`internal/proxy/session.go`의 `unanswered`). 누적 값에는 남지만, 판단 기록의 `executed`가 실제와 다를 수 있어 기록으로 다시 채점할 때 틀린다.
- 시작 패킷의 프로토콜 부 버전이 3.0, 3.2가 아니면 접속 계정을 읽지 못한다. Postgres는 3.1을 받아 주므로, 클라이언트가 누적 한도와 계정별 읽기 집계를 피할 수 있다. enforce에서는 한도를 피하는 길이 된다. 이 브랜치의 테스트가 "접속 계정을 모르는 세션"을 만드는 데 이 동작을 쓰고 있어, 고칠 때 테스트의 방법도 바꿔야 한다.
- 응답을 받지 못한, 관찰 대상이 아닌 재선언(re-DECLARE) 뒤에 파이프라인으로 온 `FETCH`는, 그 재선언이 실패해도 기록되지 않는다. 서버는 앞선 커서를 그대로 읽는다. 읽기를 가리는 길이 하나 남는다.
- `stet.read_log`에 `created_at`, `login_role` 인덱스가 없다. 읽기 기록으로 계정을 막거나 조사하려면 기록이 쌓인 뒤 질의가 느려진다.
- 프로세스 종료 순서(세션이 끝난 뒤 게이트를 닫는다)를 확인하는 자동 테스트가 없다. 손으로만 확인했다. 순서가 깨지면 종료할 때 끊긴 쓰기와 읽기가 기록되지 않는다.
- 게이트가 닫힌 뒤 도착한 읽기를 알리는 로그 줄은 100ms 타이머에서 나가므로, 그 전에 프로세스가 끝나면 남지 않는다. 기록되지 않은 읽기가 있었다는 흔적도 사라진다.
- 쿼리를 정의만 하는 DDL(`CREATE POLICY`, `CREATE RULE`, `BEGIN ATOMIC` 함수)이 행 0의 읽기로 분류된다. 읽기에 판정을 붙이면 이런 DDL이 읽기로 판정된다.

## 10. 수용 기준

- [ ] `cumulative`도 `reads`도 없는 테이블의 기록은 이 변경 전과 같다. 더해지는 것은 `login_role`과 `rows_affected` 두 컬럼의 값뿐이다. 기존 테스트의 단언을 고치지 않고 통과한다. (확인 못 함: 기존 단언 둘이 바뀌었다. `TestClassifyKinds`의 `select`와 `copy out`은 `Pass`에서 `Read`로, `TestExampleConfigShowsImpact`의 `Impact` 비교는 구조체 전체 비교에서 필드별 비교로 고쳤다. 나머지 기존 단언은 그대로이고, 새 컬럼이 NULL로 남는 것은 `TestInsertLeavesNewColumnsNull`이 본다)
- [x] 모든 기록에 `login_role`이 채워진다. `rows_affected`는 3장대로다: 0행에 걸린 쓰기는 0, 실행되지 않은 쓰기와 다른 문장 안의 쓰기와 문장이 여럿인 메시지의 쓰기는 NULL이다. (`TestEveryDecisionRecordsRoleAndRows`, `TestRowsAffectedIsNullWhenTheTagIsNotTheTablesCount`, `TestInsertRecordsLoginRoleAndRows`)
- [x] 한도 아래의 쓰기 여럿이 창 안에서 한도를 넘으면, 넘는 쓰기부터 `hold`와 `impact:cumulative`가 기록된다. 단일 행 쓰기와 다중 행 쓰기가 섞여도 같다. (`TestCumulativeHoldsOnceTheWindowIsFull`, `TestCumulativeCountsWritesItDoesNotJudge`, `TestVerdictCumulative`)
- [x] 창이 지나면 다시 한도 안으로 판정된다. (`TestCumulativeWindowSlides`)
- [x] 접속 계정이 다르면 따로 센다. 같은 계정이 다시 접속해도 이어서 센다. (`TestCumulativeIsPerLoginRole`)
- [x] 같은 계정이 동시에 여러 연결로 보낸 쓰기가 모두 한도 안으로 판정되지 않는다. (`TestCumulativeCountsConcurrentWrites`)
- [x] 재시작한 뒤에도 창 안의 사용량이 이어진다. (`TestCumulativeSurvivesARestart`, `TestCumulativeLoadsTheWindowOnceForConcurrentFirstWrites`)
- [x] 단일 행 쓰기가 누적 한도에 걸려도 섀도 모드에서는 질문과 답이 기록된다. (`TestCumulativeHoldsOnceTheWindowIsFull`)
- [x] 미지원이거나 트랜잭션 안인 쓰기는 verdict가 바뀌지 않고 누적 값에는 더해진다. (`TestCumulativeCountsWritesItDoesNotJudge`)
- [x] 서버가 실패했다고 답한 쓰기는 누적 값에 남지 않는다. 답이 오기 전에 연결이 끊긴 쓰기는 남는다. (`TestCumulativeForgetsFailedWrites`, `TestCumulativeKeepsALostWrite`)
- [x] `reads`가 켜진 테이블을 읽는 문장이 분류된다: 단순 `SELECT`, 조인, 서브쿼리, `WITH`, `COPY ... TO`, 다른 테이블에 쓰면서 읽는 문장. 게이트 대상 테이블에 쓰는 문장은 읽기로 분류되지 않는다. (`TestClassifyReads`, `TestWritesAreNotCountedAsReads`)
- [x] 읽기의 문장 수와 나간 행 수가 접속 계정과 테이블별로 `stet.reads`에 쌓인다. Simple Query와 Extended Query 모두에서 맞다. (`TestReadsAreAggregatedPerRoleAndTable`, `TestGateObservesReads`)
- [x] 행을 내보내다 실패하거나 취소된 읽기의 나간 행이 세어진다. 행 수 상한을 걸고 나눠 가져간 읽기의 행이 모두 세어진다. (`TestFailedReadCountsTheRowsItSent`, `TestGateObservesRowsOfACancelledRead`, `TestGateObservesPortalFetchedInParts`)
- [x] 커서로 읽은 행이 그 커서가 읽는 테이블의 읽기로 세어진다. `FETCH`마다 나간 행이 더해지고, `CLOSE`한 뒤나 트랜잭션이 끝난 뒤의 같은 이름은 세지 않는다. `WITH HOLD` 커서는 커밋 뒤에도 센다. 선언에 실패한 커서는 적어 두지 않는다. (`TestCursorReadsAreCounted`, `TestGateObservesFetchesAsReadsOfTheCursor`, `TestCursorIsForgottenWhenClosed`, `TestCursorIsForgottenAtTransactionEnd`, `TestHeldCursorSurvivesCommit`, `TestFailedDeclareIsNotRemembered`)
- [x] `log_above`를 넘는 읽기만 `stet.read_log`에 남는다. 커서로 읽은 경우 `statement`는 커서를 선언한 문장이다. (`TestLargeReadsAreLogged`, `TestCursorReadsAreCounted`)
- [x] `reads`가 꺼진 테이블을 읽는 문장은 게이트에 알려지지 않는다. (`TestReadsWithoutTheBlockAreNotSeen`)
- [x] 종료할 때 아직 쓰지 않은 집계가 쓰인다. (`TestCloseWritesWhatIsStillInMemory`)
- [x] 읽기가 클라이언트에게 돌려주는 결과는 직접 연결했을 때와 같다. 기존 프로토콜 테스트(큰 결과 집합, `COPY`, 파이프라인 포함)가 그대로 통과한다. (`TestObservedPassthrough`, `TestReadsAreAggregatedPerRoleAndTable`)
- [x] 인증이 끝나기 전에 보낸 쓰기와 읽기는 접속 계정 없이 게이트에 알려진다. 인증한 뒤에는 계정이 넘어간다. (`TestLoginRoleIsPassedOnlyOnceAuthenticated`)
- [x] 값 없이 쓴 `reads:`는 읽기를 기록하고, 값 없이 쓴 `cumulative:`와 초 단위로 떨어지지 않는 `window`는 설정 검증에서 거부한다. (`TestPresentButEmptyBlocks`, `TestCumulativeWindowIsWholeSeconds`)
- [x] `reads`가 켜진 테이블이 없으면 분류기는 읽기를 찾지 않는다. 커서 문장은 그래도 알아본다. (`TestClassifySkipsReadsWhenNoTableRecordsThem`, `TestInspectSkipsReadsWhenNoTableRecordsThem`, `BenchmarkClassify`)
- [x] 한 Sync 아래의 단위가 실패하면 실패 앞에서 실행된 커서 문장은 그대로 둔다: 커밋한 `WITH HOLD` 커서는 기억하고, 실행된 `CLOSE`는 커서를 지운다. (`TestFailedUnitKeepsWhatRanBeforeTheError`)
- [x] 문장이 여럿인 메시지 안의 커서 문장을 따라간다. 메시지는 많아야 한 번 세고, 실패하면 기억하는 쪽으로 정한다. (`TestCursorStatementsAmongSeveral`, `TestCursorsOfAQueryOfSeveralStatements`, `TestFailedQueryOfSeveralStatementsRemembers`)
- [x] 한 커서의 `FETCH`들과 한 포털의 Execute들은 `max_rows`와 `log_above`에서 한 읽기다. 큰 읽기 기록은 합계가 처음 기준을 넘을 때 한 번 남는다. (`TestReadInPartsIsLoggedOnce`, `TestReadsInPartsCarryTheRowsBefore`, `TestDeclaringMessageCountsTowardItsCursor`, `TestCursorReadsAreCounted`)
- [x] `STET_TEST_PG`를 설정한 `go test -race ./...`와 2ms 지연 예산을 통과한다. `reads`가 켜진 테이블의 읽기도 같은 예산 안이다. (`TestPassthroughLatencyBudget`, `TestObservedReadLatencyBudget`; 전체는 `STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...`가 통과한다)

## 11. 정한 것과 남긴 것

**이 문서에서 정한 것** (설계자의 판단. 사용자가 고른 것은 2장의 첫 표의 셋이다)

- 읽기의 행 수는 완료 태그가 아니라 클라이언트로 나간 행 메시지를 센다. 그래서 프록시를 고친다.
- 읽기는 보내기 전에 게이트를 부르지 않는다.
- SQL 커서는 세션별로 이름을 기억해 `FETCH`를 읽기로 센다 (사용자가 넣기로 정했다).
- `login_role`과 `rows_affected`를 모든 기록에 채운다. `rows_affected`는 태그가 그 테이블의 행 수일 때만 채운다.
- 누적 값은 `impact` 컬럼이 아니라 `cumulative` 컬럼에 둔다.
- 보내기 전에 잡아 두고 결과로 확정한다. 답 없이 끊긴 쓰기는 잡아 둔 채로 둔다.
- 누적 한도는 `impact` 아래에 두고, `cumulative.max_rows`는 `impact.max_rows` 이상이어야 한다.
- `window`에 기본값을 두지 않는다. 상한은 24시간이다.
- 트랜잭션 안의 쓰기와 미지원 쓰기도 누적 값에 더한다.
- 누적 값을 알 수 없으면 적용하지 않고 기록만 한다. enforce에서의 동작은 2번 단계에서 정한다.
- 읽기 집계의 키는 접속 계정과 테이블이다. `application_name`은 넣지 않는다.
- 큰 읽기 기록에 바인딩 값을 남기지 않는다.
- 읽기 기록은 테이블별로 켠다.

**만드는 사람에게 남긴 것**

- 내부 타입과 함수 이름, 파일 배치. 프록시의 새 인터페이스 메서드와 필드의 이름.
- 처음 볼 때 채우는 질의의 모양(항목을 그대로 읽을지 초 단위로 묶을지).
- 집계를 쓰는 고루틴의 구조.
- 테스트의 구성. 창이 지나는 것을 시험하는 방법(짧은 창, 또는 시계 주입).
