# Stet v0.2 6단계 첫 부분 스펙: 써 보는 길

상태: 구현 (2026-11-22). 설계는 2026-11-22에 했고, 구현은 [plan-v0.2-tryout](plan-v0.2-tryout.md)의 Task 1~11이다. 구현하면서 정한 것(계획의 스펙 보정 P1~P8과 진행 중의 판단)은 해당 장에 옮겨 적었고, 13장에 모았다. 근거는 concept-v0.2 9장의 6번("내놓기 전 숙제"), D9, D30, 그리고 2026-11-22의 논의(배포 방식, hoop의 배포 조사)다. 써 보는 순서는 [quickstart](quickstart.md)에 있다.

6번 단계는 둘로 나눈다. 이 문서는 첫 부분이다. 둘째 부분(서버 배포: 클라이언트 쪽 TLS, 자격 증명 보관, 가용성)은 이 문서의 범위가 아니다.

## 1. 목표와 비목표

지금 Stet을 써 보려면 저장소를 받아 `go build`를 하고, 테이블마다 설정을 손으로 쓰고, 판단 결과를 DB에서 직접 조회해야 한다. 업스트림에는 평문으로만 붙어서 Neon·Supabase·RDS 같은 매니지드 Postgres에는 시작도 못 한다. 이 단계는 한 사람이 자기 PC에서 자기 DB에 Stet을 10분 안에 붙여 보는 길을 만든다.

**누구를 위한 것인가.** 코딩 에이전트(Claude Code 등)를 자기 개발 DB나 스테이징 DB에 붙여 쓰는 개발자다. DB 계정은 대개 하나뿐이고, DB는 대개 원격의 매니지드 Postgres다.

**무엇을 얻는가.** 에이전트가 방금 한 쓰기가 실제로 몇 행에 닿았고 어디로 번졌는지를, DB가 답한 숫자로 터미널에서 본다.

```
$ stet init                      # DATABASE_URL을 읽어 stet.yaml을 만든다
$ stet proxy -config stet.yaml   # 에이전트의 접속 주소를 127.0.0.1:6432로 바꾼다
stet: watching 2 tables on 127.0.0.1:6432 (shadow: nothing is blocked)
20:39:42  ok            UPDATE contacts  1 row, fresh read  update contacts set status = 'customer' where id = 7
20:39:45  would hold    UPDATE contacts  300 rows, ~30% of table  [impact:rows]  update contacts set status = 'churned' where company_id < 3
20:39:48  would hold    DELETE contacts  200 rows, ~20% of table, cascades into deals  [impact:rows]  delete from contacts where id between 1 and 200
```

위 줄은 quickstart 7장의 빈 DB(`contacts` 1,000행, `deals`)에서 실제로 나온 것이다. 처음 설계의 예는 칸을 공백 하나와 열 맞춤으로 적어 6장의 규칙(칸 사이 공백 둘)과 달랐다. 규칙을 따른다(P2).

**목표**

- 업스트림 구간에 TLS로 붙는다. 에이전트의 세션, 취소 요청, Stet 자신의 연결이 같은 규칙을 쓴다.
- `stet init`이 DB를 읽어 설정 파일을 만들고, 에이전트에게 줄 접속 문자열을 알려 준다.
- `stet proxy`가 판단한 쓰기마다 사람이 읽는 한 줄을 낸다.
- 모델 없이, 계정 하나로, 섀도 모드에서 돈다. 키가 필요 없고 데이터가 DB 밖으로 나가지 않는다.
- 저장소를 받지 않고 설치할 수 있는 산출물을 만든다: 버전이 찍힌 바이너리 묶음과 Docker 이미지.

**비목표**

- 에이전트와 Stet 사이 구간의 TLS, Stet이 에이전트를 직접 인증하는 것, 진짜 비밀번호를 Stet이 쥐는 것(자격 증명 보관). 둘째 부분이다(2장의 "다").
- 가용성, 여러 인스턴스. 둘째 부분이다.
- DB에 아무것도 만들지 않는 실행 방식(2장의 "나").
- 판정을 바꾸는 것. 게이트의 규칙, 한도, verdict는 그대로다.
- 실제 공개. 릴리스 게시, brew 저장소, 이미지 레지스트리에 올리는 일은 하지 않는다(8장).
- Windows, 연결 풀러를 거친 접속, 클라이언트 인증서로 하는 에이전트 인증.

**실패로 볼 것.**

- 섀도 모드에서 에이전트의 문장이나 그 결과가 Stet 없이 보냈을 때와 달라지는 것(규칙 6). 3장의 인증 방식 목록은 문장이 아니고, 고치지 않으면 접속 자체가 되지 않는다. 그 하나만 예외다.
- 서버 인증서를 확인하라고 적은 설정(`verify-ca`, `verify-full`)에서 확인 없이 붙는 것. TLS를 요구한 설정에서 평문으로 붙는 것.
- 이 길로 깐 Stet을 "통제한다"고 말하는 것. 같은 기기의 에이전트는 진짜 접속 정보를 읽어 Stet을 건너뛸 수 있다(10장).
- 출력 때문에 세션이나 판단이 서는 것.

## 2. 접근

세 가지를 놓고 "가"를 골랐다(2026-11-22).

| | 내용 | 대가 |
|---|---|---|
| **가. 최소 경로** (고름) | 업스트림 TLS, `stet init`, 한 줄 출력, 설치 산출물. 기록은 지금처럼 대상 DB의 `stet` 스키마에 둔다 | DB에 스키마 하나가 생긴다. 남과 같이 쓰는 DB에서는 망설일 수 있다 |
| 나. 흔적 없는 체험 | 가에 더해, DB에 아무것도 만들지 않고 메모리와 로컬 파일만 쓰는 실행 방식 | 게이트가 기록 저장소 없이 도는 경로가 하나 더 생긴다. 재시작하면 누적 한도가 0부터 세고, 되돌리기와 승인을 쓸 수 없다 |
| 다. 접속 문자열 그대로 | 에이전트 쪽에서도 TLS를 받는다 | Stet이 에이전트를 직접 인증하고 진짜 비밀번호를 쥐어야 한다. 서버 배포의 일이다 |

**가를 고른 이유.** 엔진을 건드리지 않고, 되돌리기까지 지금 있는 기능을 전부 써 볼 수 있다. 나는 스키마 때문에 멈추는 사람이 실제로 보이면 더한다.

**다가 왜 지금이 아닌가.** 양쪽 구간이 모두 TLS면 인증을 중계할 수 없다. libpq 계열 클라이언트(`psql`, psycopg)는 TLS 위에서 SCRAM 인증을 서버 인증서에 묶는데, 에이전트가 보는 인증서는 Stet의 것이라 서버가 로그인을 거부한다. 묶기를 빼고 중계해도 서버가 다운그레이드로 보고 거부한다(PostgreSQL 14와 17의 `fe-auth-scram.c`, `auth-scram.c`에서 확인). 그래서 클라이언트 쪽 TLS는 자격 증명 보관과 한 덩어리다. 같은 기기 안의 구간은 TLS가 필요 없으므로, 써 보는 길은 그 구간을 평문으로 둔다.

**확인한 것** (2026-11-22, 일회용 서버에서)

