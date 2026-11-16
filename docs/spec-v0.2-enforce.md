# Stet v0.2 스펙 3: enforce 최소형

작성: 2026-11-16 · 상태: 구현됨 · 상위 문서: concept-v0.2.md 9장의 2번 단계, decisions.md D30·D31 · 앞 문서: spec-v0.1.md 7장·10장, spec-v0.2-impact.md, spec-v0.2-accounting.md

## 1. 목표와 비목표

**왜 하는가.** 지금까지 Stet은 판정을 적어 두기만 했다. 에이전트가 4만 행을 지우는 `DELETE`를 보내면 그대로 실행되고, 기록에 "enforce였다면 hold"라고 남는다. 이 단계에서 그 판정을 실제로 적용한다. 막을 수 있어야 통제 제품이고, 다음 단계인 hoop 대비 벤치마크도 막을 수 있어야 비교가 된다.

**목표**

- `mode: enforce`에서 에이전트 역할의 쓰기를 판정에 따라 보내거나 보내지 않는다. 보내지 않을 때는 에이전트가 읽고 행동을 고칠 수 있는 에러를 돌려준다.
- 판단할 수 없으면 막는다. 섀도 모드에서 일부러 열어 둔 지점을 닫는다.
- 드라이버가 `BEGIN`을 붙여 보내는 흔한 쓰기를 처음부터 판단한다.
- 섀도 모드의 판정이 enforce 모드의 판정과 같게 한다. 섀도 기록을 보면 enforce를 켰을 때 무엇이 막힐지 알 수 있어야 한다.
- 보류한 쓰기를 저장해 둔다. 다음 단계(2b)의 승인·거부 함수가 그것을 쓴다.

**비목표 (이 단계)**

- 보류 건의 승인과 거부(`stet.approve`, `stet.reject`). 2b에서 한다. 이 단계에서 hold와 escalate는 에이전트에게 "막힘"으로 보인다.
- 읽기를 막거나 한도를 거는 것. 읽기는 기록만 한다.
- 이미 무언가를 바꾼 트랜잭션 안의 쓰기를 판단하는 것. 막는다(4장).
- 여러 행을 건드리는 쓰기의 값 단위 판단. 영향 한도만 적용한다.
- 테이블별·역할별로 모드를 달리하는 것. 모드는 설정 파일 하나에 하나다.
- TLS, 고가용성, 여러 인스턴스(concept 9장의 6번 단계).
- 실험 도구(`internal/lab`) 변경.

## 2. 접근

논의에서 고른 것과 작성자가 정한 것이다. 작성자가 정한 것은 그렇게 적었다.

| 정할 것 | 고른 것 | 고르지 않은 것과 이유 |
|---|---|---|
| 트랜잭션 안의 쓰기 | 아직 아무것도 바꾸지 않은 트랜잭션의 첫 쓰기는 자동 커밋처럼 판단한다. 이미 바꾼 뒤의 쓰기는 막는다 (논의에서 고름) | 전부 막기: psycopg 기본 설정이나 ORM으로 붙는 에이전트는 아무것도 쓰지 못한다. `COMMIT` 때 모아서 판단하기: 프록시가 트랜잭션을 붙잡아야 해서 범위가 몇 배다 |
| 막는 방법 | 서버가 그 자리에서 에러를 내게 하고, 그 에러를 Stet의 에러로 바꿔 돌려준다 (작성자가 정함) | 프록시가 에러를 지어내 돌려주기: 서버는 에러를 본 적이 없어서, 같은 Sync 아래의 앞 문장이 커밋되고 트랜잭션 상태가 클라이언트와 어긋난다 |
| 동시 변경 확인 | 보내기 직전에 대상 행의 버전을 다시 읽어 비교한다 (작성자가 정함) | 문장에 `AND xmin = ...`을 붙이기: 확장 프로토콜에서는 문장이 이미 Parse로 서버에 가 있어 고칠 수 없다. 낡은 쓰기 탐지(5번 단계)에서 다시 본다 |
| 결정론적 판정 뒤의 모델 호출 | enforce에서는 부르지 않는다. 섀도에서는 누적 한도만으로 판정이 난 쓰기에 한해 지금처럼 실행 뒤에 부르고 답을 기록한다. 다른 한도로 다시 매길 수 있게 하려는 것이다 (작성자가 정함) | 항상 부르기: 이미 막기로 정해진 쓰기를 위해 에이전트가 모델 응답을 기다린다 |
| 보류 건 | `stet.held_writes`에 저장만 한다 (작성자가 정함) | 저장하지 않기: 2b가 생기기 전에 보류된 건을 승인할 수 없게 된다 |

## 3. 대상과 흐름

**대상 세션.** 설정의 `agents.roles`에 적힌 접속 계정의 세션만 본다. 다른 계정의 세션은 문장을 분류하지 않고 그대로 통과시키며, 기록도 남기지 않는다. enforce에서는 `agents.roles`가 반드시 있어야 한다(8장). 게이트 대상 테이블이 하나도 없으면 게이트를 만들지 않는다. 그때는 enforce에서도 모든 세션을 그대로 통과시키고, 시작할 때 경고 로그를 남긴다(7장).

**대상 문장.** 지금과 같다. 게이트 대상 테이블에 쓰는 문장이 대상이고, 읽기는 기록만 한다.

**enforce에서 쓰기 하나가 지나가는 순서.** 프록시는 쓰기를 서버로 보내기 전에 게이트를 기다린다(`Begin`). 섀도에서는 이때 상태만 읽고 판단은 실행 뒤에 한다. enforce에서는 판단까지 끝낸 뒤에 보낸다.

1. 프록시가 7장의 닫힌 지점을 먼저 막는다. 그런 메시지는 게이트의 판단에 오지 않는다.
2. 형태, 트랜잭션, 접속 계정을 본다. 판단할 수 없는 형태(영향 한도가 없는 테이블의 다중 행 쓰기 포함)면 테이블 설정 `unsupported`를 따른다. 이미 바꾼 트랜잭션 안이거나(4장) 접속 계정을 모르면(7장) 막는다. 여기서 정해지는 쓰기는 동시 상한의 자리를 잡지 않고, 상태를 읽거나 재지 않으며, 모델을 부르지 않는다. 막는 쓰기는 판단하는 동안 Stet의 DB에 가지 않는다. `unsupported: pass`로 보내는 쓰기는 바인딩 값을 읽고, 누적 한도가 있으면 0행으로 예약한다.
3. 동시 상한(`limits.max_inflight`)의 자리를 잡는다(10장).
4. 상태를 읽거나(단일 행) 영향을 잰다(다중 행). 섀도와 같은 코드다.
5. 누적 한도에 예약한다.
6. 결정론적 규칙을 먼저 적용한다(규칙 5): 측정 결과(실패, 영향 한도, 추정), 누적 한도, 누적 값 모름, 상태 실패. 물을 것이 없는 쓰기도 여기서 정해진다. 여기서 판정이 나면 모델을 부르지 않는다.
7. 남은 것은 단일 행 쓰기의 값 판단이다. 모델을 부른다. Stet은 이때 자기 트랜잭션을 열어 두지 않는다. 3의 자리는 쥐고 있다.
8. `commit`으로 판정된 단일 행 `UPDATE`·`DELETE`는 대상 행의 버전을 다시 읽는다. 바뀌었으면 4, 6, 7을 한 번 더 한다(5장).
9. 판정을 적용한다(5장): 보내거나, 막고 에러를 돌려준다. `hold`와 `escalate`는 막기 전에 `stet.held_writes`에 넣는다(9장). 여기서 3의 자리를 돌려준다.
10. 서버의 응답이 끝나면 결과와 함께 판단 기록을 남긴다(`End`). 막은 쓰기도 기록한다.

클라이언트가 기다리는 시간에는 단계마다 상한이 있다. 3부터 5까지(자리를 기다리는 시간, 바이너리 바인딩 값 읽기, 상태 읽기나 측정, 누적 값의 적재를 기다리는 시간)는 합쳐 500ms다. 7은 `model.timeout_ms`다. 어댑터가 시간을 지키지 않아도 그때 기다림을 끝낸다. 8의 행 버전 다시 읽기는 한 번에 500ms다. 다시 판단하면 상태 읽기(500ms), 모델(`model.timeout_ms`), 행 버전 다시 읽기(500ms)가 많아야 한 번 더 든다. 9의 보류 건 삽입은 500ms다. 2에서 `unsupported: pass`로 보내는 쓰기의 바인딩 값 읽기와 예약은 합쳐 500ms다. 상한을 넘은 단계는 그 단계의 실패로 다룬다(7장).

**섀도에서 달라지는 것.** 판정 규칙이 enforce와 같아진다. 4장의 트랜잭션 규칙, 7장의 "접속 계정 없음"과 "누적 값 모름"이 섀도의 판정에도 적용된다. 형태, 트랜잭션, 접속 계정, 측정, 상태 실패로 판정이 난 쓰기에는 섀도에서도 모델을 부르지 않는다. 누적 한도만으로 판정이 난 단일 행 쓰기에는 지금처럼 묻고 답을 기록한다. 모델이 고른 중복 후보가 Stet이 보낸 후보가 아니면 두 모드 모두 이유에 그 id를 적지 않는다(5장). 섀도는 여전히 아무것도 막지 않고, 모델은 실행 뒤에 부른다.

## 4. 트랜잭션 안의 쓰기

v0.1은 명시적 트랜잭션 안의 쓰기를 모두 판단하지 않았다(`in_transaction`). 이유는 셋이었다.

1. 트랜잭션이 앞에서 바꾼 행은 잠겨 있다. 그 상태로 모델을 기다리면 다른 연결도 기다린다.
2. Stet은 자기 연결로 상태를 읽는다. 그 트랜잭션이 앞에서 바꾼 값은 보이지 않는다.
3. 가운데 문장 하나만 막으면 앞 문장은 실행된 채 남는다.

셋 다 트랜잭션이 이미 무언가를 바꾼 뒤에만 성립한다. 드라이버가 `BEGIN`을 붙였을 뿐인 트랜잭션의 첫 쓰기는 자동 커밋으로 보낸 쓰기와 조건이 같다. 그래서 경계를 옮긴다.

**깨끗한 트랜잭션.** 세션이 마지막으로 트랜잭션 밖이었던 뒤로 지나간 문장이 `BEGIN`(`START TRANSACTION`), `SET`, `SHOW`뿐이면 그 트랜잭션은 깨끗하다. 빈 Query도 깨끗하다. `SET`에는 `SET LOCAL`, `SET TRANSACTION`, `SET ROLE`, `SET SESSION AUTHORIZATION`도 든다. `RESET`과 `SET CONSTRAINTS`는 깨끗하지 않다. 그 밖의 문장이 하나라도 지나갔으면 더럽다. `SELECT`도 더럽힌다. 함수가 무엇을 바꿨는지 프록시는 모른다. 분류기가 읽지 못한 문장, FunctionCall, 무엇을 실행하는지 모르는 포털의 Execute, 뜻이 둘 이상이고 그 가운데 깨끗하지 않은 뜻이 있는 이름의 Execute(6장)도 더럽힌다.

| 쓰기가 도착했을 때 | 처리 |
|---|---|
| 트랜잭션 밖 | 판단한다 (지금과 같다) |
| 깨끗한 트랜잭션 안 | 트랜잭션 밖과 똑같이 판단한다. 다중 행 쓰기의 영향 측정도 한다 |
| 더러운 트랜잭션 안 | 판단하지 않는다. 판정은 `unsupported`, 이유는 `in_transaction`이다(판단할 수 없는 형태면 형태의 이유가 먼저 기록된다). enforce에서는 형태의 이유가 먼저여도, 테이블 설정이 `unsupported: pass`여도 언제나 막는다(`ST002`). 자리를 잡지도, 상태를 읽거나 재지도 않는다. 두 모드 모두 모델을 부르지 않고 다중 행 쓰기를 재지 않는다 |