- 계정 하나, 모델 없음, 섀도로 지금 코드가 돈다. 계정 `try_one` 하나를 Stet의 `upstream`과 클라이언트에 함께 쓰고 쓰기 넷을 보내자, 한 행 수정은 `commit`과 `fresh`, 300행 수정과 200행 삭제는 `hold`(`impact:rows`, 삭제에는 `deals`로의 연쇄)로 기록됐다. `agents.roles`를 비우면 섀도는 모든 세션을 본다.
- 업스트림만 TLS일 때 `psql` 14는 그대로 중계하면 로그인하지 못한다(`server offered SCRAM-SHA-256-PLUS authentication over a non-SSL connection`). 서버가 TLS 연결에서 제안하는 `SCRAM-SHA-256-PLUS`를 목록에서 빼고 중계하면 로그인한다. 틀린 비밀번호는 서버가 그대로 거부한다.
- 클라이언트의 접속 문자열에 `sslmode=require`나 `channel_binding=require`가 있으면 Stet에 붙지 못한다.

## 3. 업스트림 TLS

### 설정

새 설정 키는 없다. `upstream` 주소의 질의 파라미터를 쓴다. Stet 자신의 연결(pgx)이 이미 읽는 것과 같은 파라미터다. 해석은 `pgconn.ParseConfig`가 한 결과를 그대로 쓴다. 두 구간이 다른 규칙으로 붙는 일이 없게 하기 위해서다.

| `sslmode` | 프록시 구간의 동작 |
|---|---|
| `disable` | 평문 |
| 적지 않음, `prefer` | TLS를 청하고, 서버가 거절하면 평문으로 간다. 인증서는 확인하지 않는다 |
| `require` | TLS가 아니면 붙지 않는다. 인증서는 확인하지 않는다. `sslrootcert`가 있으면 `verify-ca`와 같다(libpq와 같다) |
| `verify-ca` | 인증서의 발급자를 확인한다 |
| `verify-full` | 발급자와 호스트 이름을 확인한다. `sslrootcert`가 없으면 시스템의 루트 인증서를 쓴다 |
| `allow` | 받지 않는다. 설정 검증에서 거부한다. 평문으로 먼저 붙어 보고 거절당하면 TLS로 다시 붙는 방식은 중계하는 세션에서 흉내 낼 수 없다 |

**`sslmode`를 적지 않은 기존 설정이 바뀐다.** 이 판 전에는 Stet 자신의 연결(pgx)만 `prefer`로 TLS를 청했고, 프록시 구간은 늘 평문이었다. 이 판부터는 프록시 구간도 `prefer`를 따른다. 서버가 TLS를 받으면 에이전트의 세션도 TLS로 서버에 닿고, 그 세션의 인증 방식 목록이 고쳐진다(아래 "인증 방식 목록"). 서버가 TLS를 받지 않으면 전처럼 평문이다. 전과 같이 평문으로 두려면 `sslmode=disable`을 적는다. 기존 테스트의 세션도 `sslmode`를 적지 않은 것은 TLS 업스트림을 지나고, 단언은 고치지 않고 통과한다(12장).

- `sslrootcert`는 Stet 자신의 연결과 프록시 구간에 함께 쓴다.
- 호스트가 IP 주소가 아니면 TLS 인사에 서버 이름(SNI)을 넣는다. `sslsni=0`이면 넣지 않는다. Neon은 이 이름으로 대상을 고른다.
- `sslcert`와 `sslkey`(클라이언트 인증서)는 Stet 자신의 연결에만 쓴다. 중계하는 세션에는 인증서를 내지 않는다. 서버가 인증서로 계정을 정하는 설정(`cert`)이면 에이전트의 세션이 Stet의 신원으로 로그인되기 때문이다.
- `sslnegotiation=direct`(PostgreSQL 17의 바로 TLS)는 받지 않는다. 설정 검증에서 거부한다.
- **Stet 자신의 연결과 프록시 구간은 같은 호스트와 포트 하나에 닿아야 한다.** 프록시는 URL의 호스트와 포트(없으면 5432)로 중계하고, Stet 자신의 연결은 `pgconn`이 읽는 대로 붙는다. 둘이 다르게 읽는 주소는 설정 검증에서 거부한다(`config.OneTarget`): 질의 파라미터의 `host`·`hostaddr`·`port`, 호스트 여럿(장애 조치 목록), URL에 포트가 없는데 `PGPORT`나 서비스 파일이 다른 포트를 주는 것, 비밀번호의 인코딩하지 않은 `?`·`#`처럼 두 해석이 갈리는 것. 에러는 `upstream: stet's own connections and the proxy would reach different hosts or ports for this address; write one host and one port in the address itself (not in its query, PGHOST, PGPORT or a service file), and percent-encode special characters in the password`다. 처음 설계는 "호스트가 여럿이면 첫 호스트만 쓴다(바뀌지 않는다)"였으나, 실제로는 프록시가 다른 서버로 중계할 수 있어 거부로 바꿨다.
- Stet이 만든 `upstream`의 에러는 주소를 인용하지 않는다(`has no host`, `is not a valid URL`, `must be a postgres:// or postgresql:// URL`, 위의 한 대상 에러). 비밀번호가 들어 있기 때문이다. pgconn이 읽지 못하는 주소(예: `sslmode=foo`)는 다르다. pgconn의 에러가 그대로 나오고, 그 에러는 비밀번호를 `xxxxx`로 가린 채 주소를 인용한다: ``stet: stet.yaml: upstream: cannot parse `postgres://app:xxxxx@db.example.invalid:5432/crm?sslmode=foo`: failed to configure TLS (sslmode is invalid)``. 계정, 호스트, 데이터베이스, 파라미터는 보인다. pgconn이 가리는 것은 사용자 정보의 비밀번호와 `password=` 파라미터이고, pgx는 이 가림을 best effort라고 적는다(형식이 망가진 주소는 다 가리지 못할 수 있다). `stet init`도 같은 주소에서 같은 에러를 낸다.

### 프록시 구간의 절차

에이전트의 세션마다, 그리고 취소 요청마다 같다.

1. 업스트림에 TCP로 붙는다(지금과 같은 시간 제한).
2. TLS를 쓰는 설정이면 SSLRequest를 보내고 답을 **정확히 한 바이트** 읽는다. 버퍼를 두지 않는다. 답 뒤에 따라온 평문 바이트를 TLS 안의 것으로 읽는 일을 막는다.
3. 답이 `S`면 TLS 인사를 한다. 시간 제한은 접속과 같다. 답이 `N`이면, 평문을 허용하는 설정(`prefer`)일 때만 평문으로 계속한다. 그 밖의 답은 실패다.
4. 실패하면 클라이언트에 `FATAL`(SQLSTATE `08001`)과 이유를 주고 끝낸다. 이유는 `stet: upstream does not accept TLS, and the sslmode of upstream needs it`이나 `stet: TLS to upstream <호스트:포트> failed: <인증서 확인 실패의 문구>`다. 프록시는 `sslmode`의 이름을 모르고 해석된 TLS 설정만 받으므로 문구에 `sslmode`의 값을 적지 않는다(P1). 경고 로그(`upstream unreachable`)를 남긴다. 취소 요청은 답할 상대가 없으므로 로그만 남긴다.
5. 그 뒤는 지금과 같다. 클라이언트의 시작 패킷을 보내고 양쪽을 중계한다.

클라이언트의 SSLRequest와 GSSENCRequest에는 지금처럼 `N`으로 답한다.

### 인증 방식 목록

클라이언트 쪽이 평문이고 업스트림 쪽이 TLS인 세션에서, 서버가 보낸 AuthenticationSASL의 방식 목록에서 `SCRAM-SHA-256-PLUS`를 뺀다.