- 판단한 쓰기를 서버로 보내면 그 트랜잭션은 더러워진다. 그래서 한 트랜잭션에서 판단받는 쓰기는 하나다.
- 상태는 문장이 도착했을 때를 기준으로 본다. 프록시가 트랜잭션 상태를 아는 것은 서버의 ReadyForQuery에서뿐이다. 그래서 "마지막으로 트랜잭션 밖이었던 때"는 서버가 유휴라고 답한 마지막 ReadyForQuery다. 그 뒤로 클라이언트가 보낸 문장은 서버가 아직 답하지 않았어도 "지나간 문장"으로 친다. `BEGIN`과 쓰기를 응답을 기다리지 않고 이어 보내도 깨끗하다.
- 그 결과, 앞 문장의 응답을 기다리지 않고 이어 보낸 두 번째 쓰기는 더러운 트랜잭션 안의 쓰기로 본다. 앞 문장이 자동 커밋으로 끝났더라도 프록시는 아직 모른다. 막는 쪽으로 틀린다(13장).
- 확장 프로토콜에서 한 Sync 아래에 Execute가 여럿이면 서버는 그것들을 한 트랜잭션으로 묶는다. 같은 규칙을 적용한다. 쓰기 앞의 Execute가 모두 `BEGIN`, `SET`, `SHOW`면 판단한다(pgjdbc는 `BEGIN`과 쓰기를 한 Sync 아래에 보낸다). 그 뒤의 쓰기는 더러운 트랜잭션 안의 쓰기다.
- Sync 없이 Query나 FunctionCall로 끝난 확장 프로토콜 메시지는 그 Query와 한 암묵 트랜잭션에서 돈다(6장). 그 메시지들이 남긴 표시는 트랜잭션이 열린 채 끝난 단위처럼 이어진다.
- 한 Query 메시지에 문장이 여럿이면 지금처럼 `unsupported`(`multi_statement`)다. `begin; update ...; commit`을 한 줄로 보내는 것도 여기에 든다(13장).
- 트랜잭션 안에서 쓰기를 막으면 서버의 그 트랜잭션은 실패 상태가 된다(6장). 에이전트는 다른 DB 에러를 받았을 때처럼 `ROLLBACK`해야 한다.

**규칙 1의 문구.** CLAUDE.md의 규칙 1은 "모델 호출은 Postgres 트랜잭션 밖에서 한다"였다. 이 장의 규칙은 `BEGIN`만 된 클라이언트 트랜잭션이 열린 채로 모델을 부른다. 규칙의 목적은 지키지만 문구와 어긋났다. 구현을 시작하기 전에(계획의 Task 0) 다음으로 고쳤다.

> 1. **모델을 기다리는 동안 잠금이나 미확정 변경을 쥐지 않는다.** Stet은 모델을 부를 때 자기 트랜잭션을 열어 두지 않고, 이미 무언가를 바꾼 클라이언트 트랜잭션을 세워 두지 않는다. 판단이 끝난 뒤에만 실행한다.

## 5. 판정의 적용

판정 규칙의 순서는 spec-v0.2-accounting 4장 그대로다. 달라지는 것은 판정 뒤에 하는 일이다.

| 판정 | enforce에서 하는 일 | 에러 |
|---|---|---|
| `commit` | 서버로 보낸다. 상태를 읽은 단일 행 `UPDATE`·`DELETE`는 보내기 전에 행 버전을 다시 확인한다 | 없음 |
| `hold` | 보내지 않는다. `stet.held_writes`에 저장한다 | `ST001` |
| `escalate` | `hold`와 같다 | `ST001` |
| `unsupported` | 더러운 트랜잭션 안이거나 접속 계정을 모르면 보내지 않는다. 형태의 이유가 먼저 나왔어도 같다. 그 밖에는 테이블 설정 `unsupported`를 따른다(`reject`가 기본값이고, `pass`면 판단 없이 보낸다) | `ST002` |

판정 `reject`는 지금의 규칙에서 나오지 않는다. 판정 함수가 돌려주는 것은 위 넷이다. `ST003`은 내지 않는다. 판정을 행동으로 옮기는 표가 모르는 판정이 오면 `ST002`로 막는다.

**에러의 문장.** 막는 에러의 문장은 첫 이유로 고른다(6장).

- 문장에 필요한 값(행 수, 한도, 비율, 테이블)이 없으면 숫자를 지어 넣지 않는다. 보류한 쓰기는 "write held by stet: it needs review (<첫 이유>)"가 되고, `HINT`는 사람에게 승인을 받으라고 말한다. 판단할 수 없어 막은 쓰기는 "write refused by stet: this statement shape cannot be judged (<첫 이유>)"가 된다.
- 더러운 트랜잭션이나 접속 계정 때문에 막는 쓰기의 문장은 형태가 아니라 그 이유의 것이다. 형태를 고쳐도 지나가지 못하기 때문이다.
- 중복이면 후보의 키만 적는다(`possible duplicate of contacts.id=245`). 모델이 고른 후보가 Stet이 모델에게 보낸 후보 가운데 하나일 때만 그 키를 쓴다. 아니면 이유는 `duplicate:`이고 문장은 "possible duplicate of another row in <table>"이다. 모델의 답은 에이전트가 쓴 값에 영향을 받으므로 에러에 그대로 넣지 않는다(규칙 5). 모델의 원래 답은 판단 기록의 `answers`에 그대로 남는다. 두 모드 모두 같다.

**행 버전 다시 확인.** 상태를 읽은 때와 문장을 보내는 때 사이에 모델 호출이 있다. 그 사이 다른 쓰기가 같은 행을 바꿨을 수 있다.

- `commit`으로 판정된 단일 행 `UPDATE`·`DELETE`는, 그 상태를 읽었으면, 보내기 직전에 대상 행의 `xmin`을 Stet의 연결로 트랜잭션 밖에서 다시 읽는다. 상한은 500ms다. 상태를 읽을 때의 값과 같으면 보낸다.
- 다르면 상태를 다시 읽고 한 번 다시 판단한다. 모델이 필요하면 다시 묻는다. 처음에 없던 행이 생긴 것도 바뀐 것이다. 이유 끝에 `rejudged`를 더한다. 다시 판단한 결과가 `commit`이면 행 버전을 한 번 더 읽고, 또 달라져 있으면 `hold`로 바꾸고 이유를 `concurrent_change`, `rejudged`로 적는다.
- 행이 사라졌으면 그대로 보낸다. 0행에 걸릴 뿐이다.
- 행 버전을 다시 읽지 못하면 상태 실패(`state_error`)이고 `on_failure`를 따른다.
- 상태를 읽지 못해 `on_failure: pass`로 보내는 쓰기는 비교할 버전이 없어 다시 확인하지 않는다.
- 다시 읽은 때와 서버가 문장을 실행하는 때 사이의 틈은 남는다(13장).
- `INSERT`와 다중 행 쓰기에는 하지 않는다.

**다중 행 쓰기.** 영향 한도와 누적 한도만 적용한다. 한도 아래면 값 판단 없이 보낸다. 측정한 행 수와 실제로 바뀐 행 수(`rows_affected`)가 다를 수 있다. `commit`으로 판정된 쓰기의 실제 값이 `max_rows`를 넘었으면 판단 기록의 이유에 `impact:exceeded_after`를 더하고 경고 로그를 남긴다. 두 모드 모두 같다. 이미 실행된 쓰기는 되돌리지 않는다(되돌리기는 4번 단계다).

**`IN` 하나짜리 조건.** `WHERE id IN (5)`와 `WHERE id IN ($1)`은 한 행만 건드리지만 이 단계 전에는 다중 행으로 분류되어 값 판단을 받지 않았다. ORM이 흔히 만드는 모양이다. 분류기가 값이 하나뿐인 `IN`을 `=`로 읽어 단일 행 쓰기로 분류한다. 조인, `WITH`, 서브쿼리가 있는 문장에서는 `= 5`와 같이 `unsupported`다(13장). 그 밖의 모양(`id BETWEEN 5 AND 5`, `id = 5 OR false`)은 그대로 다중 행이다(13장).

## 6. 막는 방법과 에러

**원칙: 서버가 그 자리에서 에러를 본다.** 프록시가 문장을 버리고 에러를 지어내 돌려주면 서버는 아무 일도 없었다고 안다. 그러면 세 가지가 틀어진다.

- 확장 프로토콜에서 같은 Sync 아래의 앞 문장이 커밋된다. 진짜 에러였다면 함께 롤백됐을 것이다.
- ReadyForQuery의 트랜잭션 상태가 클라이언트가 기대하는 것과 달라진다.
- 파이프라인으로 보낸 앞 문장들의 응답 사이에 에러를 끼워 넣을 자리를 프록시가 직접 맞춰야 한다.

그래서 막을 문장을 **같은 종류의, 서버에서 반드시 실패하고 아무것도 바꾸지 않는 메시지로 바꿔 보낸다.** 서버는 그 자리에서 에러를 내고, 묶음을 롤백하고, 올바른 상태를 돌려준다. 프록시는 서버가 낸 그 에러 메시지 하나의 내용만 Stet의 에러로 바꿔 클라이언트에게 넘긴다.

- 바꿔 보내는 메시지는 사용자가 만든 객체로 성공하게 만들 수 없어야 한다. Query와 Parse는 문법 오류인 본문으로 바꾼다. Execute는 없는 포털 이름으로, Bind는 없는 문장 이름으로, FunctionCall은 OID 0으로 바꾼다. 테스트 서버(PostgreSQL 14)에서 다섯 가지 모두 그 자리에서 실패하고, 같은 Sync 아래의 앞 `INSERT`가 롤백되며, 트랜잭션 안에서는 상태가 실패로 바뀌는 것을 확인했다.
- "없는 이름"은 매번 새로 만든 무작위 값이고 63바이트를 넘지 않는다. 포털 이름은 앞 63바이트로 비교되고 SQL 커서와 이름 공간이 같다. 정해 둔 이름이나 긴 이름은 클라이언트가 같은 이름의 커서를 만들어 성공시킬 수 있다. 준비된 문장의 이름도 앞 63바이트로 비교된다. 그래서 프록시의 문장 표와 포털 표는 이름의 앞 63바이트를 키로 쓴다.
- 64MiB가 넘는 메시지는 검사하지 않는다. 섀도에서는 본문을 읽지 않고 흘려보낸다. enforce에서 막을 때는 본문을 읽어 버리고 바꾼 메시지를 보낸다.
- 바꿔 쓰는 에러는 바꿔 보낸 그 메시지가 낸 에러 하나뿐이다. 서버는 단위의 Parse, Bind, Describe, Close, Execute를 하나씩 완료로 답하고 첫 실패에서 멈춘다. 그래서 프록시는 단위 안의 메시지 순번과 서버가 그때까지 완료라고 답한 수로 에러가 어느 메시지의 것인지 가린다. 같은 Sync 아래의 앞 메시지가 먼저 실패하면 서버는 바꿔 보낸 메시지를 건너뛰고, 클라이언트는 앞 메시지의 에러만 받는다. 그 에러는 그대로 둔다. Sync에서 난 에러(커밋 때 실패한 지연 제약)는 어느 메시지의 것도 아니므로 바꾸지 않는다.
- 바꿔 쓰는 에러는 바꿔 보낸 메시지가 서버에서 받는 바로 그 에러여야 한다. 심각도가 ERROR이고, Query와 Parse는 42601이다. Execute는 34000, Bind는 26000이고 서버의 메시지에 바꿔 보낸 무작위 이름이 있어야 한다. FunctionCall은 42883이고, 실패한 트랜잭션 안에서는 25P02다. 맞지 않으면 바꾸지 않고 그대로 넘기며, 그 거부는 클라이언트에게 가지 않은 것으로 기록된다(9장의 `undelivered`). 프록시가 응답을 다른 메시지에 맞추는 일이 생겨도 다른 문장의 에러가 Stet의 에러로 바뀌지 않게 하는 장치다.
- 한 단위에서 바꿔 쓰는 에러는 많아야 하나다. 한 Sync 아래에서 두 번째로 막힌 메시지는 서버가 건너뛰므로 그 거부는 클라이언트에게 가지 않는다.
- 클라이언트가 받는 메시지의 수와 순서는 서버가 진짜 에러를 냈을 때와 같다. 에러 하나의 내용만 다르다. 바꿔 쓴 에러에는 심각도, SQLSTATE, 메시지, `DETAIL`, `HINT`만 있다.
- 원래 문장은 서버에 가지 않는다. 바인딩 값도 가지 않는다(Bind는 이미 갔을 수 있다. 실행되지 않을 뿐이다).
- 이 장치는 프록시의 두 방향에 각각 "메시지 하나를 바꿔 쓰기"를 더한다. 섀도 모드와 에이전트가 아닌 세션에서는 쓰이지 않는다.