- 모든 세션에 한다. 게이트가 보는 세션이든 아니든, 섀도든 enforce든 같다.
- 빼고 나서 목록이 비면 클라이언트에 `FATAL`(SQLSTATE `28000`, `stet: the server offers only channel-bound authentication, which cannot pass through stet`)을 주고 끝낸다.
- 업스트림이 평문이면 아무것도 고치지 않는다. 서버가 그 방식을 제안하지 않는다.
- 인증이 끝난 뒤(서버의 첫 ReadyForQuery 뒤)에는 메시지마다 드는 일이 늘지 않아야 한다. 지연 예산(2ms)의 측정에 든다.
- 그 밖의 인증 메시지는 지금처럼 손대지 않고 중계한다. 비밀번호는 Stet이 보지 않는다(md5와 SCRAM의 경우. `password` 방식은 지금도 평문이 지나간다).

이것이 뜻하는 것: 클라이언트는 자기 인증을 서버 인증서에 묶지 못한다. 서버가 진짜인지는 Stet이 `verify-full`로 확인한다. `require`나 `prefer`면 아무도 확인하지 않는다.

### 시작할 때의 경고

막지 않고 경고만 한다. 같은 사설망 안에서 평문으로 쓰는 기존 구성이 있다.

- `upstream`의 호스트가 이 기기(루프백)가 아닌데 평문으로 갈 수 있는 설정(`disable`, `prefer`, 적지 않음)일 때.
- `upstream`의 호스트가 루프백이 아닌데 인증서를 확인하지 않는 설정(`require`에 `sslrootcert` 없음)일 때.
- `listen`이 루프백이 아닐 때. 클라이언트와 Stet 사이는 암호화되지 않는다.

경고는 고칠 방법을 함께 말한다(`fix: add sslmode=verify-full to upstream` 등). 이 기기인지는 호스트가 `localhost`이거나 루프백 IP인지로 본다. 경고에는 `upstream`의 호스트만 적고 주소 전체는 적지 않는다.

## 4. `stet init`

```
stet init [-config stet.yaml] [-listen 127.0.0.1:6432] [-tables a,b,c] [-yes] [-force] [<DB 주소>]
```

DB 주소는 인자로 받고, 없으면 환경 변수 `DATABASE_URL`에서 읽는다. 문서는 환경 변수를 먼저 권한다. 인자의 비밀번호는 셸 기록과 프로세스 목록에 남는다. 플래그는 주소 앞에 둔다. 주소 뒤에 오면 `flags go before the address`로 멈춘다.

`init`은 DB에 아무것도 쓰지 않는다. 읽기 전용 트랜잭션에서만 묻는다. 비밀번호는 쓰는 설정 파일 하나에만 나간다. `init`이 만든 출력과 에러에는 넣지 않는다. pgconn이 읽지 못하는 주소에서는 pgconn의 에러가 주소를 인용하고, 비밀번호는 pgconn이 best effort로 `xxxxx`로 가린다(3장). 서버가 보낸 글자(버전, 계정과 데이터베이스 이름, 서버의 에러, 인증서 에러)는 제어 문자, U+2028·U+2029, 방향 제어 문자를 공백으로 바꿔 보인다. 서버도 믿지 않는다(규칙 5).

**하는 일**

1. **주소를 정한다.** `postgres://`와 `postgresql://`만 받는다.
   - 붙기 전에 거부하는 주소가 있다. 에러는 주소를 인용하지 않는다.
     - 호스트가 없는 것(`the database address has no host`). pgx는 유닉스 소켓으로 읽는다.
     - 공백이 든 것(`the database address contains a space; percent-encode it (%20) or remove it`). pgx는 질의 키 둘레의 공백을 버리지만 Stet의 나머지 코드는 버리지 않는다.
     - 질의 파라미터에 `host`·`hostaddr`·`port`가 있는 것(`the database address sets host, hostaddr or port in its query; write the host and port in the address itself`).
     - 호스트가 여럿인 것(`the database address lists more than one host; stet relays to one server, so give one host`).
     - 그 밖에 Stet 자신의 연결과 프록시 구간이 다르게 읽는 것. 설정 검증과 같은 검사(`config.OneTarget`, 3장)다.
   - `sslmode`가 적혀 있으면 그대로 쓴다(`allow`는 거부).
   - 적혀 있지 않고 호스트가 이 기기면 그대로 둔다. 이 기기인지는 호스트가 `localhost`이거나 루프백 IP인지로 본다.
   - 적혀 있지 않고 호스트가 다른 기기면 `verify-full`로 붙어 본다. 인증서 확인에서 실패하면 `require`로 낮춰 붙고, 그 사실과 고치는 법(`sslrootcert`에 서비스의 CA 파일을 준다)을 알린다: `the server's certificate could not be checked (<이유>), so upstream uses sslmode=require: the connection is encrypted, but the server is not verified. To verify it, add sslrootcert=<the service's CA file> and sslmode=verify-full to upstream in the config file.` 서버가 TLS를 받지 않으면 멈추고, 평문으로 쓰려면 `sslmode=disable`을 직접 적으라고 말한다(`the server does not accept TLS; to connect in plaintext anyway, write sslmode=disable in the address yourself`). 원격 평문은 사용자가 적어야 한다.
   - 정한 `sslmode`를 설정 파일의 `upstream`에 적는다. 주소 끝에 `sslmode=…`를 더하고 나머지 글자는 그대로 둔다.
2. **붙어서 확인한다.** `stet proxy`가 쓰는 것과 같은 방식(같은 시작 파라미터)으로 붙는다. 붙지 못하면 서버의 에러를 보이고 멈춘다. 연결 풀러를 거친 주소에서 흔히 나는 실패이므로, 에러 아래에 안내를 붙인다(`if this address goes through a connection pooler, use the direct connection address instead`). 붙으면 다음을 읽는다: 서버 버전, 접속한 계정과 데이터베이스, `stet` 스키마가 이미 있는지, 없으면 그 계정이 스키마를 만들 수 있는지. 한 번 붙어 보는 데 15초, Stet의 연결을 열고 이것들을 읽는 데 30초를 넘기면 멈춘다.
   - PostgreSQL 14보다 낮으면 멈춘다. Stet의 스키마와 테스트가 확인된 버전은 14뿐이다(13에서 코어에 들어온 `gen_random_uuid()`를 쓰고, 되돌리기는 14가 필요하다). 낮은 버전을 받는 것은 남긴다(13장).
   - `stet` 스키마가 없고 만들 권한도 없으면 멈추고, 필요한 권한(`GRANT CREATE ON DATABASE … TO …`)을 말한다.
3. **테이블을 고른다.** 다음을 모두 채우는 테이블이다.
   - 일반 테이블이거나 파티션을 나눈 테이블이다. 파티션(자식)은 일반 테이블이고 부모의 기본 키를 물려받지만, 고르지 않고 세지도 않는다. 부모를 고른다(P4). 시스템 스키마, 임시 스키마, `stet` 스키마의 것이 아니다.
   - 스키마 없이 적은 이름이 그 계정의 `search_path`에서 그 테이블로 풀린다. Stet은 설정의 이름을 그렇게 푼다.
   - 컬럼 하나짜리 기본 키가 있다.
   - 그 계정이 읽을 수 있다.
   - 테이블 이름과 키 컬럼 이름이 소문자, 숫자, 밑줄만으로 되어 있고 숫자로 시작하지 않는다. 키 컬럼 이름도 YAML과 SQL에 그대로 적히기 때문이다(P4).
   - 위 조건을 이 순서로 보고, 처음 걸린 이유 하나로 뺀다: `not reached by search_path`, `name needs quoting`, `no single-column primary key`, `key column name needs quoting`, `not readable by this account`.
   - `-tables`를 주면 그 테이블만 본다. 그 가운데 조건에 맞지 않는 것이 있으면 테이블마다 이유를 한 줄씩 대고 멈춘다(`table "x": not found` 등). 이때는 100개 상한이 없다.
   - 100개를 넘으면 행 수 추정(`reltuples`)이 큰 순서로 100개만 넣고(같으면 이름 순), 뺀 것의 수와 `-tables`로 고르는 법을 알린다. PostgreSQL 14에서 파티션을 나눈 테이블의 `reltuples`는 손으로 `ANALYZE`하기 전까지 -1이라 맨 뒤로 가고, 상한에서 빠질 수 있다. 그대로 둔다. 빠졌다는 알림이 `-tables`를 말한다.
   - 조건에 맞지 않아 뺀 테이블은 이유별로 세어 알린다(`left out 3 tables: no single-column primary key`). 하나도 남지 않으면 `no table can be watched`로 멈춘다.
4. **무엇이 생기는지 보이고 묻는다.** `stet` 스키마가 없으면, `stet proxy`를 처음 띄울 때 그 데이터베이스에 `stet` 스키마가 생긴다는 것, 어느 계정으로 만드는지, 그 밖에는 아무것도 바꾸지 않는다는 것, 지우는 문장(`DROP SCHEMA stet CASCADE;`)을 보이고 계속할지 묻는다(`Continue? [y/N] `).
   - `y`나 `yes`(대소문자 무관)면 계속한다. 그 밖의 답과 입력의 끝(EOF)은 "아니오"이고, `stopped; nothing was written`으로 멈춘다.
   - `-yes`면 묻지 않는다. 표준 입력이 터미널이 아닌데 `-yes`가 없으면 `this is not a terminal, so stet init cannot ask; read the lines above and pass -yes to continue`로 멈춘다. 터미널인지는 문자 장치인지로 본다(P7). `/dev/null`도 문자 장치라 묻지만, 읽는 것이 EOF라 "아니오"가 된다.
   - 스키마가 이미 있으면 묻지 않는다.
5. **설정 파일을 쓴다.** 권한은 `0600`이다(비밀번호가 든다). 같은 디렉터리의 임시 파일에 다 쓰고 나서 제자리에 놓으므로, 파일은 다 쓰였거나 없다. 묻지 못했으면 쓰지 않는다. 파일이 이미 있으면 붙기 전에 멈춘다(`stet.yaml already exists; pass -force to overwrite it`). `-force`면 덮어쓴다. 쓰기 전에 그 내용을 `stet proxy`와 같은 설정 검증(`config.Parse`)에 넣어 보고, 받지 않으면 이유를 대고 멈춘다(`stet proxy would not accept the config for this address: …`). 이유에서는 주소를 `<the database address>`로, 비밀번호를 `<password>`로 바꾼다. `init`이 `stet proxy`가 거부할 파일을 쓰는 일이 없게 하기 위해서다(예: `sslnegotiation=direct`).
6. **다음 할 일을 알린다.** 띄우는 명령(`stet proxy -config …`)과 에이전트에게 줄 접속 문자열이다. 접속 문자열은 호스트와 포트를 Stet의 것(`listen`)으로 바꾸고, 파라미터를 지우고, `sslmode=disable`을 붙인 것이다. 비밀번호는 `<password>`로 가려 보이고, 계정과 비밀번호는 전과 같다고 말한다.
   - 지우는 파라미터는 이름이 `ssl`로 시작하는 것, `channel_binding`, `gssencmode`, `requiressl`이다(TLS와 채널 바인딩은 Stet과 서버 사이의 일이다). 그리고 `host`, `hostaddr`, `port`다(에이전트를 Stet을 건너 서버로 보낸다).
   - 남은 파라미터는 적힌 글자 그대로, 이름 순서로 적고 끝에 `sslmode=disable`을 더한다. `password` 파라미터는 `password=<password>`로 가린다.
   - `listen`의 호스트가 비었거나 `0.0.0.0`·`::`면 `127.0.0.1`로 적는다(P8).

출력은 이렇다(스키마가 없을 때).

```
found 2 tables to watch
stet proxy will create one schema, "stet", in database "crm" as "app" the first time it starts.
It changes nothing else. To remove it later: DROP SCHEMA stet CASCADE;
Continue? [y/N] y
wrote stet.yaml: 2 tables, shadow mode, no model

next:
  stet proxy -config stet.yaml

then give the agent this address instead of the database's. The user and password are the same as before:
  postgres://app:<password>@127.0.0.1:6432/crm?sslmode=disable
```

**쓰는 설정**

```yaml
# Written by stet init (stet 0.2.0) on 2026-11-22.
# Shadow mode: stet records what it would do and blocks nothing.
# Every key is described in stet.example.yaml and in docs/spec-v0.1.md, chapter 9.
# This file holds the database password: keep it private.

mode: shadow
listen: 127.0.0.1:6432
upstream: postgres://app:…@db.example.com:5432/crm?sslmode=verify-full
model:
  adapter: none
tables:
  contacts:
    pk: id
    impact:
      max_rows: 100
    reads:
    stale:
```

- 테이블은 이름 순서로 적는다. 테이블마다 `pk`, `impact.max_rows: 100`, `reads`를 적는다. `reads`와 `stale`은 값 없는 키로 적는다. 키가 있으면 켜진다.
- YAML이 다른 값으로 읽을 이름(`null`, `true` 등)과 특수 문자가 든 주소는 따옴표로 감싼다.
- `stale`은 키 컬럼의 타입이 낡은 쓰기 탐지가 추적하는 타입일 때만 적는다(spec-v0.2-stale 4장의 목록). `init`이 `stale`을 적은 테이블이 키 타입 때문에 시작할 때 "추적하지 않는다"는 경고를 내서는 안 된다. 그 밖의 조건(정렬 규칙, 상속)은 시작할 때의 경고에 맡긴다.
- `send_fields`, `candidates`, `questions`, `policy`, `agents`, `undo`는 적지 않는다.
- 파일의 주석은 영어다. 프로그램이 만든 출력이다. 맨 위에 무엇으로 만들었는지(`stet init`, 버전, 날짜)와 설정 형식의 문서 위치를 적는다.

## 5. `send_fields`

`model.adapter`가 `none`이면 `send_fields`는 없어도 된다. 모델을 쓰면 지금처럼 필수다.

- 게이트는 고치지 않는다. 지금 판단 기록의 상태(`state`) 안의 `proposed`와 `current`는 `send_fields`의 컬럼만 담는다. 목록이 비면 그 둘은 빈다. 키와 행 버전, 영향 측정, 읽기 확인은 그대로 기록된다.
- 판단 기록의 `proposed` 컬럼, `statement`, `params`에는 지금처럼 쓰려던 값이 그대로 남는다. `send_fields`는 모델로 나가는 것을 고르는 목록이고, 사용자 자신의 DB에 남는 기록을 줄이는 목록이 아니다. 문서(quickstart)에 그렇게 적는다.
- 모델을 쓰는데 `send_fields`가 없으면 지금과 같은 에러(`tables.<이름>.send_fields: required: list the columns the model may see`)로 시작하지 않는다.
- `candidates`나 `context`의 SQL이 있는 테이블은 모델 없이도 지금처럼 동작한다(바뀌지 않는다).

## 6. 판단 한 줄

`stet proxy`는 게이트가 판단을 기록하는 건마다 표준 출력에 한 줄을 쓴다. 로그는 지금처럼 표준 에러로 간다.

**켜지는 때.** `-decisions auto|on|off`. 기본은 `auto`이고, 표준 출력이 터미널일 때만 켠다. 문장에는 값이 들어 있으므로, 서버에서 로그 수집기로 흘러가는 출력에는 기본으로 내지 않는다. 그 밖의 값은 `-decisions: "x" must be auto, on or off`로 시작하지 않는다.

- 터미널인지는 문자 장치인지로 본다(P7). 표준 라이브러리로는 그것까지만 알 수 있고, 새 의존성은 더하지 않는다. `/dev/null`도 문자 장치라 켜지지만, 거기로 간 줄은 해가 없다.