**프록시의 문장 표는 서버가 확인한 것만 믿는다.** 확장 프로토콜에서 프록시는 이름 붙은 문장과 포털이 무슨 SQL인지 적어 둔다. 이 단계 전에는 Parse나 Close가 도착하면 서버의 답과 상관없이 표를 바꿨다. 그래서 표와 서버가 어긋날 수 있었다.

- `s1`을 `UPDATE`로 Parse한 뒤 `s1`을 `select 1`로 다시 Parse하면 서버는 거부하고(42P05) `UPDATE`를 그대로 둔다. 프록시는 `s1`을 읽기로 안다. 그 뒤 `s1`을 실행하면 판단받지 않은 `UPDATE`가 나간다. 테스트 서버에서 확인했다.
- 앞 메시지의 실패 때문에 서버가 건너뛴 Parse나 Close도 같다.

섀도에서는 기록이 틀릴 뿐이지만 enforce에서는 아무도 판단하지 않은 쓰기다. 그래서 표의 변경을 서버의 답에 맞춘다. 두 모드에 모두 적용한다.

- Parse, Bind, Close의 변경은 서버가 그 메시지를 완료했다고 답하면(ParseComplete, BindComplete, CloseComplete) 그때부터 남는다. 완료되지 않은 변경은 그 단위의 ReadyForQuery에서 버린다.
- 같은 단위 안에서는 앞 메시지의 변경을 곧바로 센다. 그 메시지가 실패하면 서버가 뒤의 것도 건너뛰기 때문이다.
- 단위가 다르면 그렇지 않다. 앞 단위의 Parse가 실패해도 뒤 단위의 Bind는 건너뛰어지지 않고 옛 문장에 묶인다. 그래서 아직 답이 오지 않은 앞 단위의 변경이 걸린 이름은 뜻이 둘 이상이다. 표는 그 이름이 가질 수 있는 뜻을 모두 들고 있다. 그런 이름으로 Bind한 포털도 그 뜻들을 모두 들고 있고, 앞 단위가 답을 받은 뒤에도 놓지 않는다. 같은 SQL과 같은 파라미터 타입으로 다시 Parse한 것은 같은 뜻으로 본다.
- enforce에서는 뜻이 둘 이상인 포털을 Execute할 때 그 뜻 가운데 감시 대상 쓰기나 무엇인지 모르는 것이 있으면 막는다(7장의 `unsettled_name`). 게이트는 한 가지 뜻만 판단할 수 있다. 섀도에서는 가장 새 뜻으로 판단하고, 깨끗하지 않은 뜻이 하나라도 있으면 트랜잭션을 더럽힌다.
- 프록시가 읽지 못한 Parse나 Bind가 바꿨을 수 있는 이름은 그 뒤로 무엇인지 모르는 뜻을 갖는다. 깨끗한 문장으로도, 감시 대상이 아닌 문장으로도 보지 않는다. enforce에서는 그런 메시지를 막는다.
- 포털 이름은 서버가 들고 있다고 확신하지 않는다. 서버는 트랜잭션이 끝나면 포털을 모두 버리는데, 아직 답이 오지 않은 `COMMIT`이 이미 그렇게 했을 수 있다. 표는 서버가 유휴라고 답할 때 포털을 모두 잊는다.
- enforce에서 막은 이름 없는 Parse 때문에 서버는 이름 없는 문장을 버리지만, 표는 옛 뜻을 둔다. 그 비용은 판단이 한 번 더 붙는 것이고, 판단받지 않은 쓰기는 생기지 않는다.

**Sync 전에 온 Query와 FunctionCall.** 확장 프로토콜 메시지 뒤에 Sync 없이 Query나 FunctionCall이 오면, 앞 메시지가 모두 성공했을 때 서버는 그 Query를 같은 암묵 트랜잭션에서 실행하고 ReadyForQuery 하나로 둘 다 답한다. 앞 메시지 가운데 하나가 실패하면 서버는 그 Query를 포함해 다음 Sync까지 모두 건너뛰고, 그 Sync의 ReadyForQuery가 그 사이를 모두 답한다. 프록시는 서버가 하는 대로 따라간다(두 모드 모두). 그러지 않으면 그 세션의 응답이 끝까지 한 단위씩 늦게 맞춰져 표가 서버와 달라지고, 거부의 에러가 다른 문장에 붙는다.

**에러 형식.** v0.1 7장 그대로다. LLM 에이전트는 메시지를 읽고 행동을 고친다. 아래는 마무리 확인에서 `psql`(`\set VERBOSITY verbose`)이 받은 것이다.

```
ERROR:  ST001: write held by stet: more than 3 rows would be updated, over the limit of 3
DETAIL:  decision=3febf11d-6a72-411d-8606-30097c2f7bc1 held_write=ec210d65-54bd-4d00-8d64-704a3e6cbc97 reasons=impact:rows
HINT:  narrow the WHERE clause, or ask a human to approve held_write ec210d65-54bd-4d00-8d64-704a3e6cbc97
```

| SQLSTATE | 의미 | 언제 |
|---|---|---|
| `ST001` | 검토 대기 | `hold`, `escalate` |
| `ST002` | 판단할 수 없음 | `unsupported`, 7장의 닫힌 지점, 세션을 끝낼 때(FATAL) |
| `ST003` | 정책에 따른 거부 | 내지 않는다 |

- 메시지는 영어다. 이유마다 한 문장과 `HINT`를 둔다. 문구의 표는 `internal/decide/act.go`에 있다. `ST002`의 `HINT`는 고칠 방법을 말한다(예: `in_transaction`의 "ROLLBACK, then send this write as its own statement or as the first statement after BEGIN").
- 판단 기록이 있는 거부는 `write held by stet: `나 `write refused by stet: `로 시작한다. 닫힌 지점은 막은 것의 이름으로 시작하고(`statement refused by stet: `, `message refused by stet: `, `function call refused by stet: `, `DO block refused by stet: `) `DETAIL`이 없다. 세션을 끝낼 때는 `connection closed by stet: `로 시작한다.
- 메시지에 다른 행의 값을 넣지 않는다. 중복 후보는 키만 적는다(5장). 에이전트가 볼 권한이 없는 값이 에러로 새면 안 된다. 에러에 넣는 값은 제어 문자를 공백으로 바꾸고 UTF-8이 아닌 바이트를 바꿔 넣는다.
- `DETAIL`은 `decision=<id> held_write=<id> reasons=<a,b>`이다. `decision`은 판단 기록의 id다. 보류하지 않았거나 저장하지 못한 쓰기에는 `held_write`가 없다.

## 7. 실패하면 막는다

섀도에서는 원래 동작을 바꾸지 않으려고 열어 둔 곳이 있다. enforce에서는 에이전트 세션에 한해 모두 닫는다. 판단 기록이 있는 줄의 "판정과 이유"는 섀도에서도 같은 값으로 기록된다.

| 지점 | 섀도 | enforce | 판정과 이유 |
|---|---|---|---|
| 분류기가 읽지 못한 Query·Parse | 감시하지 않고 보낸다 | 막는다 | 기록 없음, `ST002` (`unparsed`) |
| 읽을 수 없는 Bind·Execute | 무엇인지 모르는 것으로 두고 보낸다 | 막는다 | 기록 없음, `ST002` (`unparsed`) |
| 64MiB가 넘는 Query·Parse·Bind·Execute | 검사하지 않고 보낸다 | 막는다 | 기록 없음, `ST002` (`too_large`) |
| FunctionCall 메시지 | 검사하지 않고 보낸다 | 막는다 | 기록 없음, `ST002` (`function_call`) |
| `DO` 블록 | 보낸다 | 막는다 | 기록 없음, `ST002` (`do_block`) |
| 감시 대상 이름의 SQL `EXECUTE`·`FETCH`·`MOVE` | 보낸다. 기록도 없다 | 막는다 | 기록 없음, `ST002` (`prepared_write`) |
| 뜻이 정해지지 않은 이름의 Execute | 가장 새 뜻으로 판단한다 | 막는다 | 기록 없음, `ST002` (`unsettled_name`) |
| 분류기와 다르게 읽히는 세션 설정 | 그대로 분류한다 | 막는다 | 기록 없음, `ST002` (`session_setting`) |
| UTF8이 아닌 서버에서 ASCII가 아닌 문장·포털 이름 | 보낸다 | 막는다 | 기록 없음, `ST002` (`name_encoding`) |
| 판단할 수 없는 형태의 쓰기 | 보낸다 | 테이블 설정 `unsupported`. 기본값은 `reject` | `unsupported`, 형태의 이유 |
| 접속 계정을 모르는 세션의 쓰기 | 보낸다 | 막는다. `unsupported: pass`여도 막는다 | `unsupported`, `no_login_role` |
| 상태를 읽지 못함·시간 초과 | 보낸다 | `on_failure` (enforce 기본 `hold`) | `on_failure`에 따라 `hold`나 `commit`, `state_error` |
| 모델 실패·시간 초과 | 보낸다 | 위와 같다 | 위와 같다, `model_error` (모델이 없으면 `no_model`) |
| 영향 측정 실패 | 보낸다 | 테이블 설정 `unsupported` | `unsupported`, 형태의 이유 |
| 누적 값을 읽지 못함 | 보낸다 | 막는다 | `hold`, `impact:cumulative_unknown` |
| 동시에 판단 중인 쓰기가 상한을 넘음 | 보낸다 | 500ms 안에 자리가 나지 않으면 `on_failure` | `state_error` |
| `held_writes` 저장 실패 | 해당 없음 | 그래도 막는다. 에러의 `DETAIL`에 `held_write`가 없다 | 로그 |

- "기록 없음"인 줄은 판단 기록에 넣지 않는다. 경고 로그 한 줄("message refused without a decision")에 이유, 세션 id, 접속 계정, 데이터베이스, `application_name`을 남긴다. 문장은 남기지 않는다. 데이터가 들어 있을 수 있다.
- 한 Query에 게이트 대상 쓰기와 `DO` 블록이 함께 있으면(`insert into <대상> ...; do $$...$$`) 감시 대상(`unsupported`, `multi_statement`)이면서 닫힌 지점이다. enforce는 닫힌 지점으로 막고 로그만 남긴다. 섀도는 `unsupported` 판단 기록을 남긴다.
- 분류기는 PostgreSQL 17 문법을 읽는다. 서버가 더 새 버전이면 서버는 받는 문장을 분류기가 읽지 못할 수 있다. enforce에서 그런 문장은 막힌다. Stet은 시작할 때 서버 버전을 읽어 분류기보다 새면 경고한다(두 모드 모두).
- `on_failure: pass`와 `unsupported: pass`는 enforce에서도 쓸 수 있다. 운영자가 고른 것이다. 시작할 때 경고 로그를 남긴다.
- `DO` 블록은 안에서 무엇을 쓰는지 볼 수 없다. 에이전트 세션에서는 대상 테이블과 상관없이 막는다. 단, 게이트 대상 테이블이 하나도 없으면 게이트가 없어 이 표의 어느 것도 막지 않는다. enforce에서도 프록시는 통과만 하고, 시작할 때 그렇다고 경고한다.
- 누적 한도가 있는 테이블에서 그 계정의 사용량을 아직 읽지 않았으면, 자리를 얻지 못한 쓰기는 `state_error`가 아니라 `hold`(`impact:cumulative_unknown`)다. 규칙의 순서에서 누적 값 모름이 상태 실패보다 앞이다. `on_failure: pass`여도 막힌다. 자리를 얻지 못한 다중 행 쓰기도 측정 실패(`unsupported`)가 아니라 `state_error`이고, 테이블 설정 `unsupported`가 아니라 `on_failure`가 정한다.
- 보낸 그대로 저장할 수 없는 보류 건도 저장하지 않고 막는다. 바인딩 값을 읽지 못했거나, 문장이나 바인딩 값이 UTF-8이 아니거나 NUL이 있는 경우다. 바꾼 값으로 승인하면 다른 쓰기가 실행되기 때문이다. 저장하지 못한 보류 건의 `HINT`는 `this write`를 승인하라고 말하고, 에러 로그를 남긴다.
- 함수 안의 쓰기, 트리거, 뷰와 파티션 자식, DDL은 문장만 봐서는 닫을 수 없다. DB 권한으로 닫는다(8장).

**이름으로 실행하는 문장.** SQL `EXECUTE s1`은 프로토콜 Parse로 준비한 쓰기를 실행한다. 트랜잭션 안에서 SQL `FETCH`·`MOVE`는 프로토콜 Bind로 만든 `UPDATE ... RETURNING` 포털을 실행한다. 분류기에는 둘 다 테이블이 보이지 않는다.

- enforce에서는 문장이 이름으로 실행하는 준비된 문장과 포털을 세션의 표(6장)에서 찾는다. 그 이름이 가질 수 있는 뜻 가운데 하나라도 감시 대상 쓰기이거나 무엇인지 모르면 막는다(`prepared_write`). `EXPLAIN ANALYZE EXECUTE`처럼 다른 문장 안에 든 것도 같다.
- 그 뜻이 다시 이름을 실행하면 네 단계까지 따라가고, 그보다 깊으면 감시 대상으로 본다. 표에 없는 이름(SQL `PREPARE`로 만든 것)은 서버의 답에 맡긴다.
- 프로토콜 Execute도 같다. 포털이 실행할 수 있는 문장이 이름으로 감시 대상을 실행하면 막는다.

**세션 설정.** 서버는 문장의 바이트를 세션의 `client_encoding`에서 바꾼 뒤 읽고, `standard_conforming_strings`에 따라 문자열 안의 역슬래시를 다르게 읽는다. 분류기는 UTF-8과 `standard_conforming_strings = on`으로 읽는다. SJIS, BIG5, GBK, UHC, GB18030, JOHAB, SHIFT_JIS_2004에서는 여러 바이트 글자의 끝 바이트가 역슬래시일 수 있고, `standard_conforming_strings`가 `off`면 `\'`가 따옴표다. 그러면 서버와 분류기가 문자열이 끝나는 곳을 다르게 보고, 분류기가 문자열로 읽은 곳에 쓰기가 숨는다. 테스트 서버에서 두 경우 모두 그 쓰기가 판단 없이 실행되는 것을 확인했다.

- enforce의 에이전트 세션은 서버의 ParameterStatus로 두 설정을 따라간다. 서버는 시작할 때와 값이 바뀔 때마다 알린다. `client_encoding`이 `UTF8`(서버가 옛 JDBC를 위해 쓰는 표기 `UNICODE` 포함)이 아니거나 `standard_conforming_strings`가 `on`이 아니면 그 세션의 Query와 Parse를 모두 막는다. 서버가 처음 알리기 전(인증이 끝나기 전)에는 값을 모르므로 막는 쪽으로 본다.
- 서버는 이름과 바인딩 값도 `client_encoding`에서 바꾼다. 그래서 그동안은 ASCII가 아닌 문장·포털 이름을 쓰는 Bind와 Execute, 그리고 감시 대상 쓰기이거나 무엇인지 모르거나 이름을 실행하는 문장일 수 있는 Bind도 막는다. ASCII가 아닌 이름의 Close는 표를 바꾸지 않는다.
- 세션이 스스로 고칠 수 있게, 두 설정의 `SET`·`RESET`과 `SET NAMES`만으로 된 메시지는 지나간다. 이것은 분류기가 아니라 따로 읽는다. ASCII이고 역슬래시, 주석, 따옴표 이름, 특수 문자열이 없는 평범한 문장만 알아본다. 그런 글은 어느 `client_encoding`과 `standard_conforming_strings`에서나 똑같이 읽힌다. 서버가 안전한 값을 다시 알리면 세션은 정상으로 돌아온다.
- 섀도 세션과 감시하지 않는 세션은 설정을 보지 않는다. 섀도는 그런 세션의 문장을 지금처럼 분류하고, 틀리게 기록할 수 있다(13장).

**이름의 인코딩.** 서버의 `server_encoding`이 UTF8이 아니면, 서버는 ASCII가 아닌 이름을 자기 인코딩으로 바꿔 63바이트로 자른다. 프록시와 분류기는 UTF-8로 자르므로 이름을 다르게 비교할 수 있다. 그런 서버에서 enforce의 에이전트 세션은 ASCII가 아닌 이름을 쓰는 Parse, Bind, Execute와 그런 이름을 SQL `EXECUTE`·`FETCH`·`MOVE`로 실행하는 문장을 막는다(`name_encoding`). 보통 문장은 ASCII가 아닌 글이 있어도 지나간다.

**세션을 끝내는 경우.** 서버의 답이 프록시가 따라가는 단위와 어긋나는 순서가 있다. enforce의 에이전트 세션에서는 그때 세션을 끝낸다. 끝내게 한 메시지는 넘기지 않고, 서버 연결을 닫고, 클라이언트에게 `ST002`의 FATAL을 마지막으로 보낸다. 판단 기록은 없고 경고 로그 "session ended without a decision"이 남는다.

- 확장 프로토콜의 `COPY ... FROM STDIN`(`copy_extended`). 서버는 그 복사 중의 Sync를 무시한다. 서버가 그 복사를 시작한다고 답하는(CopyInResponse) 순간 끝낸다. 단순 Query로 보낸 `COPY ... FROM STDIN`은 된다.
- 단순 Query의 `COPY ... FROM STDIN` 중에 온 메시지(`protocol_order`). 서버의 CopyInResponse부터 클라이언트의 CopyDone이나 CopyFail까지 서버는 복사 메시지만 읽고 Sync는 무시한다. 그 사이에 Sync나 다른 메시지가 오면 끝낸다. Flush는 지나간다. COPY와 한 번에 보낸 Sync처럼 이미 보낸 메시지가 복사 중에 읽힐 때도 끝낸다. 한 Query에 복사가 여럿이어도 하나씩 센다. CopyDone과 다음 Query를 한 번에 보내는 클라이언트는 세션을 지킨다.
- 두 설정이 읽을 수 없는 값으로 바뀌었는데 그 변경 뒤에 읽혔을 수 있는 메시지가 이미 간 경우(`setting_changed`). 서버는 변경을 그 단위의 ReadyForQuery 바로 앞에서야 알린다. 그 전에 클라이언트가 보낸 다음 메시지는 옛 값으로 검사됐다. 뒤 단위에 Query나 Parse·Bind·Describe·Close·Execute가 있거나, 같은 단위에서 Execute 뒤에 메시지가 있거나, 실행한 문장이 있는 Sync 없는 단위 뒤의 Query면 끝낸다. Sync나 Flush만 있는 뒤 단위는 세지 않는다. 이미 간 그 메시지는 서버가 실행했을 수 있다(13장).

## 8. 에이전트 역할

```yaml
mode: enforce
agents:
  roles: [agent_enricher, agent_support]
```

- `agents.roles`는 접속 계정 이름의 목록이다. 대소문자까지 같아야 한다. 빈 이름, 63바이트가 넘는 이름, 두 번 적은 이름은 설정 오류다.
- `mode: enforce`에서 목록이 비어 있으면 설정 오류다. 섀도에서는 없어도 된다. 없으면 지금처럼 모든 세션을 본다. 있으면 섀도에서도 그 계정의 세션만 본다.
- 목록에 없는 계정의 세션은 그대로 통과한다. 분류도 기록도 하지 않는다.
- 접속 계정은 시작 패킷을 서버가 읽는 대로 읽는다. 프로토콜 3.x면 부 버전과 상관없이 읽는다. `user`가 여러 번 오면 서버처럼 마지막 것을 쓴다. 63바이트가 넘는 이름은 앞 63바이트를 쓴다. 테스트 서버(14)가 그렇게 자르고, 자르지 않는 서버에서는 그런 이름으로 로그인할 수 없으므로 어느 쪽에서나 맞다. `database`가 없으면 `user`와 같다.
- 서버가 세션을 시작하지 않을 시작 패킷(다른 주 버전, 깨진 키·값, `user` 없음)은 읽지 못한 것으로 보고 에이전트 세션으로 다룬다. 그 세션은 접속 계정이 없다.
- 인증이 끝나기 전(서버의 첫 ReadyForQuery 전)에 온 문장은 접속 계정이 없다. enforce에서는 대개 서버가 세션 설정을 알리기 전이어서 세션 설정의 닫힌 지점으로 먼저 막힌다. 그 뒤라도 접속 계정이 없어 막힌다(7장). 섀도에서는 `no_login_role`로 기록된다.
- Stet 자신의 접속 계정을 목록에 넣으면 시작하지 않는다. `upstream` URL에 계정이 있으면 설정 검증에서, 없으면 시작할 때의 확인에서 거부한다. 두 모드 모두 같다.
- 서버 설정 `db_user_namespace`(PostgreSQL 16까지 있고 기본은 `off`)가 켜져 있으면 서버는 시작 패킷의 계정 이름을 바꿔 인증한다(`agent@`는 `agent`로, `agent`는 `agent@<database>`로). 그러면 프록시가 읽은 계정과 서버가 인증한 계정이 달라 에이전트 세션이 감시를 벗어날 수 있다. enforce에서는 이 설정이 켜져 있으면 시작하지 않는다.

**시작할 때의 확인.** 게이트 대상 테이블이 있으면, Stet은 스키마를 올린 뒤 서버를 받기 전에 자기 계정으로, 읽기 전용 트랜잭션에서 아래를 확인한다. 합쳐 10초가 상한이다. 설정의 이름은 SQL에 파라미터로만 들어간다.