**한 줄의 모양.** 영어다. 모든 칸 사이는 공백 둘이고, 판정 칸만 12글자 폭으로 채운다(P2). 줄 끝의 공백은 지운다.

```
<시각>  <판정>  <OP> <테이블>  <사실들>  [<이유들>]  <문장>
```

| 칸 | 내용 |
|---|---|
| 시각 | 쓰기가 들어온 시각(판단 기록의 `created_at`), 이 기기의 시간대, `15:04:05` |
| 판정 | 섀도: `commit`은 `ok`, `hold`·`escalate`·`reject`는 `would hold`·`would escalate`·`would reject`, `unsupported`는 `unsupported`. enforce: `ok`, `hold`, `escalate`, `reject`, `unsupported`. 열두 글자 폭으로 맞춘다 |
| OP, 테이블 | `UPDATE contacts`. OP는 대문자로 적는다 |
| 사실들 | 쉼표로 잇는다. 없는 것은 뺀다. 아래 순서다 |
| 이유들 | 판단 기록의 `reasons`를 쉼표로 이어 대괄호에 넣는다. 비면 칸을 뺀다 |
| 문장 | 줄바꿈, 연속된 공백, 아래의 문자를 공백 하나로 바꾸고 양 끝을 다듬는다. 80자를 넘으면 79자와 `…`로 자른다 |

문장, 테이블 이름, 이유, 연쇄 테이블 이름은 에이전트나 DB가 쓴 글자다(규칙 5). 제어 문자(Unicode `Cc`), U+2028·U+2029, 방향 제어 문자(U+202A–202E, U+2066–2069)를 공백으로 바꿔 넣는다(P5). 처음 설계는 줄바꿈과 연속된 공백만 말했다.

사실들의 순서:

1. 행 수(P3). 실행된 쓰기의 실제 행 수(`rows_affected`)가 있으면 `300 rows`(하나면 `1 row`). 없으면 영향 측정에서 온다.
   - 측정이 상한에서 멈췄고 추정이 있으면 추정을 `~300 rows`로.
   - 측정이 상한에서 멈췄고 추정이 없으면 `101+ rows`. 이때는 비율을 뺀다.
   - 측정 방법이 추정(`estimate`)이면 `~300 rows`.
   - 그 밖의 세기(상한 아래의 정확한 세기)는 `40 rows`.
   - 측정한 행 수 없이 추정만 있으면 `~300 rows`.

   처음 설계의 세 갈래는 실행하지 않은 쓰기의 정확한 세기와, 상한에서 멈춘 세기에 함께 있는 추정을 다루지 못했다.
2. 테이블에서의 비율. 행 수가 0보다 크고 영향 측정에 테이블 행 수가 있고 0보다 크면 `~30% of table`. 통계에서 온 추정이라 늘 `~`를 붙인다. 반올림하고, 100%를 넘으면 100%로 적는다. 1% 미만은 `<1% of table`.
3. 연쇄. `cascades into deals, notes`. 같은 테이블은 한 번만 적는다.
4. 읽기 확인. 결과가 `fresh`면 `fresh read`, `stale`이면 `stale read`. 그 밖의 결과는 적지 않는다.
5. 실행. Stet이 보내지 않은 쓰기는 `refused ST001`(답한 SQLSTATE). 보냈지만 서버가 거부했거나 결과를 모르는 쓰기는 `not executed`.

**지켜야 할 것**

- 출력은 세션과 판단을 세우지 않는다. 줄은 1,024줄짜리 대기열을 거쳐 따로 쓴다. 대기열이 차면 새 줄을 버린다. 버린 것이 있는 동안에는 대기열에 자리가 나도 새 줄을 모두 버리고 센다. 그 앞에 들어간 줄이 다 나가면 버린 수를 한 줄로 알리고(`… 37 lines dropped`, 하나면 `… 1 line dropped`), 그 뒤로 다시 받는다. 알림이 계속 밀려 진짜 줄이 하나도 나가지 못하는 일을 막기 위해서다. 알림은 다음 줄이 오지 않아도 나온다.
- 쓰다가 실패해도 다음 줄을 계속 쓴다. 실패를 알릴 곳은 없다.
- 끝낼 때는 대기열에 남은 줄을 쓰되, 받는 쪽이 멈춰 있으면 2초까지만 기다린다(P6). 처음 설계는 끝낼 때를 말하지 않았고, 끝까지 기다리면 `stet`이 끝나지 않는다.
- 기록(`stet.decisions`)에 넣지 못한 판단도 줄은 낸다.
- 줄에 넣는 것은 판단 기록에 이미 있는 값뿐이다. 새로 DB에 묻지 않는다.
- 게이트는 터미널을 모른다. 게이트는 기록한 판단을 알려 주는 자리 하나(`Decided`)만 내주고, 줄을 만드는 것과 쓰는 것은 그 밖(`internal/lines`)에 둔다.
- **줄의 순서는 판단이 기록된 순서다.** 섀도에서는 쓰기가 실행된 뒤에 세션 밖에서 판단하므로, 잇달아 들어온 쓰기의 줄이 들어온 순서와 다르게 나올 수 있다. 줄의 시각은 쓰기가 들어온 시각이다. 그대로 둔다(제품의 동작이다). 테스트는 쓰기마다 그 줄을 기다린 뒤에 다음 쓰기를 보낸다.

**시작할 때 한 줄.** 줄 출력이 켜져 있으면, 받기 시작할 때 사람이 읽는 줄 하나를 표준 출력에 낸다: `stet: watching 12 tables on 127.0.0.1:6432 (shadow: nothing is blocked)`. 테이블이 하나면 `1 table`이다. enforce면 괄호 안이 `enforce`다. 주소는 실제로 받는 소켓의 주소다. `listen: 0.0.0.0:6432`면 `[::]:6432`로 나온다. 이 줄도 대기열을 거친다. 멈춘 출력에 바로 쓰면 시작이 서기 때문이다(1장의 "실패로 볼 것"). 그래서 대기열이 차 있으면 다른 줄처럼 버려질 수 있다. 로그는 표준 에러로 바로 가므로, 터미널에서 이 줄과 로그의 앞뒤는 바뀔 수 있다.

## 7. 지우는 법

새 명령은 없다. `init`의 안내와 문서가 문장을 알려 준다.

- 되돌리기(`undo`)를 쓰지 않았으면 `DROP SCHEMA stet CASCADE;` 하나다.
- 되돌리기를 썼으면 먼저 `stet undo-sql -drop`이 내는 문장으로 캡처 트리거를 지운다.

Stet은 이 문장을 스스로 실행하지 않는다.

## 8. 설치 산출물