- 두 모드: 서버가 분류기의 문법(PostgreSQL 17)보다 새면 경고한다. Stet 자신의 계정이 `agents.roles`에 있으면 시작하지 않는다.
- enforce에서만: `db_user_namespace`가 켜져 있으면 시작하지 않는다. `model.on_failure: pass`와 테이블의 `unsupported: pass`는 경고한다. 에이전트 계정마다 다음을 경고한다. 계정이 없을 때. superuser이거나 `SET ROLE`로 될 수 있는 superuser 역할의 구성원일 때. `stet` 스키마에 권한(USAGE, CREATE)이 있을 때. 게이트 대상 테이블의 소유자일 때. 역할 수준의 `search_path`(이 데이터베이스용이나 전체용)가 Stet 연결의 것과 다를 때. 권한과 소유는 직접 받은 것, PUBLIC에 준 것, `SET ROLE`로 될 수 있는 역할을 거친 것(NOINHERIT 구성원 포함), 스키마나 테이블의 소유자 역할의 구성원인 것을 모두 센다. 경고는 그 계정과, 거둘 역할이나 고칠 문장을 말한다.
- 확인하지 못한 것은 통과가 아니다. enforce에서 그 결과가 시작을 멈추는 확인(`db_user_namespace`, Stet 자신의 계정)은 확인하지 못해도 시작하지 않는다. 그 밖의 확인과 섀도의 확인은 "확인하지 못했다"고 경고하고 시작한다.
- Stet 계정이 각 에이전트 계정의 구성원인지는 시작할 때 확인하지 않는다. 게이트가 그 계정으로 측정하지 못하면 계정마다 한 번 경고한다.
- 리스너는 스키마 이전과 확인보다 먼저 열린다. 그동안 붙는 클라이언트는 Stet이 서버를 받기 시작할 때까지 기다린다. 시작하지 않으면 리스너를 닫는다.

**운영자가 DB에서 해 둘 일.** Stet이 할 수 없는 것들이다. 위의 확인으로 볼 수 있는 것은 Stet이 경고한다. 나머지는 문서로 안내한다.

- 에이전트 계정은 Stet을 거쳐서만 DB에 닿아야 한다. `pg_hba.conf`에서 그 계정의 접속을 Stet의 주소로만 허용한다. 직접 붙을 수 있으면 Stet은 아무것도 막지 못한다.
- 에이전트 계정은 superuser도, superuser 역할의 구성원도 아니고, 게이트 대상 테이블의 소유자가 아니며, `stet` 스키마에 권한이 없다.
- 게이트 대상 테이블에 쓰는 함수의 `EXECUTE`, `dblink`·`postgres_fdw`, `TRIGGER`·`CREATE` 권한을 에이전트 계정에 주지 않는다(v0.1 스펙 10장의 표).
- Stet의 계정은 각 에이전트 계정의 구성원이어야 한다(`GRANT agent_x TO stet`). 영향 측정이 그 계정의 권한으로 돌기 때문이다.
- 에이전트 계정의 `search_path`는 Stet 계정의 것과 같아야 하고, 게이트 대상 테이블과 이름이 같은 테이블이 다른 스키마에 없어야 한다. Stet은 상태 읽기와 영향 측정에서 스키마 없는 이름을 자기 `search_path`로 푼다(13장). 시작할 때 에이전트 계정에 역할 수준의 `search_path` 설정이 있고 Stet의 것과 다르면 경고한다.
- `db_user_namespace`를 끈다(PostgreSQL 16까지).

## 9. 보류 건의 저장

`hold`와 `escalate`로 막은 쓰기를 `stet.held_writes`에 한 줄 남긴다. 2b의 승인 함수가 이 줄을 읽어 실행한다. 이 단계에서는 넣기만 한다.

```sql
CREATE TABLE IF NOT EXISTS stet.held_writes (
  id            uuid PRIMARY KEY,
  decision_id   uuid NOT NULL,                 -- stet.decisions의 id
  created_at    timestamptz NOT NULL,          -- 문장이 들어온 시각
  status        text NOT NULL DEFAULT 'pending',  -- 이 단계에서는 pending | undelivered
  login_role    text NOT NULL,
  database      text NOT NULL,
  statement     text NOT NULL,
  params        jsonb,                          -- 바인딩 값의 텍스트 배열
  param_types   oid[],                          -- 클라이언트가 Parse에서 준 타입
  target_table  text NOT NULL,
  target_pk     text,
  row_version   text,                           -- 상태를 읽을 때의 xmin. 단일 행 UPDATE·DELETE만
  verdict       text NOT NULL,                  -- hold | escalate
  reasons       text[] NOT NULL
);
CREATE INDEX IF NOT EXISTS held_writes_pending ON stet.held_writes (created_at) WHERE status = 'pending';
```

- 문장과 바인딩 값이 클라이언트가 보낸 그대로 들어간다. 텍스트 값은 그대로, 바이너리 값은 서버가 텍스트로 풀어 넣고, NULL은 NULL이다. `param_types`는 클라이언트가 Parse에서 준 그대로다(주지 않았으면 0). `stet.decisions`와 같은 정책이다.
- 보낸 그대로 저장할 수 없으면 넣지 않는다(7장).
- 같은 쓰기를 다시 보내면 보낼 때마다 한 줄씩 생긴다.
- 막으려던 쓰기의 에러가 클라이언트에게 가지 않을 수 있다. 에이전트는 자기 쓰기가 보류된 줄 모른다. 이때는 서버의 응답이 끝난 뒤 그 줄의 `status`를 `undelivered`로 바꾼다. 2b는 `pending`만 승인 대상으로 본다.
- 프록시는 바꿔 쓴 에러가 클라이언트에게 실제로 써졌는지 모른다. 그래서 바꿔 보낸 메시지의 에러를 Stet의 에러로 바꿔 썼고 그 단위의 ReadyForQuery까지 왔을 때만 전달된 것으로 본다. 다음은 모두 `undelivered`다. 같은 Sync 아래의 앞 메시지가 먼저 실패해 서버가 바꿔 보낸 메시지를 건너뛰었을 때. 서버의 에러가 바꿔 보낸 메시지가 받을 에러가 아니었을 때(6장). 서버의 에러가 FATAL이었을 때. 그 단위의 ReadyForQuery 전에 연결이 끝났을 때.
- 넣는 일은 에러를 돌려주기 전에 끝낸다. 에러의 `DETAIL`에 id가 들어가야 하기 때문이다. 시간 상한은 500ms다. 넘으면 7장의 마지막 줄이다. 시간을 넘겨 실패로 보고된 삽입도 서버에서는 들어갔을 수 있다. 그래서 그 id도 판단 기록을 남길 때 `undelivered`로 바꾼다. 들어가지 않았으면 바뀌는 줄이 없다(13장).
- `undelivered`로 바꾸는 것은 `pending`인 줄뿐이다.
- 누가 이 표를 읽고 고칠 수 있는지, 승인할 때 누구의 권한으로 실행하는지는 2b가 정한다. 에이전트 계정은 `stet` 스키마에 권한이 없어야 한다. 시작할 때 확인해 경고한다(8장).
- 프록시가 막은 쓰기를 실행됐다고 보고하면(프록시의 결함이다) 그 보류 줄은 `pending`으로 남는다. 2b가 그런 줄을 거둬야 한다(11장, 17장).

## 10. 설정

더해지는 것과 기본값이 바뀌는 것이다.

```yaml
mode: enforce                # shadow | enforce
agents:
  roles: [agent_enricher]    # enforce에서는 필수
model:
  timeout_ms: 800            # enforce에서는 에이전트가 모델을 기다리는 시간의 상한이다
  on_failure: hold           # enforce의 기본값. 섀도의 기본값은 pass 그대로
limits:
  max_inflight: 32           # 동시에 판단 중인 쓰기의 상한. 기본 32
```

- `mode: enforce`에서 `model.on_failure`를 적지 않으면 `hold`다.
- `mode: enforce`에서 `model.adapter: none`은 `model.on_failure: pass`를 직접 적었을 때만 받는다. 판단할 모델이 없으면 값 판단이 필요한 쓰기가 전부 실패 처리되므로, 영향 한도만 쓰겠다는 뜻을 적게 한다. `on_failure`를 적지 않았거나 `hold`면 설정 오류다. 받으면 시작할 때 경고한다.
- `limits.max_inflight`는 1 이상이어야 한다. 판단 중인 쓰기 하나가 자리 하나를 쥔다. 자리를 기다린 시간은 상태 읽기의 500ms 상한에 든다. 그 안에 자리가 나지 않으면 상태를 읽지도 재지도 않고 `state_error`로 `on_failure`를 따른다(7장).
- enforce에서는 자리를 잡은 뒤 `Begin`이 끝날 때까지 쥔다. 상태 읽기나 측정, 모델 호출, 행 버전 다시 읽기, 다시 판단, 보류 건 삽입이 모두 든다. 형태, 트랜잭션, 접속 계정만으로 판정이 나는 쓰기는 자리를 잡지 않는다.
- 섀도에서는 상태 읽기와 측정 동안만 쥐고 `Begin`이 끝날 때 돌려준다. 실행 뒤의 모델 호출은 이 수에 들지 않고, 따로 한 번에 8개까지로 묶인다. 섀도가 이 상한 때문에 느려지지 않게 하려는 것이다.
- v0.1 스펙은 enforce에서 `application_name`을 필수라고 했다. 이제 신원은 접속 계정이다(D31). `application_name`은 기록에만 쓰고 필수가 아니다.

## 11. 기록

- `stet.decisions.mode`에 `enforce`가 들어간다.
- 컬럼 하나를 더한다: `refused text`. Stet이 문장을 보내지 않고 돌려준 SQLSTATE다(`ST001`, `ST002`). 보냈으면 NULL이다. 섀도에서는 언제나 NULL이다. 다른 컬럼처럼 append-only다.
- 막은 쓰기는 `executed = false`, `rows_affected = NULL`이고, `exec_error`에는 돌려준 에러의 첫 줄이 `SQLSTATE ST001: write held by stet: ...` 꼴로 들어간다. 에러가 클라이언트에게 가지 않았어도 같다.
- 형태, 트랜잭션, 접속 계정만으로 막은 쓰기(3장의 2)는 상태가 NULL이다. 바인딩 값은 모두 텍스트일 때만 기록한다. 바이너리 값을 풀려면 Stet의 DB에 다녀와야 하는데, 그동안 이미 무언가를 바꾼 트랜잭션이 기다리게 되기 때문이다.
- 닫힌 지점과 세션을 끝낸 경우는 판단 기록이 없다(7장).
- `latency_ms`는 지금처럼 클라이언트가 `Begin`에서 기다린 시간이다. enforce에서는 상태 읽기, 모델, 행 버전 다시 읽기, 다시 판단, 보류 건 삽입이 모두 든다.
- 행 버전을 다시 확인해 다시 판단했으면 마지막 판단을 기록한다. 상태, 질문, 답, 모델 지연은 마지막 판단의 것이고, `cost_usd`는 모든 모델 호출의 합이다. 이유 끝에 `rejudged`를 더한다.
- `impact:exceeded_after`는 두 모드 모두 붙는다(5장).
- 누적 한도의 정산은 지금 표 그대로다. 막은 쓰기는 "서버가 거부"와 같이 예약에서 뺀다. 연결이 끊겼어도 같다. 그래서 막힌 쓰기를 곧바로 다시 보내도 그 계정의 한도를 쓰지 않는다. `unsupported: pass`로 판단 없이 보낸 쓰기도 예약하고 정산한다. CommandComplete를 받은 뒤 연결이 끊긴 쓰기는 그 행 수로 정산한다(12장).
- 프록시가 막은 쓰기를 실행됐거나 결과를 모른 채 끊겼다고 보고하면(프록시의 결함이다) 게이트는 에러 로그("refused write reported as sent")를 남기고, 프록시가 보고한 대로 기록하고 정산한다. `refused`는 그대로 남는다.
- 판단 기록은 지금처럼 실행 뒤에 비동기로 넣는다. 기록이 실패해도 이미 보낸 쓰기는 되돌리지 않는다. 로그를 남긴다.
- 재현할 때 다시 추론하지 않는다(규칙 4). 기록에 모델의 답이 그대로 있다.

## 12. 앞 단계가 남긴 것

앞의 세 스펙이 "enforce 전에"라고 적어 둔 것들이다.

**이 단계에서 닫는다**

| 출처 | 항목 | 어떻게 |
|---|---|---|
| v0.1 10장 | 파싱 실패, 큰 메시지, FunctionCall, `DO` 블록, 모델·상태 실패 | 7장 |
| impact 10장 | `IN` 하나짜리로 값 판단을 피함 | 5장. 조인, `WITH`, 서브쿼리가 있는 문장에서는 `= 5`처럼 `unsupported`다 |
| impact 10장 | `테이블.함수` 표기의 조건식을 Stet이 실행함 | 세기 전에, 조건식에서 테이블로 한정된 이름이 모두 그 테이블의 열인지 측정의 읽기 전용 트랜잭션에서 클라이언트의 역할로 카탈로그를 읽어 확인한다. 아니거나 확인하지 못하면 세지 않고 추정으로 간다 |
| impact 10장 | 동시에 도는 측정에 상한이 없음 | `limits.max_inflight`(10장) |
| accounting 9장 | 누적 값의 첫 적재가 느리면 한도가 꺼짐 | 그 계정과 테이블의 창을 초 단위로 묶어 한 번에 읽는다. 게이트가 시작하기 전에 기록된 것만 읽고, 그 범위가 비면 읽지 않는다. 적재는 자기 500ms 상한으로 따로 돌고, 적재를 시작한 쓰기를 포함해 기다리는 쓰기는 각자의 상한까지만 기다린다. 실패하면 그 적재를 시작한 때부터 5초 동안 다시 읽지 않고, 그동안은 7장의 "누적 값을 읽지 못함"이다 |
| accounting 9장 | 누적 값 정리가 공용 잠금 아래에서 느림 | 사용량을 초 단위 칸으로 바꾼다. 칸은 그 초 전체가 창 밖으로 나가야 빠진다(최대 1초 더 센다). 쓰지 않는 쌍은 1분에 많아야 한 번 지운다. 적재한 칸은 한 번에 합친다 |
| accounting 9장 | 프로토콜 3.1 시작 패킷의 계정을 못 읽음 | 8장. 3.x면 부 버전과 상관없이 서버가 읽는 대로 읽는다 |
| accounting 9장 | 인증 전 문장이 한도를 피함 | 7장, 8장. enforce에서는 막고, 섀도에서는 `no_login_role`로 기록한다 |
| accounting 9장 | CommandComplete 뒤 ReadyForQuery 전에 끊긴 쓰기가 "실행 안 됨"으로 기록됨 | CommandComplete를 받은 쓰기는 `executed = true`와 그 행 수로 기록하고, 누적 한도도 그 행 수로 정산한다. 문장이 여럿인 Query는 어느 문장의 완료인지 몰라 그대로 둔다 |
| 이 스펙의 검토 | 프록시의 문장 표가 서버와 어긋나 판단받지 않은 쓰기가 나감 | 6장. 서버가 완료한 변경만 남기고, 뜻이 둘 이상인 이름은 그 뜻을 모두 들고 있으며, Sync 전의 Query를 서버대로 따라간다. 표를 피해 가는 길(SQL `EXECUTE`·`FETCH`·`MOVE`, 뜻이 정해지지 않은 이름, 확장 프로토콜 복사, 복사 중의 Sync)은 enforce에서 막거나 세션을 끝낸다(7장). 섀도에 남은 어긋남은 13장 |
| accounting 9장 | 종료 순서의 자동 테스트가 없음 | `stet proxy`의 실행부를 떼어 내고, 끝날 때 아직 돌던 쓰기와 읽기가 기록되는지 테스트한다 |

**남긴다**

| 항목 | 이유 |
|---|---|
| 커서 표의 남은 틈 둘, 게이트가 닫힌 뒤의 읽기 로그, 정의만 하는 DDL이 읽기로 분류됨, `stet.read_log` 인덱스 | 읽기는 이 단계에서 막지 않는다. 읽기에 판정을 붙일 때 닫는다 |
| `stet.reads`·`stet.read_log`의 append-only | 위와 같다 |
| 상태 크기 상한(v0.1 4장) | 상태가 너무 크면 모델이 실패하고 `on_failure: hold`로 막힌다. 실패하는 쪽이 닫힌 쪽이다 |

## 13. 한계

- 이미 무언가를 바꾼 트랜잭션 안의 쓰기는 모두 막힌다. 한 트랜잭션에서 게이트 대상 테이블에 두 번 쓰는 에이전트는 두 번째에서 막히고, 트랜잭션 전체가 롤백된다. 쓰기 앞에 `SELECT`를 둔 트랜잭션도 마찬가지다.
- 한 Query에 문장을 여럿 담아 보내는 쓰기는 막힌다(`begin; update ...; commit` 한 줄 포함).
- 앞 문장의 응답을 기다리지 않고 이어 보낸 쓰기는 막힌다. 프록시는 앞 문장이 끝났는지 모르기 때문이다. 흔한 드라이버는 응답을 받고 다음 문장을 보낸다. 파이프라인 모드를 켜고 Sync를 따로 붙여 보내는 클라이언트가 해당한다.
- 판단을 기다리는 동안 클라이언트가 취소를 보내거나 연결을 끊어도, `commit`으로 판정된 쓰기는 서버로 간다. Stet 없이 보냈을 때도 이미 보낸 문장은 실행된다. enforce는 그 사이의 시간을 늘릴 뿐이다.
- 상태 읽기와 영향 측정은 스키마 없는 테이블 이름을 Stet 연결의 `search_path`로 푼다. 에이전트 세션의 `search_path`가 다르고 같은 이름의 테이블이 다른 스키마에 있으면, 에이전트가 쓰는 테이블과 Stet이 재는 테이블이 달라진다. 섀도에서는 틀린 기록이고, enforce에서는 영향 한도를 피하는 길이다. 운영으로 막는다(8장). 세션의 `search_path`를 따라가는 것은 남긴다.
- 세션에서 `SET ROLE`이나 `SET SESSION AUTHORIZATION`으로 바꾼 역할은 따라가지 않는다. 판단, 영향 측정, 누적 한도는 접속 계정으로 한다. 두 문장은 트랜잭션을 더럽히지도 않는다(4장).
- hold와 escalate는 이 단계에서 reject와 같아 보인다. 승인할 방법이 2b에 온다.
- 행 버전 확인과 서버의 실행 사이에 틈이 있다. 그 사이의 동시 변경은 잡지 못한다. `INSERT`의 중복도 같다. 두 에이전트가 같은 연락처를 동시에 넣으면 둘 다 통과할 수 있다.
- 다중 행 쓰기의 측정과 실행 사이에도 틈이 있다. 측정 뒤에 조건에 맞는 행이 늘면 한도를 넘는 쓰기가 실행된다. 기록에 남을 뿐이다.
- 다중 행 쓰기는 값 판단을 받지 않는다. 한 행짜리를 다중 행 모양으로 쓰면(`id BETWEEN 5 AND 5`) 값 판단을 피한다. 영향 한도와 누적 한도는 적용된다.
- 조인, `WITH`, 서브쿼리가 있는 문장에서 키에 값 하나짜리 `IN`을 쓴 쓰기는 `= 5`처럼 `unsupported`다. 이 단계 전에는 다중 행으로 재었지만 이제 재지 않는다.
- 에이전트는 모델 응답을 기다린다. `model.timeout_ms`만큼 쓰기가 느려질 수 있다. 행이 바뀌어 다시 판단하면 한 번 더 기다린다.
- 시간을 지키지 않는 모델 어댑터의 호출은 `model.timeout_ms`가 지나면 기다리지 않고 버린다. 그 호출은 혼자 끝날 때까지 돌고, Stet이 끝날 때도 기다리지 않는다. Stet에 든 어댑터는 시간을 지킨다.
- 섀도의 판정은 enforce와 같은 규칙이지만, 누적 한도가 있으면 갈라질 수 있다. 섀도는 enforce라면 막았을 쓰기도 실행하고 그 행을 누적 값에 센다. 그래서 그 뒤의 쓰기가 섀도에서 먼저 한도에 걸릴 수 있다.
- 세션 설정의 경주. `client_encoding`이나 `standard_conforming_strings`를 바꾸고 서버의 답을 기다리지 않고 다음 메시지를 보내는 클라이언트는, 그 메시지를 옛 값으로 검사받는다. Stet은 그 변경을 알게 되면 세션을 끝내지만, 서버는 그 메시지를 이미 실행했을 수 있다. 일부러 그렇게 하는 클라이언트는 enforce에서도 판단받지 않은 메시지 하나를 보낼 수 있다(D30). 막으려면 프록시가 이어 보낸 메시지를 서버의 답까지 붙잡아 두어야 한다.
- 실행한 문장이 있는 확장 프로토콜 단위 뒤에 Sync 없이 이어 보낸 Query가 마지막 문장으로 두 설정을 읽을 수 없는 값으로 바꾸면, 뒤에 아무것도 보내지 않았어도 세션이 끝날 수 있다.
- `client_encoding`이 `UTF8`이 아니거나 `standard_conforming_strings`가 `off`인 클라이언트는 enforce를 쓸 수 없다. SJIS 계열만이 아니라 UTF8이 아닌 모든 인코딩이다. 정직한 클라이언트도 그 세션의 문장이 모두 막힌다. 섀도에서는 그런 세션의 문장이 틀리게 분류되고 기록될 수 있다.
- 확장 프로토콜로 `COPY ... FROM STDIN`을 보내는 클라이언트(tokio-postgres의 `copy_in`이 그렇다고 알려져 있다)는 enforce에서 세션이 끝난다.
- 섀도에서는 단순 Query의 복사 중에 온 Sync나 확장 프로토콜의 `COPY ... FROM STDIN` 뒤로 그 세션의 응답이 한 단위 늦게 맞춰진다. 그 뒤의 기록과 표가 틀린다. enforce는 그런 세션을 끝낸다.
- 섀도에서는 감시 대상 이름을 SQL `EXECUTE`·`FETCH`·`MOVE`로 실행한 쓰기가 판단도 기록도 받지 않는다.
- 아직 답이 오지 않은 변경 때문에 뜻이 둘 이상인 이름으로 Bind한 포털은, 그 변경에 답이 온 뒤에도 그 뜻들을 모두 들고 있다. 그 가운데 감시 대상이 있으면 enforce에서 그 포털은 끝까지 막힌다.
- 보류 건 삽입의 틈. 시간을 넘겨 실패로 보고된 삽입도 서버에서 들어갈 수 있다. Stet은 그 id를 판단 기록을 남길 때 `undelivered`로 바꾸지만, 그때 아직 진행 중이던 삽입은 그 뒤에 `pending`으로 남을 수 있다. 클라이언트는 보류된 것이 없다고 들었다.
- 프록시가 막은 보류 건을 실행됐다고 보고하면 그 보류 줄은 `pending`으로 남는다. 2b가 거두지 않으면 승인할 때 같은 쓰기가 두 번 실행된다.
- 형태, 트랜잭션, 접속 계정만으로 막은 쓰기의 바이너리 바인딩 값은 기록되지 않는다(11장).
- 서버의 `db_user_namespace`가 켜져 있으면 enforce를 쓸 수 없다. Stet이 시작하지 않는다.
- 함수, 트리거, 뷰, 파티션 자식, DDL, 다른 연결을 통한 쓰기는 보지 못한다. DB 권한이 막아야 한다(8장).
- 에이전트 계정이 Stet을 거치지 않고 DB에 붙을 수 있으면 아무것도 막지 못한다. Stet은 이것을 확인할 수 없다.
- 에러 메시지는 에이전트에게 왜 막혔는지 알려 준다. 한도의 값과 판단의 이유가 드러난다. 실수하는 에이전트에게는 도움이고, 공격자에게는 정보다(D30).
- Stet이 죽으면 에이전트는 DB에 붙지 못한다. 고가용성은 6번 단계다.
- 판단 기록의 삽입이 실패해도 쓰기는 이미 갔다. enforce에서도 기록 없는 쓰기가 생길 수 있다.
- 인증 전 문장을 다루는 통합 테스트(`earlyQuery`를 쓰는 것)는 프록시가 서버의 첫 ReadyForQuery보다 클라이언트의 첫 Query를 먼저 읽는 경주에 기댄다. 대개 넉넉히 이기지만 지면 그 테스트가 실패한다. 마무리 확인에서 `-count=50`으로 다시 돌렸고, 1,000번 가운데 1번 졌다(`TestImpactIsNotMeasuredWithoutALoginRole`).