- **`stet version`.** 버전, 커밋, Go 버전을 낸다: `stet 0.2.0 (commit 23b4da6d9e99, go1.27.3)`. 빌드할 때 넣고, 넣지 않은 빌드는 `dev`다. 커밋을 넣지 않으면 빌드 정보의 `vcs.revision`(12자)을 쓰고, 그것도 없으면 `unknown`이다.
- **`scripts/release.sh <버전>`.** 이 기기의 OS와 아키텍처용 묶음을 `dist/`에 만든다. 버전은 `0.2.0`처럼 앞에 `v` 없이 받는다. `stet_<버전>_<os>_<arch>.tar.gz` 안에는 `stet_<버전>_<os>_<arch>/` 디렉터리 하나가 있고, 거기에 `stet`, `LICENSE`, `README.md`, `stet.example.yaml`을 넣는다. `dist/checksums.txt`에 SHA-256을 더한다. 같은 묶음의 줄은 바꾸고 다른 줄은 둔다. 만든 파일의 경로를 한 줄로 낸다. SQL 파서가 C 코드라 cgo가 필요하고, 그래서 다른 OS용은 그 OS에서 만든다.
- **`Dockerfile`.** 여러 단계 빌드다. 빌드 단계는 `golang:1.27-bookworm`, 실행 이미지는 `gcr.io/distroless/base-debian12:nonroot`이다. 바이너리가 glibc에 링크되므로 두 단계의 Debian 판이 같다. 실행 이미지에는 셸이 없고, root가 아닌 사용자(65532)로 돈다. 진입점은 `stet`이고 `EXPOSE 6432`를 적는다. 버전과 커밋은 `--build-arg VERSION=…`, `COMMIT=…`로 넣는다.
- **`.github/workflows/release.yml`.** 손으로 돌리거나(`workflow_dispatch`) `v*` 태그에서 돈다. macOS(arm64, x86_64)와 Linux(amd64, arm64)의 러너에서 통합 테스트를 뺀 테스트와 `release.sh`를 돌리고 묶음을 워크플로의 산출물로 올린다. 릴리스를 만들거나 이미지를 올리지 않는다.
- **`dist/`는 git이 무시한다.**

**이 단계에서 하지 않는 것.** GitHub 릴리스 게시, brew 저장소와 포뮬러, 이미지 레지스트리에 올리기. 바깥에 공개하는 일이고, 모듈 경로의 `stetdb`가 아직 가칭이다(D12). 소유자가 정한 뒤에 한다.

## 9. 문서

- `docs/quickstart.md`(새 문서, 한국어): 설치(묶음, Docker, 소스), `init`, `proxy`, 에이전트 접속 문자열 바꾸기, 줄 읽는 법, 지우는 법, 한계(10장).
- `stet.example.yaml`: `upstream`의 TLS 파라미터, `send_fields`가 선택이 되는 조건, 루프백이 아닌 `listen`의 뜻.
- `docs/spec-v0.1.md` 9장(설정 형식): `send_fields`와 `upstream` 파라미터.
- `README.md`, `docs/concept-v0.2.md`, `docs/decisions.md`는 소유자의 문서다. 고치지 않고, 낡는 줄을 보고한다.

## 10. 한계

- **통제가 아니다.** 같은 기기의 에이전트는 진짜 접속 정보(설정 파일, 환경 변수)를 읽어 DB에 직접 붙을 수 있다. 이 길은 실수를 보여 주는 데까지다.
- **계정이 하나면 기록을 지킬 수 없다.** 에이전트가 Stet과 같은 계정을 쓰므로 `stet` 스키마를 읽고 고칠 수 있다. 섀도에서만 받아들인다. enforce가 요구하는 것(계정 분리, 역할 부여, `stet` 스키마 권한)은 그대로다(spec-v0.2-enforce 8장).
- **에이전트와 Stet 사이는 평문이다.** 같은 기기 안이라는 전제다. 루프백이 아닌 주소로 받으면 경고한다.
- **채널 바인딩을 요구하는 클라이언트는 붙지 못한다.** `sslmode=require`를 적은 클라이언트도 같다. 접속 문자열을 `init`이 알려 준 것으로 바꿔야 한다.
- **`require`와 `prefer`는 서버를 확인하지 않는다.** `prefer`는 중간에서 TLS를 거절당하면 평문으로 간다. libpq와 같은 뜻이다. 원격 DB에는 `verify-full`을 쓴다.
- **연결 풀러를 거친 주소는 지원하지 않는다.** Stet 자신의 연결은 시작 파라미터와 세션 상태를 쓴다. 풀러에 따라 접속이 거부되거나, 붙더라도 Stet의 읽기와 에이전트의 세션이 어긋날 수 있다. 직접 접속 주소를 쓴다.
- **주소에는 호스트와 포트를 하나만 적는다.** 호스트 여럿(장애 조치 목록), 질의 파라미터나 `PGHOST`·`PGPORT`·서비스 파일로 정한 호스트와 포트는 받지 않는다. Stet 자신의 연결과 에이전트의 세션이 다른 서버에 닿을 수 있기 때문이다(3장).
- **인증서로 하는 에이전트 인증은 지나가지 못한다.** 중계하는 세션에는 클라이언트 인증서를 내지 않는다.
- **스키마를 적은 이름과 `search_path` 밖의 테이블은 고르지 않는다.** 지금 Stet이 이름을 푸는 방식의 한계다.
- **실제 매니지드 서비스에서 확인하지 못했다.** 시험은 로컬의 TLS 서버로 한다. Neon·Supabase·RDS에서의 확인은 계정이 있어야 한다(12장).

## 11. 시험

- **시험 서버에 TLS를 켠다.** `scripts/testdb.sh`가 자기 서명 인증서(이름 `localhost`, 주소 `127.0.0.1`)를 만들고 `ssl=on`으로 띄운다. 인증서의 SAN은 `DNS:localhost,DNS:127.0.0.1,IP:127.0.0.1`이다. libpq 14는 IP SAN을 보지 않아, `psql`이 `verify-full`로 `host=127.0.0.1`에 붙으려면 같은 글자의 DNS 항목이 있어야 한다. 시험에만 쓰는 인증서다. 이미 만든 데이터 디렉터리에 인증서가 없으면 그때 만든다. 접속 규칙은 그대로 둔다. 평문과 TLS를 모두 받는다. 지금의 테스트는 바뀌지 않아야 한다.
- 테스트는 서버가 낸 인증서를 받아 그것을 루트로 쓴다. 새 환경 변수는 없다. 서버가 TLS를 받지 않으면 건너뛰지 않고 실패하며, 시험 서버를 다시 띄우라고 말한다.
- TLS를 거절하는 서버, 인증서가 다른 서버, SSLRequest에 엉뚱한 답을 하는 서버는 테스트 안의 가짜 리스너로 만든다.

확인할 것:

| 대상 | 확인 |
|---|---|
| `sslmode`별 접속 | 표의 줄마다. `require`인데 서버가 거절하면 `08001`. `verify-full`인데 이름이 다르면 `08001`. `prefer`인데 거절하면 평문으로 붙는다 |
| 인증 | 업스트림 TLS에서 SCRAM, md5, `password`, `trust` 계정이 모두 로그인한다. `psql`(libpq)과 pgx로 붙는다. 틀린 비밀번호는 서버의 에러가 그대로 온다 |
| 목록 고치기 | TLS 업스트림에서 클라이언트가 받는 목록에 `SCRAM-SHA-256-PLUS`가 없다. 평문 업스트림에서는 받은 바이트가 서버가 보낸 것과 같다 |
| 취소 | TLS 업스트림에서 `pg_sleep`을 취소한다 |
| 클라이언트 인증서 | `sslcert`가 있어도 중계하는 세션의 TLS 인사에 인증서가 없다 |
| 한 바이트 | SSLRequest의 답 뒤에 바이트를 붙여 보내는 가짜 서버에서 그 바이트가 TLS 인사 전에 읽히지 않는다 |
| 지연 | `latency_test.go`의 2ms 예산. TLS 업스트림의 줄을 더하고, 직접 TLS로 붙은 것과 비교한다 |
| `init` | 만든 설정으로 `stet proxy`가 경고 없이 뜬다. 고르는 조건의 줄마다. 100개 상한. 파일 권한. 덮어쓰기 거부. 터미널이 아닐 때의 `-yes`. DB에 아무것도 쓰지 않는다 |
| `send_fields` | 모델 없이 생략하면 뜨고, 기록의 상태 안의 `proposed`와 `current`가 비어 있다. 모델이 있으면 거부한다 |
| 한 줄 | 판정과 사실의 조합마다 줄의 글자. 대기열이 찼을 때 버리고 센다. 쓰는 쪽이 막혀도 쓰기가 끝난다 |
| 섀도 | 기존 섀도 테스트가 단언을 고치지 않고 통과한다 |
| 묶음과 이미지 | `release.sh`가 만든 묶음의 `stet version`. 이미지를 띄워 시험 서버에 붙는다 |