## 14. 패키지별 변경

| 패키지 | 바뀌는 것 |
|---|---|
| `internal/config` | `agents.roles`, `limits.max_inflight`, enforce의 기본값과 검증 |
| `internal/sqlparse` | 깨끗한 트랜잭션을 가리는 문장 종류(`BEGIN`, `SET`, `SHOW`), `DO` 블록, 값 하나짜리 `IN`, 이름으로 실행하는 문장(`EXECUTE`, `FETCH`, `MOVE`), 조건식의 한정된 이름 |
| `internal/proxy` | 세션이 에이전트의 것인지 가리기(시작 패킷을 서버처럼 읽기). 트랜잭션이 깨끗한지 세기. 문장·포털 표를 서버가 완료한 것에 맞추기와 Sync 전의 Query 따라가기. 게이트가 "보내지 마라"고 답하면 메시지를 바꿔 보내고 돌아온 그 에러 하나를 바꿔 쓰기. 닫힌 지점(읽지 못한 메시지, 큰 메시지, FunctionCall, `DO`, 이름으로 실행하는 문장, 뜻이 정해지지 않은 이름, 세션 설정, 이름의 인코딩). 세션 끝내기(복사, 설정 변경) |
| `internal/gate` | enforce에서 `Begin`이 판단까지 끝내고 무엇을 할지 돌려준다. 행 버전 다시 확인. 보류 건 저장. 동시 상한. 누적 값의 구조와 적재. 세기 전의 열 확인. 섀도의 판정을 같은 규칙으로 |
| `internal/decide` | `in_transaction`의 새 뜻, `no_login_role`, `impact:cumulative_unknown`, `concurrent_change`. enforce에서 판정을 행동과 에러 문구로 옮기는 표. 닫힌 지점과 세션을 끝낼 때의 문구 |
| `internal/store` | `stet.held_writes`, `stet.decisions.refused`, 초 단위로 묶은 사용량 질의 |
| `cmd/stet` | enforce를 거부하던 검사를 없앤다. 시작할 때의 확인과 경고. 실행부를 테스트할 수 있게 뗀다 |
| `CLAUDE.md` | 규칙 1의 문구(구현 전에 고쳤다) |

모델 인터페이스는 바뀌지 않는다(규칙 7).

## 15. 지연과 부하

- 섀도 모드와 에이전트가 아닌 세션의 지연은 지금과 같아야 한다. `internal/proxy/latency_test.go`의 2ms 예산을 그대로 둔다. 에이전트가 아닌 세션은 분류조차 하지 않으므로 지금보다 빨라진다.
- enforce에서 판단받는 쓰기는 3장의 단계마다 그 상한까지 기다린다. 상태 읽기(500ms 이하, 자리를 기다리는 시간 포함), 모델 호출(`timeout_ms` 이하), 행 버전 다시 읽기(500ms 이하), 많아야 한 번의 다시 판단, 보류 건 삽입(500ms 이하)이다. 결정론적 규칙으로 판정이 나는 쓰기는 모델을 기다리지 않는다.
- 게이트 대상 테이블에 쓰지 않는 문장은 enforce에서도 기다리지 않는다. 분류 비용만 든다.
- 행 버전 다시 확인은 기본 키로 한 행을 읽는 질의 하나다.
- 마무리 확인(2026-11-17)에서 프록시가 더한 p95는 패스스루 22.5µs, 읽기를 기록하는 세션 23.9µs, 감시하지 않는 세션 22.3µs, enforce의 에이전트 세션 21.9µs, 매번 ParameterStatus를 받는 enforce 세션 22.6µs였다.

## 16. 수용 기준