## 12. 수용 기준

2026-11-22에 확인했다. 테스트는 `STET_TEST_PG=127.0.0.1:55432`(PostgreSQL 14.18, TLS와 평문을 모두 받는 시험 서버)로 돌렸고, 건너뛴 통합 테스트는 없다. 테스트 이름 뒤의 괄호는 패키지다.

- [x] `sslmode`가 `require`, `verify-ca`, `verify-full`인 `upstream`으로 띄운 Stet을 거쳐 `psql`과 pgx가 SCRAM 계정으로 로그인하고 문장을 실행한다. 서버 쪽에서 그 세션이 TLS다(`pg_stat_ssl`).
  - `TestSessionsReachTheServerOverTLS`(cmd/stet): 세 `sslmode`마다 pgx(SCRAM 계정, md5 계정)와 `psql`(SCRAM 계정)이 붙어 `pg_stat_ssl.ssl`이 참이고, 에이전트의 쓰기가 실행되고 기록된다. 경고는 없다. `TestSessionOverTLSIsTLSOnTheServer`(internal/proxy).
- [x] 3장의 표와 절차의 줄마다 테스트가 있다(11장의 표).
  - `sslmode`별 접속: `TestUpstreamTLSBySSLMode`, `TestUpstreamRejectsAllowAndDirect`(internal/config). `TestUpstreamDeclinesTLS`(`Required`는 `08001`, `Preferred`는 평문으로 붙는다), `TestUpstreamCertificateIsChecked`(`verify-full`에서 이름이 다르면 `08001`), `TestOddTLSAnswerIsRefused`(internal/proxy).
  - 인증: `TestPsqlLogsInThroughTLSUpstream`(`psql`로 SCRAM, md5, `password`, `trust` 계정과 틀린 비밀번호), `TestPassthroughOverTLS/AuthMethodsPassThrough`(pgx로 같은 넷)(internal/proxy).
  - 목록 고치기: `TestSASLListOverTLSHasNoPlus`, `TestSASLListOverPlaintextIsTheServers`, `TestOnlyChannelBoundAuthIsRefused`, `TestStripPlus`, `TestAuthTapBecomesInner`, `TestAuthTapRefusesChannelBoundOnly`(internal/proxy).
  - 취소: `TestCancelOverTLSNeedsTLS`, `TestPassthroughOverTLS/CancelRequestReachesUpstream`(internal/proxy).
  - 클라이언트 인증서: `TestRelayedSessionShowsNoClientCertificate`(cmd/stet), `TestUpstreamTLSCarriesNoClientCertificate`(internal/config).
  - 한 바이트: `TestSSLAnswerIsReadAlone`(internal/proxy).
  - 한 대상: `TestOneTarget`, `TestParseRefusesAnUpstreamReadTwoWays`, `TestUpstreamErrorsNeverQuoteTheAddress`(internal/config).
  - 시작할 때의 경고: `TestTransportWarnings`, `TestTransportWarningsNameNoPassword`(cmd/stet).
  - 지연: `TestTLSUpstreamLatencyBudget`(internal/proxy).
- [x] 취소 요청이 TLS 업스트림에서 전달된다. `TestPassthroughOverTLS/CancelRequestReachesUpstream`이 TLS 업스트림에서 `pg_sleep(10)`을 취소한다. `TestCancelOverTLSNeedsTLS`는 취소 요청도 TLS로 가는 것을 본다.
- [x] 평문 업스트림에서 클라이언트가 받는 바이트는 이 단계 전과 같다. `TestSASLListOverPlaintextIsTheServers`(`Relayed`, `Watched`, `Enforced`)가 서버가 보낸 인증 바이트와 클라이언트가 받은 바이트가 같음을 본다. `TestPassthrough`, `TestWatchedPassthrough`는 단언을 고치지 않고 통과한다.
- [x] `stet init`이 만든 설정으로 `stet proxy`가 뜨고, 그 테이블들에 보낸 쓰기가 판단되어 기록된다. `init`은 DB에 아무것도 쓰지 않는다. `TestInitThenProxy`(경고 없이 뜨고, 쓰기 셋이 `commit`, `hold`, `hold`로 기록된다. `init` 뒤에 `stet` 스키마가 없다), `TestReadInitFactsWritesNothing`(읽기 전용 트랜잭션), `TestInitDeclinedWritesNothing`(cmd/stet). quickstart를 따라 한 결과도 같다(아래).
- [x] `init`이 알려 준 접속 문자열로 `psql`이 붙는다. `TestInitThenProxy`가 출력된 주소에 비밀번호만 넣어 `psql`로 쓴다. quickstart를 따라 할 때도 그 주소로 붙었다.
- [x] 계정 하나, 모델 없음, `send_fields` 없음으로 뜬다. `TestInitThenProxy`(한 계정이 `upstream`이자 에이전트), `TestRecordsWithoutSendFields`(cmd/stet, 상태 안의 `proposed`와 `current`가 비고 `proposed` 컬럼에 값이 있다), `TestSendFieldsOptionalWithoutAModel`(internal/config).
- [x] 판단한 쓰기마다 6장의 모양으로 한 줄이 나온다. 표준 출력이 터미널이 아니면 기본으로 나오지 않는다. `TestFormatLines`, `TestFormatCutsTheStatement`, `TestLineHasNoControlCharacters`(internal/lines), `TestDecisionLinePerJudgedWrite`, `TestEnforceLineShowsTheRefusal`, `TestLineWhenTheRecordFails`, `TestDecisionsFlag`(`auto`는 파일과 파이프에서 끈다), `TestRunWithoutOutWritesNothing`(cmd/stet).
- [x] 출력을 받는 쪽이 멈춰도 에이전트의 쓰기가 끝난다. `TestBlockedOutputDoesNotHoldWrites`(cmd/stet, 멈춘 출력 뒤에서 쓰기 2,000개가 끝나고 기록된다), `TestOfferNeverBlocks`, `TestCloseGivesUpOnABlockedWriter`(internal/lines).
- [x] 기존 테스트가 단언을 고치지 않고 통과한다. 고친 것이 있으면 이유와 함께 적는다. 2026-11-22의 전체 실행(`STET_TEST_PG=127.0.0.1:55432 go test -race ./...`)에서 테스트가 있는 19개 패키지가 모두 `ok`다. 고친 것:
  - `TestTablesNeedSendFields`(internal/config): 설정을 `adapter: none`에서 버전을 고정한 `jev` 모델로 바꿨다. 단언은 그대로다. 5장이 모델 없는 설정의 `send_fields`를 선택으로 만들었으므로, 예전 설정은 이 단계가 없앤 동작을 단언하고 있었다.
  - 테스트의 헬퍼만 고쳤고 단언은 그대로다: `proxy.New`의 새 시그니처에 맞춘 헬퍼, 이름이 겹친 `decisionsOn`을 `decisionsRecorded`로 바꾼 것(`history_test.go`, `undo_test.go`), `start`와 `runPsql`에서 `startOn`과 `psqlWith`를 떼어 낸 것(cmd/stet), `latency_test.go`의 측정 고리를 `measureAdded`로 떼어 낸 것(기존 `Test…` 함수의 본문과 단언은 그대로다).
  - `sslmode`를 적지 않은 기존 테스트의 세션은 이제 TLS 업스트림을 지난다(3장). 단언은 고치지 않았다.
- [x] `latency_test.go`의 예산을 지킨다. TLS 업스트림의 값을 적는다. `TestTLSUpstreamLatencyBudget`: `select 1` 2,000회 왕복에서 더해진 p95가 31.4µs와 31.9µs(Task 4, `-race`), 32.1µs(2026-11-22 다시 잼, `-race`)다. `-race` 없이는 19.8µs다. 예산은 2ms다.
- [x] `scripts/release.sh`가 이 기기용 묶음을 만들고, 그 안의 `stet version`이 준 버전을 낸다. darwin/arm64에서 `scripts/release.sh 0.2.0-test`가 `dist/stet_0.2.0-test_darwin_arm64.tar.gz`를 만들고, `shasum -a 256 --ignore-missing -c checksums.txt`가 `OK`, 묶음 안의 `stet version`이 `stet 0.2.0-test (commit 23b4da6d9e99, go1.27.3)`다(Task 8에서는 `fd9c72035c2f`). 묶음에는 `LICENSE`, `README.md`, `stet`, `stet.example.yaml`이 있다. `TestVersionCommand`(cmd/stet).
- [x] Docker 이미지가 빌드되고, 그 이미지로 띄운 Stet을 거쳐 쓰기가 판단된다. Task 8: 이미지 사용자 `65532`, 시험 서버의 테이블에 보낸 쓰기가 `09:57:39  ok            UPDATE task8_probe  1 row  update task8_probe set v='z' where id=1`로 나오고 기록됐다. 2026-11-22: quickstart 4장의 명령 그대로 컨테이너에서 `init`(`host.docker.internal`이라 `require`로 낮추고 알렸다)과 `proxy -decisions on`을 돌려 `11:40:35  ok            UPDATE contacts  1 row, fresh read  update contacts set status = 'customer' where id = 7`을 봤다. 이미지와 컨테이너는 지웠다.
- [x] `docs/quickstart.md`의 명령을 처음부터 끝까지 그대로 따라 해서 1장의 예와 같은 줄을 본다(처음 설계의 문장은 "2장의 예"였으나 예는 1장에 있다). 2026-11-22: 묶음을 빈 디렉터리에 풀고 시험 서버에 데이터베이스와 계정을 하나씩 만들어 2·7·8·9장의 명령을 그대로 쳤다. 1장의 세 줄과 시각만 다른 세 줄이 나왔다(1장의 예가 그 줄이다). `stet init`은 표준 입력이 터미널일 때 묻고 `y`에 썼다. 지운 뒤 데이터베이스에는 `public` 스키마만 남았다.
- [ ] (이 단계에서 확인하지 못한다) 릴리스 워크플로가 네 대상에서 묶음을 만든다. 저장소에 올린 뒤 손으로 돌려야 알 수 있다. 이 단계는 저장소에 올리지 않는다. 확인한 것은 YAML이 읽히고, 대상이 `macos-15`, `macos-15-intel`, `ubuntu-24.04`, `ubuntu-24.04-arm`이고, `workflow_dispatch`와 `v*` 태그에서 돈다는 것까지다(Task 8).
- [ ] (계정이 있어야 한다) Neon, Supabase, RDS 가운데 하나에서 `init`부터 줄을 보기까지 된다. 계정이 없어 하지 못했다. 원격 호스트로 가는 `init`의 길(`verify-full`을 해 보고 `require`로 낮추며 알리는 것)은 컨테이너에서 `host.docker.internal`로 로컬 시험 서버에 붙어서만 봤다.

## 13. 정한 것과 남긴 것

**정한 것**

- 6단계를 둘로 나누고, 써 보는 길을 먼저 한다. 서버 배포는 그 뒤다.
- 제품을 둘로 만들지 않는다. 같은 바이너리를 두 자리에 놓는다.
- 써 보는 길의 기본은 섀도, 모델 없음, 계정 하나다.
- 기록은 대상 DB의 `stet` 스키마에 둔다(가).
- 에이전트와 Stet 사이는 평문으로 둔다.

**구현하면서 정한 것** (해당 장에 옮겨 적었다)

- P1(3장): TLS를 요구하는 설정에서 서버가 거절할 때의 문구는 `stet: upstream does not accept TLS, and the sslmode of upstream needs it`다.
- P2(1·6장): 모든 칸 사이는 공백 둘이고 판정 칸만 12글자로 채운다. 1장의 예를 실제 줄로 바꿨다.
- P3(6장): 행 수의 갈래를 실행하지 않은 쓰기의 정확한 세기와 상한에서 멈춘 세기의 추정까지 넓혔다.
- P4(4장): 파티션은 고르지도 세지도 않는다. 키 컬럼 이름도 테이블 이름과 같은 글자 조건을 본다.
- P5(6장): 제어 문자, U+2028·U+2029, 방향 제어 문자를 공백으로 바꾼다. 문장 말고 테이블 이름, 이유, 연쇄 테이블 이름에도 한다.
- P6(6장): 끝낼 때 줄은 2초까지만 기다린다.
- P7(4·6장): 터미널인지는 문자 장치인지로 본다.
- P8(4장): 에이전트 주소에서 지우는 파라미터의 목록. 진행 중에 `host`, `hostaddr`, `port`를 더했고(에이전트를 Stet을 건너 서버로 보낸다), `password` 파라미터를 가리게 했다.
- 3장: Stet 자신의 연결과 프록시 구간이 다른 서버에 닿을 수 있는 `upstream`은 거부한다(`config.OneTarget`). "호스트가 여럿이면 첫 호스트만 쓴다"를 대신한다. Stet이 만든 `upstream`의 에러는 주소를 인용하지 않는다. pgconn이 읽지 못하는 주소는 pgconn의 에러가 비밀번호를 `xxxxx`로 가린 채 인용한다(pgx는 best effort라고 적는다).
- 3장: `sslmode`를 적지 않은 설정의 프록시 구간도 TLS를 먼저 청한다.
- 4장: `init`은 서버가 보낸 글자의 제어 문자를 공백으로 바꿔 보인다. 호스트가 없거나, 공백이 있거나, 질의에 `host`·`hostaddr`·`port`가 있거나, 호스트가 여럿인 주소는 붙기 전에 거부한다. 한 번 붙어 보는 데 15초, 읽는 데 30초까지 기다린다. 쓰기 전에 그 파일을 `config.Parse`에 넣어 본다. PostgreSQL 14에서 `ANALYZE`하지 않은 파티션 부모는 100개 상한에서 빠질 수 있고, 그대로 둔다.
- 6장: 줄의 시각은 쓰기가 들어온 시각이고, 줄은 판단이 기록된 순서로 나온다. 시작 줄도 대기열을 거친다. 대기열이 넘치면 빌 때까지 새 줄을 버린 뒤 버린 수를 알린다.
- 8장: 묶음 안의 디렉터리 하나, 실행 이미지 `gcr.io/distroless/base-debian12:nonroot`.
- 11장: 시험 인증서에 `DNS:127.0.0.1`을 더했다(libpq 14).

**남긴 것**

- 서버 배포: 클라이언트 쪽 TLS, Stet이 에이전트를 인증하는 것, 자격 증명 보관, 가용성.
- 흔적 없는 체험(나).
- 공개: 릴리스 게시, brew, 이미지 레지스트리, 조직 이름.
- 무료와 유료의 경계. 엔진은 제한 없이 열고 여러 대의 관리와 사람이 보는 화면에서 받는다는 방향만 이야기했고, 정하지 않았다.
- 세션의 `search_path`를 따라가는 것, 스키마를 적은 테이블 이름.
- 연결 풀러 지원.
- PostgreSQL 14보다 낮은 버전.
- Windows.