- [x] `mode: enforce`로 시작한다. `agents.roles`가 없으면 시작하지 않고 이유를 말한다. (`TestInvalidConfigs`의 "enforce needs agent roles"와 "enforce with no roles listed", `TestProxyCommandNamesTheMissingAgentRoles`, `TestRunServesInEnforceMode`)
- [x] 에이전트 세션에서 `commit`으로 판정된 쓰기는 실행되고, 클라이언트가 받는 응답은 직접 연결했을 때와 같다. (`TestEnforceCommittedWritesAnswerLikeADirectConnection`, `TestEnforceSendsACommittedWrite`)
- [x] `hold`, `escalate`, `unsupported`로 판정된 쓰기는 서버에서 실행되지 않는다. 테이블의 내용이 그대로다. 클라이언트는 해당 SQLSTATE의 에러를 받는다. (`TestEnforceHoldsWhatTheModelFlags`, `TestEnforceEscalatesAnEstimate`, `TestEnforceRefusesUnsupportedShapes`)
- [x] 막힌 뒤에도 세션을 계속 쓸 수 있다. Simple Query와 Extended Query 모두에서, 그리고 앞뒤에 다른 문장을 파이프라인으로 붙여 보냈을 때도 응답의 수와 순서가 맞다. (`TestRefusedQueryGetsTheGatesError`, `TestRefusedExecuteGetsTheGatesError`, `TestRefusedPipelineAnswersLikeAServerError`, `TestRefusedExecuteLeavesThePreparedStatementUsable`)
- [x] 한 Sync 아래에서 뒤의 쓰기가 막히면 앞의 문장도 롤백된다. (`TestRefusalRollsBackTheRestOfItsSync`)
- [x] `hold`와 `escalate`로 막힌 쓰기가 `stet.held_writes`에 문장, 바인딩 값, 계정과 함께 남고, 에러의 `DETAIL`에 그 id가 있다. (`TestInsertHeldWrite`, `TestEnforceHoldsWhatTheModelFlags`, `TestHeldWriteKeepsBinaryAndNullParamsAsText`)
- [x] `BEGIN` 바로 뒤의 쓰기는 자동 커밋 쓰기와 같은 판정을 받는다. psycopg 기본 설정으로 보낸 쓰기가 판단된다. (`TestTxDirtyFollowsWhatWasSent`, `TestShadowJudgesTheFirstWriteOfATransaction`, `TestEnforceJudgesTheFirstWriteAfterBegin`. psycopg 자체는 쓰지 않았고, psycopg 기본 설정이 보내는 모양인 `BEGIN` 뒤의 Parse·Bind·Execute·Sync로 확인했다)
- [x] 트랜잭션 안에서 다른 문장 뒤에 온 쓰기는 `ST002`로 막히고, 그 트랜잭션은 실패 상태가 된다. (`TestRefusalFailsTheOpenTransaction`, `TestEnforceRefusesAWriteInADirtyTransaction`)
- [x] 한 Sync 아래에 `BEGIN`과 쓰기를 Execute로 이어 보내면 그 쓰기가 판단된다. (`TestGateSeesCleanTransactions`, `TestEnforceJudgesTheFirstWriteAfterBegin`)
- [x] 같은 이름으로 다시 Parse해서 서버가 거부한 뒤에도, 그 이름의 실행은 서버가 가진 문장으로 판단된다. 건너뛴 Parse와 Close도 같다. (`TestGateJudgesTheStatementTheServerHolds`, `TestSkippedParseAndCloseAreUndone`)
- [x] 막으려던 쓰기의 에러가 앞 메시지의 실패에 가려지면 클라이언트는 앞 메시지의 에러를 그대로 받고, 그 보류 줄은 `undelivered`가 된다. (`TestEarlierFailureHidesTheRefusal`, `TestEnforceMarksAHiddenHoldUndelivered`)
- [x] 트랜잭션 안에서 막힌 뒤 `ROLLBACK`하면 세션이 정상으로 돌아온다. (`TestRefusalFailsTheOpenTransaction`, `TestEnforceRefusesAWriteInADirtyTransaction`)
- [x] `agents.roles`에 없는 계정의 세션에서는 같은 쓰기가 그대로 실행되고 기록이 남지 않는다. (`TestSessionsOfOtherRolesAreNotSeen`, `TestEnforceLeavesOtherRolesAlone`)
- [x] 7장의 표의 줄마다 테스트가 있다. 읽지 못한 메시지(`TestUnreadableStatementsAreRefused`, `TestEnforceClosedPointsAnswerThroughTheGate`), 큰 메시지(`TestOversizedMessagesAreRefused`), FunctionCall(`TestFunctionCallsAreRefused`), `DO` 블록(`TestClassifyDo`, `TestDoBlocksAreRefused`), 이름으로 실행하는 문장(`TestSQLExecuteOfAPreparedWriteIsRefused`, `TestFetchFromAWritePortalIsRefused`), 뜻이 정해지지 않은 이름(`TestUnsureNameIsRefused`), 세션 설정(`TestStatementsTheClassifierMisreadsAreRefused`), 이름의 인코딩(`TestNamesThatAServerEncodingMayCompareOtherwiseAreRefused`), 판단할 수 없는 형태(`TestEnforceRefusesUnsupportedShapes`), 계정 없는 세션(`TestWritesWithoutALoginRoleAreUnsupported`, `TestEnforceRefusesWithoutALoginRole`), 상태 실패와 모델 실패(`TestEnforceFailuresFollowOnFailure`, `TestEnforceRowVersionThatCannotBeReadFollowsOnFailure`), 측정 실패(`TestEnforceRefusesWhenMeasuringFails`), 누적 값 모름(`TestVerdictCumulative`, `TestCumulativeWaitsBeforeLoadingAgain`, `TestEnforceHoldsWhenTheCumulativeUsageIsUnknown`), 동시 상한(`TestInflightLimitTurnsIntoAStateError`, `TestInflightLimitTurnsAMeasuredWriteIntoAStateError`, `TestEnforceHoldsWhenTooManyWritesAreInFlight`), `held_writes` 저장 실패(`TestEnforceRefusesEvenWhenTheHeldWriteIsNotStored`, `TestHeldWriteThatCannotBeStoredAsSentIsStillRefused`, `TestEnforceMarksAnUnconfirmedHeldWriteUndelivered`), 게이트 대상 테이블이 없을 때(`TestRunWithoutTablesWarnsThatEnforceOnlyPassesThrough`, `TestEnforceWithoutAGateOnlyRelays`).
- [x] 상태를 읽은 뒤 다른 연결이 행을 바꾸면 다시 판단하고, 또 바뀌면 `hold`(`concurrent_change`)가 된다. (`TestEnforceRejudgesWhenTheRowChanged`, `TestEnforceHoldsWhenTheRowKeepsChanging`, `TestEnforceSendsAWriteWhoseRowIsGone`, `TestEnforceRejudgesWhenTheRowAppears`)
- [x] 결정론적 규칙으로 판정이 난 쓰기는 enforce에서 모델을 부르지 않는다. (`TestDeterministic`, `TestEnforceSkipsTheModelWhenTheRulesDecide`)
- [x] `WHERE id IN (5)`와 `WHERE id IN ($1)`이 단일 행 쓰기로 분류되어 값 판단을 받는다. (`TestClassifySingleValueIn`, `TestEnforceJudgesASingleValueIn`)
- [x] 섀도 모드에서 같은 쓰기 스트림을 돌리면 판정이 enforce와 같다(모델 답이 같다는 전제에서). 섀도는 아무것도 막지 않는다. (`TestShadowAndEnforceAgreeOnVerdicts`, `TestEnforceNamesOnlyACandidateAsTheDuplicate`. 누적 한도가 없는 설정에서 확인했다. 누적 한도가 있으면 갈라질 수 있다(13장))
- [ ] 섀도 모드의 기존 테스트는 4장과 7장이 바꾼 판정(`in_transaction`, 계정 없는 세션, 누적 값 모름)을 빼고 단언을 고치지 않고 통과한다. (확인 못 함: 그 밖의 기존 단언도 셋이 바뀌었다. `TestAbandonLosesUnansweredWrites`는 CommandComplete를 받은 뒤 끊긴 쓰기를 `Executed`로 보게 바뀌었고(12장), `TestCumulativeWindowSlides`는 초 단위 칸 때문에 기다리는 시간이 늘었으며(12장), `TestClassifyMeasured`는 비교 방식만 바뀌었다(기대값은 그대로). 모두 계획이 허락한 변경이다. `BEGIN`만으로는 더러워지지 않게 되어 `select 1`을 더하거나, 이제 읽히는 3.1 시작 패킷 대신 인증 전 Query로 계정 없는 세션을 만드는 식으로 입력을 바꾼 테스트도 있다)
- [x] 2ms 지연 예산을 지킨다. 에이전트가 아닌 세션은 게이트를 부르지 않는다. (`TestPassthroughLatencyBudget`, `TestObservedReadLatencyBudget`, `TestUnwatchedSessionLatencyBudget`, `TestEnforcedSessionLatencyBudget`, `TestEnforcedSessionSettingsLatencyBudget`, `TestSessionsOfOtherRolesAreNotSeen`)
- [x] 12장의 "이 단계에서 닫는다" 표의 줄마다 테스트가 있다. 7장의 실패 지점(위), 값 하나짜리 `IN`(`TestClassifySingleValueIn`, `TestEnforceJudgesASingleValueIn`), `테이블.함수`(`TestClassifyQualifiedNames`, `TestImpactDoesNotCountThroughATableQualifiedFunction`, `TestAllColumnsFailsClosed`), 동시 측정의 상한(`TestAgentRolesAndLimits`, `TestInflightLimitTurnsIntoAStateError`), 첫 적재(`TestUsageBySecond`, `TestUsageLoadsOnlyWhatCameBeforeItStarted`, `TestUsageRetriesAfterAnInterval`, `TestCumulativeWaitsBeforeLoadingAgain`, `TestUsageLoadOutlivesTheWriteThatStartedIt`, `TestUsageDoesNotLoadAWindowThatStartsAfterTheGate`), 사용량 정리(`TestUsageCountsPerSecond`, `TestUsageKeepsASecondUntilItLeftTheWindow`, `TestUsageDropsIdlePairs`, `TestUsageMergesALoadInOnePass`), 3.1 시작 패킷(`TestStartupUserIsReadAsTheServerReadsIt`, `TestProtocol31SessionHasItsLoginRole`, `TestProtocol31SessionsRecordTheirLoginRole`), 인증 전 문장(`TestWritesWithoutALoginRoleAreUnsupported`, `TestEnforceRefusesWithoutALoginRole`), CommandComplete 뒤 끊김(`TestAbandonKnowsACompletedWriteRan`, `TestCumulativeSettlesAWriteLostAfterItCompleted`), 문장 표(`TestGateJudgesTheStatementTheServerHolds`, `TestSkippedParseAndCloseAreUndone`, `TestNamesAreComparedByTheirFirst63Bytes`, `TestUnsureNameIsRefused`, `TestSQLExecuteOfAPreparedWriteIsRefused`, `TestFetchFromAWritePortalIsRefused`, `TestQueryBeforeSyncIsFollowed`, `TestUnitsAfterAQueryBeforeSyncFollowTheServer`, `TestRefusalsAfterAQueryBeforeSyncAreDelivered`, `TestRewriteIsBoundToTheReplacementsOwnError`, `TestCopyFromStdinThroughTheExtendedProtocolEndsTheSession`, `TestSyncDuringCopyEndsTheSession`), 종료 순서(`TestRunRecordsWhatWasInFlightAtShutdown`).
- [x] 분류기와 다르게 읽히는 세션 설정에서는 에이전트 세션의 문장이 `ST002`로 막히고, 두 설정을 고치는 `SET`·`RESET`만 지나간다. 고친 세션은 다시 판단받는다. 설정을 읽을 수 없는 값으로 바꾸고 이미 다음 메시지를 보낸 세션은 끝난다. 섀도는 그대로 보낸다. (`TestStatementsTheClassifierMisreadsAreRefused`, `TestSettingsTheClassifierCannotReadAreRefused`, `TestRepairsSettings`, `TestUnreadableSessionRepairsItsSettings`, `TestClientEncodingUnicodeIsUTF8`, `TestSettingChangedAheadOfAStatementEndsTheSession`, `TestSettingChangedAheadOfSentStatementsEndsTheSession`, `TestSettingReportedWhileAMessageIsInspected`)
- [x] 아직 답이 오지 않은 변경 때문에 뜻이 둘 이상인 이름은 그 뜻을 모두 들고 있고, 그 가운데 감시 대상이 있으면 enforce에서 그 실행이 막힌다. 이름은 앞 63바이트로 비교한다. 감시 대상 이름을 SQL `EXECUTE`·`FETCH`·`MOVE`로 실행하면 막힌다. (`TestUnansweredChangeLeavesSeveralMeanings`, `TestUnsureNameIsRefused`, `TestUnsettledNamesAreRefused`, `TestNamesAreComparedByTheirFirst63Bytes`, `TestSQLExecuteOfAPreparedWriteIsRefused`, `TestFetchFromAWritePortalIsRefused`, `TestWhatRunsAWatchedNameIsRefused`, `TestNameThatMayMeanAnUncleanStatementDirties`)
- [x] enforce의 에이전트 세션에서 확장 프로토콜의 `COPY ... FROM STDIN`과 단순 복사 중의 Sync는 세션을 FATAL `ST002`로 끝낸다. CopyDone과 다음 Query를 한 번에 보낸 세션과 한 Query의 복사 여럿은 그대로 쓴다. 막은 `COPY ... FROM STDIN`은 멈추지 않고 에러를 받는다. (`TestCopyFromStdinThroughTheExtendedProtocolEndsTheSession`, `TestSyncDuringCopyEndsTheSession`, `TestSyncDuringASecondCopyEndsTheSession`, `TestSyncBeforeASecondCopyEndsTheSession`, `TestTwoCopiesInOneQueryKeepTheSession`, `TestCopyRuleHoldsOnlyDuringTheCopy`, `TestRefusedCopyFromStdinDoesNotHang`)
- [x] 시작할 때의 확인이 8장대로다. Stet 자신의 계정이 목록에 있으면 시작하지 않는다. enforce에서 `db_user_namespace`가 켜져 있거나, 그것이나 Stet 자신의 계정을 확인하지 못하면 시작하지 않는다. 섀도는 확인하지 못해도 시작한다. 에이전트 계정의 superuser, superuser 역할의 구성원, `stet` 스키마 권한, 테이블 소유, `search_path`를 역할 구성원을 거친 것까지 경고한다. (`TestCheckRefusesStetsOwnAccount`, `TestRunRefusesToStartAsAnAgentsAccount`, `TestCheckRefusesDBUserNamespaceInEnforce`, `TestCheckStopsEnforceWhenItCannotRead`, `TestRunStopsEnforceButStartsShadowWhenChecksCannotRead`, `TestCheckWarnsAboutAgentRoles`, `TestCheckFollowsRoleMembership`, `TestCheckWarnsAboutPassSettings`, `TestNewerThanParser`. `db_user_namespace = on`은 테스트 서버에서 켤 수 없어 읽는 함수를 바꿔 끼워 확인했다)
- [x] `go test -race ./...`가 통과한다. (`STET_TEST_PG=127.0.0.1:55432 go test -race -count=1 ./...`가 모든 패키지에서 `ok`)

## 17. 정한 것과 남긴 것

**정한 것**

- 경계는 "트랜잭션 안이냐"가 아니라 "이미 무언가를 바꿨느냐"다.
- 막을 때는 서버가 에러를 보게 한다. 서버의 답이 프록시가 따라가는 단위와 어긋나는 순서를 만나면 세션을 끝낸다.
- 섀도와 enforce의 판정 규칙은 하나다.
- 실패하면 막는다. 운영자가 `pass`를 고를 수는 있다. 확인할 수 없는 것(서버가 다르게 읽을 수 있는 세션 설정, 뜻이 정해지지 않은 이름, 시작할 때 확인하지 못한 `db_user_namespace`)도 막는 쪽으로 본다.
- 에이전트 역할의 세션만 본다.

**남긴 것**

- 승인과 거부(2b). `held_writes`의 `status`가 바뀌는 길, 승인할 때의 권한, 만료.
- 2b가 이 단계에서 넘겨받는 것. 누가 `stet.held_writes`를 읽고 고칠 수 있는지. 프록시가 실행됐다고 보고한 보류 건의 줄을 거두는 것(남겨 두면 승인할 때 같은 쓰기가 두 번 실행된다). 클라이언트에게 알리지 못했는데 삽입이 늦게 들어와 `pending`으로 남은 줄(13장).
- 더러운 트랜잭션 안의 쓰기에 결정론적 한도만이라도 적용하는 것. 섀도 기록에서 `in_transaction` 비율을 본 뒤에 정한다.
- 테이블별·역할별 모드. 한 테이블씩 enforce로 넘기는 길.
- 읽기에 대한 판정.
- 문장에 행 버전 조건을 붙이는 정확한 동시성 확인(5번 단계).
- 에이전트 세션의 `search_path`와 `SET ROLE`을 따라가는 것.
- 응답을 기다리지 않고 이어 보낸 쓰기를 판단하는 것. 클라이언트 쪽에서 트랜잭션의 경계를 따라가야 한다.
- 설정을 바꾼 뒤 이어 보낸 메시지를 서버의 답까지 붙잡아 두어 세션 설정의 경주를 닫는 것.
- `UTF8`이 아닌 `client_encoding`과 `standard_conforming_strings = off`를 서버처럼 읽는 것.
- 확장 프로토콜의 `COPY ... FROM STDIN`을 세션을 끝내지 않고 따라가는 것.
