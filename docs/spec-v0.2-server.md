# Stet v0.2 6단계 둘째 부분 스펙: 서버에 놓기

상태: 구현 (2026-11-23). 설계는 2026-11-22에 했고, 구현은 [plan-v0.2-server](plan-v0.2-server.md)의 Task 1~9이다. 구현하면서 정한 것(계획의 스펙 보정 V1~V7과 진행 중의 판단)은 해당 장에 옮겨 적었고, 15장에 모았다. 근거는 concept-v0.2 5장("에이전트용 자격 증명이 Stet을 거쳐야만 통하게 만든다")과 9장의 6번, D21, D30, [spec-v0.2-tryout](spec-v0.2-tryout.md) 2장의 "다"와 13장의 "남긴 것", 그리고 2026-11-22의 논의다. 놓는 순서는 [deploy](deploy.md)에 있다.

6번 단계의 첫 부분은 Stet을 개인 PC에 놓고 실수를 보여 주는 데까지였다. 이 문서는 둘째 부분이다. Stet을 서버에 놓고, 에이전트가 Stet을 거쳐야만 DB에 닿게 한다. 가용성은 이 문서의 범위가 아니다. 셋째 부분으로 뗀다.

## 1. 목표와 비목표

지금 Stet은 인증을 중계한다. 에이전트는 DB의 진짜 비밀번호를 알고, 그 비밀번호로 DB에 직접 붙을 수 있다. 직접 운영하는 Postgres는 접속 규칙(`pg_hba.conf`)으로 그 길을 막을 수 있지만, 매니지드 Postgres는 대개 그럴 수 없다. 그리고 에이전트와 Stet 사이는 평문이어서 Stet을 다른 기기에 놓을 수 없다.

**누구를 위한 것인가.** 에이전트 여럿을 매니지드 Postgres(Neon, Supabase, RDS 등)에 붙여 쓰는 팀의 운영자다. 에이전트마다 DB 계정이 따로 있고, Stet을 서버 한 대에 띄운다.

**무엇을 얻는가.** 에이전트는 Stet이 내준 토큰만 안다. 진짜 비밀번호는 Stet만 쥔다. 그래서 에이전트는 Stet을 건너뛸 수 없고, 에이전트와 Stet 사이도 암호화된다.

```
$ stet agent add agent_enricher        # 진짜 비밀번호를 묻고, 토큰을 한 번 보여 준다
Database password for "agent_enricher":
token for "agent_enricher" (shown once; stet keeps only what it needs to check it):
  stet_pZ3…

에이전트의 접속 문자열:
  postgres://agent_enricher:stet_pZ3…@stet.internal:6432/crm?sslmode=verify-full&sslrootcert=stet.crt
```

처음 설계의 예는 토큰 줄을 `token: stet_pZ3…  (shown once)`로 적었다. 실제 출력(7장)으로 바꿨다.

**목표**

- 에이전트와 Stet 사이 구간에 TLS를 받는다.
- 에이전트를 Stet이 직접 인증한다. 에이전트는 계정 이름과 토큰으로 로그인한다.
- Stet이 그 계정의 진짜 자격 증명으로 DB에 로그인하고, 그 세션을 에이전트에게 잇는다. 그 뒤는 지금처럼 중계하고 판단한다.
- 진짜 비밀번호와 토큰의 검증값을 두는 파일, 그것을 다루는 명령(`stet agent`)을 둔다.
- 기존 클라이언트를 고치지 않는다. 접속 문자열의 호스트와 비밀번호만 바꾼다.

**비목표**

- 가용성, 여러 인스턴스. 셋째 부분이다.
- Stet이 DB 계정의 비밀번호를 스스로 만들고 돌리는 것, 비밀번호 대신 유도한 키만 두는 것(2장의 2와 3).
- 바깥 비밀 보관소(Vault, AWS Secrets Manager 등), 클라이언트 인증서로 하는 에이전트 인증.
- DB 계정과 다른 Stet만의 신원. 신원은 접속 계정이다(D21).
- 로그인 횟수 제한, 로그인 기록 테이블.
- 판정을 바꾸는 것. 게이트의 규칙과 기록은 그대로다.
- 복제 접속, GSS, PostgreSQL 17의 바로 TLS(`sslnegotiation=direct`).

**실패로 볼 것.**

- 토큰이나 진짜 비밀번호가 로그, 에러, 표준 출력, 판단 기록에 나오는 것. 나가는 곳은 자격 증명 파일과, 토큰을 만든 순간의 표준 출력뿐이다.
- 토큰 없이, 또는 틀린 토큰으로 DB 세션을 얻는 것. 한 계정의 토큰으로 다른 계정의 세션을 얻는 것.
- 진짜 비밀번호를 확인되지 않은 서버에 내주는 것(8장).
- 보관을 켠 Stet에서, 섀도든 enforce든 에이전트의 문장이나 그 결과가 직접 붙었을 때와 달라지는 것(규칙 6). 로그인 절차는 문장이 아니다.
- 로그인 뒤의 중계가 지금보다 느려지는 것. 지연 예산(2ms)은 그대로다.

## 2. 접근

**무엇을 먼저 만드나.** 세 가지를 놓고 "매니지드 Postgres부터"를 골랐다(2026-11-22).

| | 내용 | 대가 |
|---|---|---|
| **매니지드 Postgres부터** (고름) | 에이전트는 토큰으로 Stet에 로그인하고 Stet이 진짜 자격 증명으로 DB에 로그인한다. 양쪽이 TLS다 | 가장 크다. Stet이 인증 서버이자 비밀 보관소가 된다 |
| 직접 운영하는 Postgres부터 | 클라이언트 쪽 TLS와 접속 규칙 안내. 인증은 중계한다 | 업스트림이 평문이어야 해서 매니지드 사용자는 쓸 수 없다 |
| 가용성부터 | 재시작과 장애 넘김 | 통제는 그대로 뚫려 있다 |

고른 이유: 첫 부분의 사용자가 대개 매니지드 Postgres에 있다. 그리고 이것을 만들면 둘째 줄은 그 일부로 따라온다(3장의 TLS는 보관 없이도 켤 수 있다).

**어떻게 만드나.** 세 가지를 놓고 1을 골랐다(2026-11-22).

| | 내용 | 대가 |
|---|---|---|
| **1. 운영자가 준 비밀번호를 파일에 둔다** (고름) | 진짜 비밀번호는 Stet 서버의 파일이나 환경 변수에 있다. Stet은 토큰의 검증값만 둔다. DB 로그인은 pgx가 한다 | 진짜 비밀번호가 Stet 서버에 평문으로 있다. 운영자도 그 값을 안다. 돌리는 일은 운영자가 한다 |
| 2. Stet이 비밀번호를 만든다 | Stet이 관리 계정으로 에이전트 계정의 비밀번호를 무작위 값으로 바꾼다 | Stet이 역할을 고칠 권한을 쥔다. 서비스마다 권한 모델이 다르다 |
| 3. 로그인 키만 둔다 | SCRAM에서 유도한 키만 저장한다 | DB 쪽 로그인을 직접 써야 한다. 줄어드는 위험은 다른 곳에서의 재사용뿐이다 |

고른 이유: 가장 작고, 가장 민감한 부분(DB 로그인)을 검증된 코드에 맡긴다. 2는 1 위에 명령 하나를 더하는 일이라 뒤에 얹을 수 있다.

**확인한 것** (2026-11-22, 시험 서버에서. 실험 코드는 지웠다)

- pgx(`pgconn`)로 DB에 로그인한 연결을 넘겨받을 수 있다(`Hijack`). TLS와 인증서 확인이 끝난 연결, 서버 프로세스 번호와 취소 키, 서버가 알린 세션 설정 13개를 함께 준다.
- 그 연결을 평문 클라이언트에 이어 주면 된다. 아무 계정 이름과 틀린 비밀번호로 붙은 `psql`이 `stet_scram`의 세션을 얻었고, 서버 쪽에서 그 세션은 TLS였다. 시작 파라미터(`application_name`, `options`의 `statement_timeout`)가 전달됐고, `Ctrl-C`의 취소 요청이 실행 중인 문장을 멈췄다.
- pgx에는 서버 쪽 SCRAM이 없다. 에이전트를 인증하는 쪽은 새로 쓴다.
- 양쪽 구간이 TLS면 SCRAM 인증을 중계할 수 없다는 것은 첫 부분에서 확인했다(spec-v0.2-tryout 2장).

## 3. 클라이언트 쪽 TLS

```yaml
listen: 0.0.0.0:6432
listen_tls:
  cert: /etc/stet/tls/server.crt
  key: /etc/stet/tls/server.key
```

- `listen_tls`가 있으면 클라이언트의 SSLRequest에 `S`로 답하고 TLS 인사를 한다. 없으면 지금처럼 `N`으로 답한다. GSSENCRequest에는 늘 `N`이다. TLS 안에서 온 두 번째 SSLRequest에는 답하지 않고 접속을 끝낸다. SSLRequest 뒤에 붙어 온 평문은 TLS 안의 것으로 읽지 않는다(요청만 읽고 `S`를 보낸 뒤 인사를 한다).
- 인증서와 키는 운영자가 준다. PEM 파일이다. 인증서 파일에는 체인을 함께 넣을 수 있다. TLS 1.2 이상만 받는다.
- `cert`와 `key`의 상대 경로는 설정 파일이 있는 디렉터리에 대해 읽는다(`config.Load`. `config.Parse`는 적힌 그대로 둔다. 보정 V2). `~`는 풀지 않는다.
- 시작할 때(`stet proxy`) 두 파일을 읽지 못하거나 짝이 맞지 않으면 시작하지 않는다(`listen_tls: …`). 설정 검증은 두 키가 다 있는지만 본다(`listen_tls: cert and key are both required`). 키 파일을 소유자 말고 다른 사용자가 읽을 수 있으면 경고한다(`listen_tls.key <경로> gives users other than its owner access (mode 0640): whoever can read it can pose as stet to the agents; fix: chmod 600 <경로>`).
- 파일이 바뀌면(바뀐 시각, 크기, 권한, 파일 자체) 다음 인사에서 다시 읽고 그 인사부터 새 인증서를 쓴다(INFO `read listen_tls again; serving the new certificate from the next handshake`). 다시 읽지 못하면 쓰던 것을 계속 쓰고 에러 로그(`could not read listen_tls again; serving the certificate in use`)를 남긴다. 바뀔 때마다의 실패를 남기고, 파일을 볼 수 없는(stat이 실패하는) 동안은 한 번만 남긴다. 반쯤 쓰인 짝(인증서만 바뀌고 키는 아직)도 쓰던 인증서를 계속 쓴다. 인증서를 갈 때 Stet을 다시 띄우지 않기 위해서다.
- `listen_tls`가 있을 때, 루프백이 아닌 주소에서 온 접속이 TLS 없이 세션을 시작하려 하면 `FATAL`(SQLSTATE `28000`, `stet: this server requires TLS; connect with sslmode=require or stronger`)로 끝낸다. 업스트림에는 아무것도 보내지 않는다. 루프백에서 온 접속은 평문을 받는다. 루프백인지는 접속의 상대 주소로 본다. 그래서 같은 기기의 TCP 전달자(사이드카 프록시, `ssh -L`, `kubectl port-forward`)를 Stet 앞에 두면 모든 클라이언트가 루프백으로 보여 평문도 받는다(12장). 취소 요청은 평문으로도 받는다(libpq 16까지는 취소를 늘 평문으로 보낸다).
- 인사에는 접속의 시작과 같은 시간 제한(10초)이 걸린다. 그사이 멈춘 클라이언트는 다른 접속을 막지 않는다.
- `listen_tls`가 있으면 루프백이 아닌 `listen`에 대한 "암호화되지 않는다" 경고(spec-v0.2-tryout 3장)는 나오지 않는다.

**보관 없이 켤 때.** `listen_tls`는 자격 증명 보관(4장) 없이도 켤 수 있다. 그때 인증은 지금처럼 중계한다. 업스트림도 TLS면 SCRAM 계정은 기본 설정의 클라이언트로 로그인하지 못한다. 클라이언트가 채널 바인딩을 끄면(`channel_binding=disable`) 된다. libpq와 pgx는 그때 묶지 않는다는 표시(`n`)를 보내고 서버는 그것을 받는다. 설계할 때는 PostgreSQL 14와 17, pgx 5.11의 소스에서 읽기만 했고, 구현할 때 돌려 확인했다(`TestRelayWithTLSOnBothSides`): 기본 설정의 `psql`과 pgx의 SCRAM 로그인은 서버가 `28000` `SCRAM channel binding check failed`로 거부하고, `channel_binding=disable`이면 로그인한다. md5와 `password` 계정은 로그인한다. 그래서 시작할 때 경고한다(`listen_tls is set without agents.credentials and the upstream's sslmode asks for TLS: SCRAM accounts cannot log in through stet with TLS on both sides unless the client sets channel_binding=disable; fix: add agents.credentials, or reach upstream without TLS on a private network`). 업스트림의 `sslmode`가 `disable`이 아니면(`prefer`와 적지 않은 것도) 경고한다. 이 조합에서는 인증 방식 목록을 고치지 않는다. 클라이언트는 서버가 보낸 목록(`SCRAM-SHA-256-PLUS` 포함)을 받는다. 첫 부분의 목록 고치기는 클라이언트 쪽이 평문일 때만 한다.

**로그.** 보관을 켠 Stet은 TLS 없이 거절한 접속을 `agent login refused`(WARN, `reason="tls required"`)로 남긴다(9장). 보관 없이 켠 Stet에서는 디버그(`startup failed`)로만 남는다.

## 4. 에이전트의 로그인

자격 증명 보관은 `agents.credentials`(6장)를 적으면 켜진다. 켜면 Stet은 인증을 중계하지 않고 직접 한다.

**절차.** 클라이언트의 시작 패킷을 받은 뒤다.

1. 시작 패킷의 `user`를 접속 계정으로 읽는다(지금 읽는 방식 그대로다. spec-v0.2-enforce 8장).
2. Stet이 AuthenticationSASL로 `SCRAM-SHA-256` 하나를 제안하고, 그 계정의 토큰 검증값으로 SCRAM 교환을 한다(RFC 5802, 7677. PostgreSQL 서버가 하는 것과 같다).
3. 클라이언트의 증명이 맞으면 Stet이 업스트림에 로그인한다(5장).
4. 업스트림 로그인이 되면 클라이언트에 AuthenticationOk, 세션 설정들, 서버 프로세스 번호와 취소 키, ReadyForQuery를 보낸다. 그 뒤는 지금의 중계다.

**토큰.** 무작위 32바이트를 글자로 적은 것이고 `stet_`로 시작한다(`stet_` 뒤에 base64url 43글자). ASCII뿐이다. Stet은 토큰을 저장하지 않고 검증값만 둔다. 검증값은 PostgreSQL이 비밀번호를 두는 모양과 같다(`SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>`). 반복 수는 4096이다.

**거절.** 클라이언트가 받는 것은 PostgreSQL이 주는 것과 같다.

| 경우 | 클라이언트가 받는 것 |
|---|---|
| 모르는 계정, 틀린 토큰, 지운 계정 | `FATAL` `28P01` `password authentication failed for user "<계정>"` |
| 시작 패킷을 읽지 못함, `user` 없음 | 지금과 같다(접속을 끝낸다) |
| `replication` 파라미터가 있는 시작 패킷 | `FATAL` `0A000` `stet: replication connections are not supported`(인증 전에) |
| SASL이 아닌 답, 모르는 방식, 깨진 메시지, 16KiB를 넘는 메시지 | `FATAL` `28000` `stet: authentication exchange failed` |
| 업스트림에 로그인하지 못함 | `FATAL` `08001` `stet: could not open a database session for "<계정>"` |
| 루프백이 아닌 주소에서 TLS 없이(3장) | `FATAL` `28000` `stet: this server requires TLS; connect with sslmode=require or stronger` |

진짜 비밀번호를 토큰 자리에 넣은 것과 한 계정의 토큰으로 다른 계정 이름을 댄 것도 첫 줄이다.

- 모르는 계정에도 같은 교환을 끝까지 한다. 계정 이름과 비밀값에서 만든 가짜 검증값을 쓴다. 메시지의 수와 모양으로 계정이 있는지 알 수 없어야 한다.
- **가짜 검증값의 비밀값은 자격 증명 파일에 둔다**(보정 V6). 처음 설계는 "프로세스마다 정한 비밀값"이었다. 그러면 다시 띄울 때 가짜 검증값의 salt가 바뀌고 진짜 계정의 salt는 그대로라서, 다시 띄우기 전후로 같은 이름을 대 보면 계정이 있는지 알 수 있다. 그래서 파일의 맨 위 키 `mock_secret`(무작위 32바이트, base64)을 쓴다(6장). 파일에 없으면 프로세스마다 무작위 값을 쓰고 시작할 때 경고한다. 가짜 검증값은 비밀값과 계정 이름의 HMAC에서 만든다.
- 증명의 비교는 걸리는 시간이 값에 따라 달라지지 않는 방식으로 한다.
- 업스트림 로그인이 실패한 이유(진짜 비밀번호가 틀림, 서버에 닿지 못함, 인증서 확인 실패)는 Stet의 로그에만 남긴다. 에이전트는 믿지 않는 상대다.
- 채널 바인딩(`SCRAM-SHA-256-PLUS`)은 제안하지 않는다. `channel_binding=require`를 적은 클라이언트는 붙지 못한다(12장. `psql`은 `channel binding is required, but server did not offer an authentication method that supports channel binding`으로 끝낸다).
- 첫 메시지(SASLInitialResponse)에 클라이언트의 첫 SCRAM 메시지가 없으면(길이 -1) `28000`이다. 이 단계가 받는 클라이언트는 늘 그것을 보낸다.
- **인증 메시지의 크기.** 인증 동안 클라이언트의 메시지는 본문 16KiB까지만 받는다. 넘으면 본문을 읽기 전에 `28000`으로 끝낸다. 메시지는 버퍼 없이 읽는다. 인증 뒤에 클라이언트가 보낸 바이트를 미리 읽어 중계에서 빼앗지 않기 위해서다(보정 V4).
- **프로토콜 버전.** Stet은 3.0으로 답한다. 시작 패킷이 3.0보다 높은 부 버전을 청하거나 프로토콜 옵션(`_pq_.`로 시작하는 파라미터)을 보내면, 인증 전에 NegotiateProtocolVersion(3.0과 모르는 옵션들)을 보낸다. 프로토콜 옵션은 업스트림에 전하지 않는다.
- 로그인 전체(TLS 인사부터 ReadyForQuery까지)에 시간 제한을 둔다. 클라이언트와의 교환은 10초, 업스트림 로그인은 5초다. 교환의 10초는 시작 패킷을 다 받은 때부터 따로 잰다. 시작 패킷과 TLS 인사의 10초가 그 앞에 있으므로, 한 접속이 로그인에 쓰는 시간은 모두 합쳐 25초 남짓까지다. 교환이 10초를 넘기면 클라이언트에 `FATAL`을 보내지 않고 끝내고 `exchange failed`로 남긴다(9장).
- 프록시를 닫으면(`Server.Close`) 진행 중인 업스트림 로그인을 취소한다. 5초를 기다리지 않는다(보정 V5).

**목록에 없는 계정.** 보관을 켠 Stet은 자격 증명 파일에 있는 계정만 받는다. 지금은 `agents.roles`에 없는 계정의 세션을 그대로 통과시키지만, 보관을 켜면 그런 세션은 없다. 서비스와 사람은 지금처럼 DB에 직접 붙는다.

## 5. 업스트림 로그인과 세션 잇기

- Stet은 `upstream` 주소의 호스트, 포트, TLS 설정으로 업스트림에 붙고, 접속 계정과 그 계정의 진짜 비밀번호로 로그인한다. 로그인은 pgx(`pgconn`)가 한다. 서버가 요구하는 방식(SCRAM, 채널 바인딩 포함, md5, `password`)을 pgx가 따른다.
- 데이터베이스는 클라이언트의 시작 패킷에 적힌 것이다(지금과 같다).
- 클라이언트의 시작 파라미터는 `user`, `database`, `replication`을 빼고 그대로 업스트림의 시작 패킷에 넣는다(`application_name`, `client_encoding`, `options` 등).
- `upstream`의 클라이언트 인증서(`sslcert`, `sslkey`)는 이 로그인에 쓰지 않는다(첫 부분과 같다). Stet 자신의 연결에만 쓴다.
- `PGPASSWORD`와 `.pgpass`의 비밀번호는 이 로그인에 쓰이지 않는다. 자격 증명 파일이 정한 값(`password`의 값, 또는 `password_env`가 가리키는 변수를 시작할 때 읽은 값)만 쓴다. 값이 비어 있으면 로그인하지 않는다.
- **프로토콜 3.0으로 고정한다.** 주소나 환경(`PGMAXPROTOCOLVERSION` 등)이 무엇을 청하든 업스트림 로그인은 3.0으로 한다. 3.2 세션의 취소 키는 3.0 클라이언트가 읽지 못하는 길이이기 때문이다.
- `upstream` 주소는 Stet 자신의 연결 풀이 읽는 대로(`pgxpool.ParseConfig`) 읽는다. 그래서 pgx와 pgxpool의 키(`pool_max_conns` 등)는 시작 파라미터로 보내지 않는다. 시작할 때와 `stet agent`의 확인 로그인(8장, 7장)은 주소에 적힌 실행 파라미터(`application_name`, `options`)를 보낸다. Stet 자신의 풀이 그 위에 더하는 설정은 보내지 않는다.
- **`prefer`의 평문 되돌아가기.** 업스트림이 `sslmode=prefer`(또는 적지 않음)면, 이 로그인은 pgconn을 따라 TLS 시도가 어떤 이유로든 실패하면 평문으로 다시 붙는다. 중계하는 구간은 서버가 `N`으로 답할 때만 평문으로 간다(spec-v0.2-tryout 3장). 보관을 켜면 원격 업스트림은 서버 확인이 필수라 되돌아가기가 없으므로(8장), 이것은 루프백 업스트림에서만 일어난다.
- 로그인이 끝나면 연결을 넘겨받아(`Hijack`) 지금의 중계에 넣는다. 넘겨받을 때 읽지 않은 바이트가 남아 있으면 그것부터 클라이언트에 보낸다.

**클라이언트가 받는 것.** AuthenticationSASLFinal 뒤에 AuthenticationOk, 서버가 알린 세션 설정(ParameterStatus, 이름 순서), BackendKeyData(서버의 진짜 프로세스 번호와 취소 키), ReadyForQuery다. 로그인 동안 서버가 보낸 알림(NoticeResponse)은 전하지 않는다. 직접 붙었을 때와 다른 것은 세션 설정의 순서와 그 알림뿐이다.

**잇는 방식**(보정 V3). 프록시가 위의 메시지들(AuthenticationOk부터 ReadyForQuery까지)을 만들어 넘겨받은 업스트림 연결의 앞머리로 붙이고, 그 연결을 지금의 중계에 넣는다. 세션은 그것을 서버가 보낸 것으로 읽고, 서버 프로세스 번호, 첫 ReadyForQuery, 세션 설정(enforce에서)을 거기서 얻는다. 세션의 코드는 바꾸지 않았다.

**취소 요청.** 지금과 같다. 클라이언트가 받은 키가 서버의 진짜 키이므로 그대로 업스트림에 전한다. 인증이 없다(PostgreSQL과 같다).

**세션과 게이트.** 게이트는 이 세션을 처음부터 인증된 세션으로 본다. 접속 계정은 Stet이 확인했고, 서버도 그 계정으로 인증했다. 서버 프로세스 번호와 세션 설정(시간대, 날짜 형식)은 넘겨받은 값에서 채운다. 그 뒤의 판단, 기록, 한도, 낡은 쓰기 탐지, 되돌리기는 바뀌지 않는다. `stet.decisions`의 `login_role`은 그 계정이다.

**Stet 자신의 연결과 승인된 쓰기의 실행.** 바뀌지 않는다. Stet은 `upstream`의 계정으로 붙고, 승인된 쓰기는 지금처럼 그 연결에서 에이전트 계정으로 바꿔 실행한다. 여러 행에 쓰는 쓰기의 영향 측정도 지금처럼 에이전트의 역할로 하므로, Stet의 계정은 에이전트 계정마다 그 역할의 구성원이어야 한다(`GRANT agent_x TO stet`. spec-v0.2-enforce 8장). 섀도에서도 그렇다. 빠지면 그 쓰기는 `unsupported`(`not_single_row`)로 기록되고, 보관을 켠 Stet은 섀도에서도 시작할 때 경고한다(8장). 보관을 켜면 Stet의 계정과 에이전트 계정이 반드시 다르므로(8장), 계정 하나로 도는 quickstart의 길에서 옮겨 오면 이 부여가 새로 필요하다. deploy를 따라 하다 찾았다(14장). 이 부여가 Stet의 계정에 주는 것과 그 비밀번호의 무게는 12장에 있다.

## 6. 자격 증명 파일

```yaml
agents:
  credentials: /etc/stet/agents.yaml
```

```yaml
# Written by stet agent. It holds database passwords: keep it private.
mock_secret: <무작위 32바이트, base64>
agents:
    agent_enricher:
        token: SCRAM-SHA-256$4096:Wm9v…$b1Zk…:Q0hl…
        password: the real password
        added: 2026-11-22T13:02:11Z
        token_issued: 2026-11-22T13:02:11Z
    agent_support:
        token: SCRAM-SHA-256$4096:…
        password_env: AGENT_SUPPORT_PASSWORD
        added: 2026-11-22T13:05:40Z
        token_issued: 2026-11-22T13:05:40Z
```

`stet agent`가 쓰는 모양이다(계정은 이름 순서, 들여쓰기 네 칸). 처음 설계의 예에는 `mock_secret`이 없었다(보정 V6).

- 계정마다 토큰의 검증값(`token`)과 진짜 비밀번호의 자리 하나가 있다. `password`(값 그대로)이거나 `password_env`(Stet 프로세스의 환경 변수 이름)다. 둘 다 있거나 둘 다 없으면 오류다. 계정 이름은 비어 있지 않고 63바이트 이하다. 이름은 바이트 그대로 비교한다(대소문자만 다른 두 계정은 다른 계정이다). 모르는 키는 오류다. 오류는 계정 이름과 무엇이 틀렸는지만 말하고 값을 인용하지 않는다.
- **`mock_secret`**(보정 V6). 모르는 계정의 가짜 검증값을 만드는 비밀값이다(4장). 자격 증명이 아니지만 어디에도 적지 않는다. 파일을 쓸 때(`custody.Write`, 곧 `stet agent`가 파일을 바꿀 때) 없으면 만든다. 손으로 쓴 파일에 없으면 `stet proxy`가 프로세스마다 무작위 값을 쓰고 경고한다(`the credentials file has no mock_secret: after a restart a login attempt can tell an unknown account from a known one; run a stet agent command that changes the file (add, token, password or remove) to add one`).
- `password_env`의 값은 `stet proxy`가 시작할 때 한 번 읽는다. 비어 있으면 그 계정을 경고한다. 도는 프로세스의 환경은 밖에서 바꿀 수 없으므로, 그 계정의 진짜 비밀번호를 바꾸려면 다시 띄워야 한다. 다시 띄우지 않고 바꾸려면 `password`에 둔다. 띄운 뒤에 파일이 시작할 때 읽지 않은 변수를 가리키게 바뀌면, 쓰던 비밀번호를 그대로 쓰고 한 번 경고한다.
- 계정 하나에 토큰 하나다.
- 경로의 상대 경로는 설정 파일이 있는 디렉터리에 대해 읽는다(보정 V2).
- **권한.** PostgreSQL이 서버 키 파일에 두는 규칙 그대로다(진행 중의 판단). 파일의 소유자가 Stet의 실효 uid면(root로 돌리면 root) 그룹과 다른 사용자에게 아무 권한이 없어야 한다. 소유자가 root면(Stet은 다른 사용자로 돈다) 그룹 읽기까지(`0640`)다. 그 밖의 소유자이거나 그보다 열려 있으면 Stet은 시작하지 않는다. 처음 설계의 "root의 파일이면 `0640` 이하"는 root로 도는 Stet에게는 `0600` 이하로 좁혀진다. 컨테이너에 비밀을 넣는 흔한 방식(root 소유, 그룹 읽기)을 받기 위해서다. 일반 파일이 아니거나(FIFO는 열 때 막히지 않게 연다) 1 MiB보다 크면 받지 않는다.
- 파일은 `stet agent`가 쓴다. 손으로 고쳐도 된다. 쓰는 방식은 `init`과 같다(같은 디렉터리의 임시 파일에 다 쓰고 sync한 뒤 이름을 바꾸고, 디렉터리를 sync한다). 반쯤 쓰인 파일은 남지 않는다. 경로가 심볼릭 링크면 링크가 가리키는 파일을 바꾼다. 이미 있는 파일을 바꿔 쓸 때는 그 파일의 소유자, 그룹, 권한을 그대로 둔다(소유자를 바꾼 뒤에 권한을 맞춘다). 그대로 둘 수 없으면(다른 사용자의 파일인데 root가 아님) 쓰지 않고 멈춘다. `sudo`로 돌린 뒤에 Stet이 파일을 다시 읽지 못하는 일을 막는다. 새로 만드는 파일은 돌린 사용자의 `0600`이다.
- **바꿀 수 있는 사람과 파일**(진행 중의 판단). `stet agent`는 파일을 그 소유자나 root로 바꾼다. 읽는 규칙은 `stet proxy`의 것과 다르다: 이미 있는 파일이 그 이상 열려 있지 않아야 한다(소유자가 root면 `0640`까지, 그 밖이면 그룹과 다른 사용자에게 권한이 없어야 한다). 그래서 root는 자기의 `0640` 파일과 다른 사용자의 파일을, `sudo`는 Stet을 돌리는 사용자의 파일을 바꿀 수 있다. Read의 규칙으로 읽으면 둘 다 거부됐다. 너무 열린 파일에는 비밀번호를 쓰지 않는다.
- **잠금**(진행 중의 판단). `stet agent`의 변경은 파일이 있는 디렉터리에 건 `flock(LOCK_EX)` 안에서 읽고, 바꾸고, 쓴다. 두 `stet agent`가 동시에 돌아도 서로의 변경을 지우지 않는다. 잠금 파일은 남지 않는다. `stet proxy`의 읽기는 잠금을 쓰지 않는다(이름 바꾸기가 원자적이다). NFS 같은 네트워크 파일 시스템에서는 잠금이 한 기기 안에서만 통한다.
- 컨테이너에는 파일 하나가 아니라 **디렉터리를 마운트한다**. 파일 하나를 마운트하면 이름 바꾸기가 되지 않거나, 바깥에서 바꾼 것이 안에서 보이지 않는다. 읽기 전용으로 마운트한 `stet proxy`가 다른 컨테이너의 `stet agent token`을 다시 띄우지 않고 받는 것을 확인했다(14장).
- **다시 읽기.** Stet은 로그인마다 그 경로의 파일이 바뀌었는지(바뀐 시각, 크기, 권한, 파일 자체가 갈렸는지. 처음 설계에는 권한이 없었다) 보고, 바뀌었으면 다시 읽는다. 토큰과 비밀번호를 바꾼 것은 다음 로그인부터 적용된다. 열려 있는 세션은 그대로 간다. 지운 계정은 다음 로그인부터 거절한다. 다시 읽지 못하거나 권한이 풀려 있으면 쓰던 것을 계속 쓰고 에러 로그(`could not read the credentials file again; keeping the accounts as they were`)를 한 번 남긴다. 실패하는 동안은 파일이 그대로 보여도 로그인 때마다(200ms에 한 번까지) 다시 읽어 본다. 실패의 원인이 stat에 보이지 않을 수 있기 때문이다(진행 중의 판단. 처음에는 다시 시도하지 않아, 지운 계정이 계속 로그인할 수 있었다). 다시 읽히면 INFO(`read the credentials file again; following it once more`)를 남긴다.
- **계정을 더하는 것은 다시 띄워야 적용된다.** 시작할 때의 확인(8장)이 계정마다 돌고, 게이트가 보는 계정 목록이 시작할 때 정해지기 때문이다. 띄운 뒤에 파일에 생긴 계정은 받지 않고, 처음 볼 때 경고를 한 번 남긴다(`account "<계정>" was added after stet started; restart stet proxy to accept it`).

## 7. `stet agent`

```
stet agent add      [-config stet.yaml] [-password-env NAME] <계정>
stet agent token    [-config stet.yaml] <계정>
stet agent password [-config stet.yaml] [-password-env NAME] <계정>
stet agent remove   [-config stet.yaml] <계정>
stet agent list     [-config stet.yaml]
```

설정 파일에서 `upstream`과 `agents.credentials`를 읽는다. `agents.credentials`가 없으면 그것부터 적으라고 말하고 멈춘다(`the config has no agents.credentials; add it first`). 자격 증명 파일이 없으면 `add`가 만든다. 파일이 들어갈 디렉터리는 만들지 않는다(`agents.credentials: the directory <디렉터리> is not there; create it first`).

- **`add`.** 계정을 더한다.
  1. 묻기 전에 멈출 것을 본다. Stet 자신의 계정이면(`upstream`의 계정을 pgconn이 정하는 대로. `"<계정>" is the account stet itself connects as; an agent needs its own`), 이미 있는 계정이면(`"<계정>" is already there; use stet agent token or stet agent password`), 디렉터리가 없으면 멈춘다.
  2. 진짜 비밀번호를 받는다. 터미널이면 글자를 보이지 않고 한 번 묻는다. 묻는 글(`Database password for "<계정>": `)은 표준 에러로 간다. 묻는 중의 Ctrl-C는 터미널을 되돌리고 130으로 끝낸다. 터미널인지는 `term.IsTerminal`로 본다(그래서 `/dev/null`은 터미널이 아니다. `stet init`과 다르다). 터미널이 아니면 표준 입력의 첫 줄을 읽는다(끝의 `\r\n`이나 `\n`은 뺀다). 빈 비밀번호는 받지 않는다. `-password-env NAME`이면 값을 묻지 않고 그 환경 변수의 이름을 파일에 적는다(값은 `stet agent`를 돌린 환경에서 읽어 3의 확인에만 쓴다. 비어 있으면 멈춘다). `NAME`이 변수 이름의 모양(`[A-Za-z_][A-Za-z0-9_]*`)이 아니면 값을 준 것일 수 있으므로, 그 글자를 다시 보이지 않고 멈춘다(`-password-env takes the name of an environment variable, not its value`). `stet proxy`의 환경에도 같은 변수가 있어야 한다고 알린다.
  3. 그 계정과 비밀번호로 업스트림에 로그인해 본다. Stet이 로그인하는 것과 같은 방식(5장)이고, 데이터베이스는 `upstream`의 것(주소에 없으면 `upstream`의 계정 이름. 서버가 데이터베이스 없는 시작 패킷을 읽는 방식이다), 시간 제한은 5초다. 되지 않으면 아무것도 쓰지 않는다. 에러는 서버가 비밀번호를 거부했는지(`the database refused that password for "<계정>", so nothing was written: …`) 다른 이유로 로그인하지 못했는지(`could not log in to the database as "<계정>" to check the password, so nothing was written: …`) 가르고, 실패의 갈래와 서버의 문구를 붙인다. 주소와 비밀번호는 적지 않는다.
  4. 토큰을 만들고, 파일에 검증값과 비밀번호(또는 변수 이름)를 쓴다. 쓰기는 잠금 안에서, 그때의 파일에 대해 한다(6장). 그사이 다른 `stet agent`가 같은 계정을 더했으면 쓰지 않고 멈춘다.
  5. 토큰을 표준 출력에 한 번 보여 준다. 다시 볼 수 없다고 말한다. 이어서 할 일을 알린다: 에이전트의 접속 문자열에서 호스트를 Stet의 것으로, 비밀번호를 토큰으로 바꿀 것. 이 계정의 예전 비밀번호를 다른 곳이 알고 있으면 DB에서 바꾸고 `stet agent password`로 새 값을 넣을 것(`-config`를 줬으면 그대로 넣은 명령을 보인다). 설정에 `agents.roles`가 있으면 거기에도 더할 것. `stet proxy`를 다시 띄울 것.
- **`token`.** 토큰을 새로 만든다. 예전 토큰은 다음 로그인부터 통하지 않는다. 다시 띄울 필요가 없다(`the old token stops working at the next login; no restart is needed`).
- **`password`.** 진짜 비밀번호를 바꿔 적는다. 운영자가 DB에서 비밀번호를 바꾼 뒤에 쓴다. `add`의 2와 3을 그대로 한다. 파일에 둔 비밀번호는 다시 띄울 필요가 없다(`password for "<계정>" updated; no restart is needed`). `password_env`인 계정은 `stet proxy`의 환경을 고치고 다시 띄워야 한다고 말하고 아무것도 바꾸지 않는다. `-password-env NAME`을 주면 그 계정의 비밀번호 자리를 그 변수로 옮긴다(진행 중의 판단. 다시 띄워야 한다고 말한다).
- **`remove`.** 계정을 지운다. 다음 로그인부터 거절한다. 설정에 `agents.roles`가 있으면 거기서도 빼라고, 마지막 계정을 지웠으면 계정을 더하기 전에는 `stet proxy`가 시작하지 않는다고 알린다.
- **`list`.** 계정(이름 순서), 비밀번호의 자리(`file`이나 `env NAME`), 더한 때, 토큰을 만든 때(UTC, 분까지)를 보인다. 토큰과 비밀번호는 보이지 않는다. 계정이 없으면 `no accounts`다.

**파일을 다루는 방식**(진행 중의 판단). `add`와 `password`는 묻고 로그인해 보는 동안 잠금을 쥐지 않는다. 그 뒤 잠금 안에서 그때의 파일을 다시 읽고 자기 계정의 변경만 더한다. 그래서 비밀번호를 묻는 동안 다른 `stet agent`가 지운 계정을 되살리거나 새로 만든 토큰을 덮어쓰지 않는다. `token`과 `remove`는 잠금 안에서 읽고 바꾼다. `list`는 같은 규칙으로 읽기만 한다. 파일의 소유자나 root로 돌린다(6장).

**디렉터리를 sync하지 못했을 때.** 파일은 제자리에 있고 디렉터리의 sync만 실패하면, 명령은 성공으로 끝나고(토큰도 보인다) `warning: … the change may not survive a crash …`를 덧붙인다. 그 밖에 쓰기가 실패하면 토큰을 보이지 않고 아무것도 바꾸지 않는다.

`stet agent`는 DB의 계정을 만들거나 고치지 않는다. `add`와 `password`가 로그인을 한 번 해 볼 뿐이다. 토큰은 그것을 만든 `add`나 `token`의 표준 출력에 한 번만 나오고, 검증값과 비밀번호, Stet 자신의 비밀번호와 `upstream` 주소는 어느 출력과 에러에도 나오지 않는다. 바깥에서 온 글자(서버의 문구, 파일의 이름)는 제어 문자를 걸러 적는다.

## 8. 설정과 시작할 때의 확인

**설정 검증** (`config.Load`)

설정 검증은 파일을 열지 않는다(보정 V1). `config.Parse`는 바이트만 읽고, 테스트 수백 개가 그렇게 쓴다. 그래서 자격 증명 파일의 권한과 모양, `agents.roles`와 파일의 계정 비교, 인증서와 키의 짝은 `stet proxy`가 시작할 때 본다(아래). 다르면 시작하지 않는 것은 같다. `config.Load`는 상대 경로를 설정 파일의 디렉터리에 대해 푼다(보정 V2).

- `listen_tls`는 `cert`와 `key`가 모두 있어야 한다. 공백뿐인 값은 없는 것이다.
- **키만 있고 값이 비면 시작하지 않는다**(마지막 검토 뒤에 고쳤다. 처음에는 키가 없는 것과 같았다). `listen_tls:` 뒤에 아무것도 없거나 `~`면 `listen_tls: present but empty; give cert and key, or remove it`, `agents.credentials`가 없음, `""`, `~`, 공백뿐이면 `agents.credentials: present but empty; give the file's path, or remove it`다. 템플릿이 빈 값을 내면 TLS도 보관도 없이 인증을 중계하는 Stet이 뜨고, 그것을 알리는 것은 시작 줄의 `listen_tls=off`와 `custody=off`뿐이기 때문이다. 아래에 아무것도 없는 `agents:`는 그대로 아무것도 정하지 않는다.
- `agents.credentials`가 있으면(보관을 켜면):
  - 에이전트 계정은 자격 증명 파일의 계정들이다. `agents.roles`는 적지 않아도 된다(enforce에서도. 보관이 없으면 enforce는 `agents.roles`가 필수다). 적었으면 파일의 계정들과 같아야 한다. 다르면 시작하지 않고 어느 계정이 한쪽에만 있는지 말한다(`agents.roles and the credentials file name different accounts: only in agents.roles: […]; only in the file: […]`). 섀도와 enforce 모두 같다. 이 비교는 시작할 때 한다.
  - **업스트림의 서버를 확인해야 한다.** `upstream`의 호스트가 루프백이 아니면 `sslmode`가 `verify-full`이거나, `sslrootcert`를 준 `verify-ca`나 `require`여야 한다. 아니면 시작하지 않는다(`upstream: with agents.credentials the server must be verified, or stet could hand a database password to whoever answers; use sslmode=verify-full, or verify-ca with sslrootcert=<CA file>`). 확인하지 않는 연결에서는 중간에 선 누군가가 비밀번호를 평문으로 달라고 할 수 있고, 클라이언트는 그것을 따른다. 첫 부분의 경고를 여기서는 거부로 올린다.
  - `sslrootcert` 없는 `verify-ca`는 받지 않는다(마지막 검토 뒤에 고쳤다. 처음에는 받았다). 그것은 시스템의 루트 인증서에 대해 발급자만 확인하고 호스트 이름은 보지 않는다. 공개 CA가 내준 인증서를 가진 누구나 통과해 비밀번호를 받을 수 있다. libpq는 이 조합을 거부하고 pgx는 받는다. 시작 줄의 `upstream_tls`는 이 조합도 `verified`로 적는다(첫 부분의 구분 그대로다).
  - **`listen`이 루프백이 아니면 `listen_tls`가 있어야 한다.** 없으면 시작하지 않는다(`listen: with agents.credentials a listen address that is not loopback needs listen_tls: on a plaintext connection whoever sits in between can take over a session once the agent has logged in`). 토큰은 SCRAM이라 평문에서도 드러나지 않지만, 로그인이 끝난 평문 연결에는 중간에 선 누군가가 문장을 끼워 넣을 수 있다. 그것은 토큰 없이 세션을 얻는 것이다(1장).

**시작할 때** (`stet proxy`). 이 순서다.

1. `listen_tls`의 두 파일을 읽는다. 읽지 못하거나 짝이 맞지 않으면 시작하지 않는다. 키 파일의 권한을 경고한다(3장).
2. 자격 증명 파일의 권한과 모양을 본다(6장). 읽지 못하면 시작하지 않는다(`agents.credentials: …`). 계정이 하나도 없으면 시작하지 않는다(`the credentials file has no account; add one with stet agent add`). `password_env`가 가리키는 변수가 비어 있으면 그 계정을 경고한다. `mock_secret`이 없으면 경고한다.
3. **Stet 자신의 계정이 파일에 있으면 시작하지 않는다**(진행 중의 판단): `agents.credentials: "<계정>" is Stet's own account (the upstream user)`. 그 계정은 pgconn이 `upstream`을 읽어 정하는 사용자다(주소, `PGUSER`, Stet을 돌리는 사용자). 그 토큰을 가진 쪽은 게이트가 보지 않는 세션을 얻는다. `stet` 스키마의 소유자이고 모든 에이전트 역할의 구성원인 계정이다. 설정 검증의 같은 확인(`agents.roles`의 계정)은 주소에 계정이 적혔을 때만 보므로, 파일의 계정은 여기서 본다. `stet agent add`도 그 계정을 받지 않는다(7장).
4. `agents.roles`가 있으면 파일의 계정들과 비교한다.
5. 게이트를 연다. 지금의 에이전트 계정 확인(spec-v0.2-enforce 8장. enforce에서)은 그대로 돈다. 대상이 파일의 계정들이다. 섀도에서도 Stet의 계정이 계정마다 그 역할의 구성원인지는 보고, 아니면 enforce와 같은 문구로 경고한다(`Stet's account "<Stet의 계정>" is not a member of agent role "<계정>": an approved write of that role ends as failed, and its multi-row writes are not measured; fix: GRANT "<계정>" TO "<Stet의 계정>"`. 마지막 검토 뒤에 더했다). 보관을 켜면 두 계정이 반드시 달라서, 이 부여가 없으면 여러 행의 쓰기가 모두 `unsupported`로 기록되고 운영자는 그것을 기록에서야 알게 되기 때문이다. DB에 없는 계정은 여기서 말하지 않는다(6의 로그인이 경고한다). 보관이 없는 섀도는 그대로 이것을 보지 않는다. 게이트 대상 테이블이 있는데 DB에 닿지 않으면 여기서 곧바로 멈춘다. 그래서 계정마다의 로그인(6)은 게이트를 연 뒤에 한다.
6. 계정마다 업스트림에 한 번 로그인해 본다(계정마다 5초, 합쳐 30초까지). 되지 않으면 그 계정과 이유의 갈래를 경고한다(`an account of the credentials file cannot log in to the database: its agent's logins are refused until it can`). 시작은 한다. 다른 계정은 일해야 하고, 그 계정의 로그인은 DB가 그 비밀번호를 받을 때까지 어차피 거절된다(`08001`). Stet은 그 계정을 따로 표시하지 않는다. DB가 받게 되면 그다음 로그인부터 된다. 합친 시간이 다 되면 그때 확인 중이던 계정과 닿지 못한 계정들을 한 경고에 적는다(`the start ran out of time to log in as these accounts, so they are not checked`).
7. 받기 시작할 때의 로그 줄에 클라이언트 쪽의 상태를 더한다(`listen_tls=on|off`, `custody=on|off`). 처음 설계의 `agents=<수>`는 빼었다(진행 중의 판단). `agent_roles`가 이미 계정들을 적고, 보관을 켜면 그 값이 파일의 계정들이다.

## 9. 기록과 로그

- 로그인이 되면 INFO `agent logged in`으로 계정, 상대 주소, 클라이언트 쪽 구간의 TLS 여부(`tls=true|false`)를 남긴다. 거절하면 WARN `agent login refused`로 계정, 상대 주소, 이유의 갈래(`reason`)를 남긴다. 갈래는 `unknown account`, `bad token`, `upstream login failed`, `exchange failed`, `tls required`다. 처음 설계에는 `exchange failed`가 없었다. `upstream login failed`와 `exchange failed`에는 `err`가 붙는다(업스트림 로그인 실패의 갈래 `wrong password`, `cannot reach the server`, `certificate`, `refused TLS`, `other`와 서버의 문구). 계정 이름은 시작 패킷에서 온 글자다. 제어 문자를 걸러 적는다(`internal/safetext`).
- 디버그(`-v`)에만 남는 것(진행 중의 판단): 교환 도중에 클라이언트가 끊은 것(`agent left during login`. `psql`은 비밀번호 없이 먼저 붙어 보고 끊는다), 복제 접속을 거절한 것(`replication connection refused`). 교환이 10초를 넘긴 것은 WARN `exchange failed`이고 클라이언트에 `FATAL`을 보내지 않는다.
- `tls required`의 WARN은 보관을 켠 Stet만 남긴다(3장).
- 토큰, 검증값, 진짜 비밀번호는 어디에도 적지 않는다. pgx가 돌려준 에러도 그대로 적지 않는다. 업스트림 로그인의 에러는 실패의 갈래와 서버가 보낸 문구(SQLSTATE 포함, 제어 문자를 거름)로 다시 만든다. pgconn의 에러는 주소를 인용하고, 그 접속 에러는 비밀번호가 든 설정을 쥐고 있어서 감싸지도 않는다.
- **검증값의 글자**(보정 V7). 검증값을 담은 값은 `%v`, slog, JSON, YAML로 찍히면 가린 글자(`SCRAM-SHA-256$4096:<redacted>`)가 된다. 파일에 쓰는 글자는 `Verifier.Text()`로만 얻는다. 검증값을 담은 구조체가 나중에 로그 줄에 찍혀도 StoredKey와 ServerKey가 새지 않게 하기 위해서다.
- 판단 기록의 스키마는 바뀌지 않는다.

## 10. 의존성

- 서버 쪽 SCRAM은 표준 라이브러리(`crypto/hmac`, `crypto/sha256`, `crypto/pbkdf2`, `crypto/subtle`)로 쓴다(`internal/scram`). 자격 증명 파일과 업스트림 로그인은 `internal/custody`에 둔다. `internal/proxy`는 `Login` 인터페이스만 안다(규칙 7).
- 터미널에서 글자를 보이지 않고 읽는 데 `golang.org/x/term`(v0.48.0)을 더한다. 이 단계에서 더하는 의존성은 이것 하나다. 그와 함께 `golang.org/x/sys`가 v0.35.0에서 v0.50.0으로 올라갔다.

## 11. 문서

이 단계의 Task 9에서 썼다.

- `docs/deploy.md`(새 문서, 한국어): 서버에 놓는 순서(인증서, 설정, `stet agent add`, 에이전트의 접속 문자열, 예전 비밀번호 바꾸기), 다시 읽기와 다시 띄워야 하는 것, 비밀을 두는 곳과 그 권한, 한계(12장). 두 가지 놓는 법을 처음부터 끝까지 적고 그대로 따라 해 본다: 서비스 계정으로 돌리는 바이너리, 그리고 디렉터리를 마운트한 Docker 이미지. Kubernetes의 비밀 볼륨은 적기만 하고 돌려 보지 않는다(그렇게 적는다).
- `docs/quickstart.md`: "통제가 아니다"에서 이 문서로 잇는다.
- `stet.example.yaml`, `docs/spec-v0.1.md` 9장: `listen_tls`, `agents.credentials`.
- `README.md`, `docs/concept-v0.2.md`, `docs/decisions.md`는 소유자의 문서다. 고치지 않고, 낡는 줄을 보고한다.

## 12. 한계

- **진짜 비밀번호가 Stet 서버에 있다.** 파일이나 환경 변수에 평문으로 있고, 프로세스의 메모리에 있다. Stet 서버가 뚫리면 에이전트 계정의 비밀번호가 모두 나간다. Stet 자신의 계정의 비밀번호(`upstream`)도 나가는데, 그 계정은 아래처럼 모든 에이전트 역할을 가지고 판단 기록을 소유한다. `listen_tls`의 키도 나가고, 그 키를 가진 쪽은 에이전트에게 Stet인 척할 수 있다. 운영자도 비밀번호들을 안다.
- **Stet 자신의 계정은 모든 에이전트 계정을 합친 것보다 무겁다.** 영향 측정과 승인된 쓰기의 실행 때문에 에이전트 계정마다 `GRANT agent_x TO stet`을 한다(5장). 그 부여로 Stet의 계정은 각 에이전트 계정으로 `SET ROLE`할 수 있고, 역할의 기본값(`INHERIT`)이면 `SET ROLE` 없이도 그 테이블 권한을 가진다(PostgreSQL 14.18에서 확인했다). 그 위에 `stet` 스키마의 소유자다. 그 비밀번호를 아는 쪽은 Stet을 건너뛰어 어느 에이전트의 권한으로든 쓰고 `stet.decisions`를 고칠 수 있다. 계정 하나로 도는 quickstart의 길에서 옮겨 오면 에이전트가 이 비밀번호를 알고 있으므로, 토큰을 나눠 주기 전에 DB와 `upstream`에서 바꾸고 다시 띄워야 한다(deploy 8장). Stet은 그것이 됐는지 알 수 없다. 부여는 DB의 관리 계정이 한다(Stet의 계정이 superuser도 `CREATEROLE`도 아니면 스스로 하지 못한다: `must have admin option on role …`). PostgreSQL 16부터는 superuser가 아닌 관리 계정에 그 역할의 `ADMIN OPTION`이 있어야 한다(릴리스 노트의 동작이다. 돌려 보지 않았다).
- **예전 비밀번호를 아는 에이전트는 건너뛸 수 있다.** 계정을 Stet 뒤로 옮길 때 DB에서 비밀번호를 바꿔야 한다. Stet은 그것이 됐는지 알 수 없다.
- **운영 규칙은 그대로 전제다.** 누군가 에이전트에게 서비스나 사람의 자격 증명을 주면 Stet은 그 쓰기를 보지 못한다(concept-v0.2 5장).
- **채널 바인딩을 요구하는 클라이언트는 붙지 못한다.** 에이전트는 Stet의 인증서를 `sslmode=verify-full`로 확인한다.
- **에이전트가 `sslmode=require`로 붙으면 Stet이 진짜인지 확인하지 않는다.** 중간에 선 누군가가 로그인을 그대로 흘려보낸 뒤 세션을 차지할 수 있다. Stet은 클라이언트의 `sslmode`를 알 수 없어 막지 못한다. 문서가 `verify-full`을 쓰라고 말한다. 채널 바인딩을 제안하면 막을 수 있지만 남긴다(15장).
- **루프백은 평문도 받는다.** 루프백인지는 접속의 상대 주소로 본다. 같은 기기의 TCP 전달자(사이드카 프록시, `ssh -L`, `kubectl port-forward`)를 Stet 앞에 두면 모든 클라이언트가 루프백으로 보여, 루프백이 아닌 `listen`의 TLS 요구가 무력해진다(3장). 서비스 메시(Istio, Linkerd)의 사이드카도 받은 연결을 파드 안에서 넘겨주므로 그렇게 보일 수 있다(돌려 보지 않았다. deploy 13장).
- **`password_env`인 계정의 진짜 비밀번호를 바꾸려면 다시 띄워야 한다.**
- **계정 하나에 토큰 하나다.** 토큰을 바꾸면 예전 토큰을 쓰던 에이전트는 다음 로그인에서 거절된다. 겹쳐 쓰는 기간이 없다.
- **계정을 더하면 다시 띄워야 한다.** 그동안 에이전트의 접속이 끊긴다(가용성은 셋째 부분이다).
- **로그인이 한 번 더 든다.** 세션을 열 때 Stet이 업스트림에 로그인하는 시간이 더해진다. 시험 서버에서 한 번의 로그인은 중앙값으로 직접 TLS로 8.5ms, 양쪽이 TLS인 Stet을 거쳐 17.8ms였다(14장). 세션을 자주 여닫는 클라이언트에서 보인다. 연결 풀은 만들지 않는다.
- **Stet은 프로토콜 3.0으로 답한다.** 더 높은 부 버전을 청한 클라이언트는 3.0으로 내려 받는다.
- **`stet agent`의 잠금은 한 기기 안에서만 통한다.** 네트워크 파일 시스템의 자격 증명 파일을 두 기기에서 바꾸면 서로 막지 못한다(6장).
- **실제 매니지드 서비스에서 확인하지 못했다.** 시험은 로컬의 TLS 서버로 한다(첫 부분과 같다).
- **컨테이너의 파일 소유자 규칙은 Linux에서 확인하지 못했다.** 따라 한 기기(macOS의 Docker Desktop)는 바인드 마운트한 파일을 컨테이너 안에서 컨테이너를 돌린 사용자의 것으로 보였다(deploy 12장).
- **연결 풀러, 스키마를 적은 이름 등 첫 부분의 한계는 그대로다**(spec-v0.2-tryout 10장).

## 13. 시험

시험 서버는 첫 부분에서 TLS를 켰다. Stet 쪽 인증서는 테스트가 만든다.

| 대상 | 확인 |
|---|---|
| 서버 쪽 SCRAM | RFC 7677의 예제 값. PostgreSQL이 만든 검증값(`pg_authid`)을 우리 코드가 같은 비밀번호로 받아들인다. 우리가 만든 검증값으로 `psql`과 pgx가 로그인한다 |
| 로그인 | 토큰으로 `psql`과 pgx가 `sslmode=verify-full`로 붙어 문장을 실행한다. `current_user`가 그 계정이고 서버 쪽에서 TLS다. 시작 파라미터가 전달된다 |
| 거절 | 틀린 토큰, 모르는 계정, 지운 계정, 진짜 비밀번호를 토큰 자리에 넣은 것 → 모두 `28P01`과 같은 문구. 모르는 계정과 틀린 토큰에서 클라이언트가 받는 메시지의 수와 종류가 같다 |
| 건너뛰기 | 토큰으로 DB에 직접 붙으면 서버가 거절한다. 한 계정의 토큰으로 다른 계정 이름을 대면 거절한다 |
| 클라이언트 쪽 TLS | SSLRequest에 `S`. 루프백이 아닌 평문은 거절(판단하는 자리의 단위 테스트). 인증서 파일을 바꾸면 다음 인사에 새 인증서. 보관 없이 켠 경우의 중계(md5 계정) |
| 프로토콜 | 첫 부분의 시나리오 표(확장 질의, COPY, 큰 결과, 취소, 알림, 파이프라인)를 보관을 켠 세션으로 돌린다 |
| 게이트 | 보관을 켠 세션의 쓰기가 섀도에서 기록되고 enforce에서 막힌다. `login_role`이 그 계정이다. 승인된 쓰기가 실행된다. 낡은 쓰기 탐지가 돈다 |
| 다시 읽기 | `stet agent token` 뒤에 예전 토큰은 다음 로그인에서 거절되고 열린 세션은 계속 간다. 다시 띄우지 않는다. 띄운 뒤에 더한 계정은 받지 않는다 |
| 파일 | 권한 규칙의 줄마다(자기 파일 `0600`, `0640`은 거부, 누구나 읽는 파일은 거부. root 소유의 경우는 판단하는 자리의 단위 테스트). `password_env`. 반쯤 쓰인 파일이 남지 않는다. 바꿔 쓴 뒤에 권한이 그대로다 |
| 확인 안 된 업스트림 | 보관을 켜고 원격 `sslmode=require`나 `verify-ca`(루트 없음)면 시작하지 않는다 |
| 평문 `listen` | 보관을 켜고 루프백이 아닌 `listen`에 `listen_tls`가 없으면 시작하지 않는다 |
| 보관 없는 양쪽 TLS | SCRAM 계정이 기본 설정의 `psql`로는 로그인하지 못하고 `channel_binding=disable`로는 로그인한다. md5 계정은 로그인한다 |
| 비밀 | 로그인 성공·실패, 업스트림 실패, `stet agent`의 모든 출력과 로그에 토큰·검증값·비밀번호가 없다(토큰을 만든 그 출력 한 번은 뺀다) |
| 지연 | `latency_test.go`의 2ms 예산을 보관을 켠 세션으로. 로그인에 드는 시간을 따로 재어 적는다 |
| `stet agent` | 명령마다. 틀린 비밀번호로는 아무것도 쓰지 않는다. 터미널이 아닐 때의 입력 |

## 14. 수용 기준

2026-11-23에 확인했다. 테스트는 `STET_TEST_PG=127.0.0.1:55432`(PostgreSQL 14.18, TLS와 평문을 모두 받는 시험 서버)로 돌렸고, 건너뛴 통합 테스트는 없다(건너뛴 것은 앞 단계의 `STET_MEMORY_AT_MAX`로 켜는 메모리 측정 `TestLockHoldAtTheAllowedMaximum` 하나다). 테스트 이름 뒤의 괄호는 패키지다. "따라 하기"는 [deploy](deploy.md) 11장(바이너리)과 12장(Docker)을 이 기기(macOS)에서 그대로 친 것이다. 시험 서버에 이 확인만을 위한 데이터베이스 하나와 역할 셋(Stet의 계정, 에이전트 계정 둘)을 만들어 쓰고 끝난 뒤 지웠다.

- [x] 토큰만 아는 `psql`과 pgx가 `sslmode=verify-full`로 Stet에 붙어 그 계정의 세션을 얻고 문장을 실행한다. 진짜 비밀번호는 클라이언트 쪽 어디에도 없다.
  - `TestAgentLogsInWithAToken`(internal/proxy): pgx와 `psql`이 토큰만으로 `verify-full`로 붙는다. `current_user`가 그 계정이고, 서버 쪽에서 TLS이고, 시작 파라미터가 전달된다. 루프백에서는 TLS 없이도 붙는다.
  - `TestCustodyEndToEnd`(cmd/stet): `listen_tls`와 자격 증명 파일로 띄운 Stet에 `psql`과 pgx가 `verify-full`로 붙어 게이트 대상 테이블을 고친다. 경고는 없다.
  - 따라 하기: 두 놓는 법 모두 에이전트 쪽 셸은 토큰(`PGPASSWORD`)과 Stet의 인증서만 가졌다. `current_user`가 그 계정이었다.
- [x] 그 토큰으로는 DB에 직접 로그인할 수 없다. `TestTokenIsNoDatabasePassword`(internal/proxy). 따라 하기: 토큰으로 시험 서버(55432)에 바로 붙자 서버가 `28P01`로 거부했다.
- [x] 틀린 토큰과 모르는 계정은 같은 답을 받는다. `TestLoginRefusals`(internal/proxy): 틀린 토큰, 모르는 계정, 진짜 비밀번호를 토큰 자리에 넣은 것, 다른 계정의 토큰이 모두 `28P01`이고, 모르는 계정과 틀린 토큰이 받는 메시지의 수와 모양이 같다(`SameAnswers`). 가짜 검증값: `TestVerifierKnownAndMock`, `TestStoreMockSecret`(internal/custody), `TestMockNeverSucceeds`(internal/scram). 따라 하기: 두 경우의 `psql` 출력이 계정 이름 말고 같았다.
- [x] 양쪽 구간이 TLS다(클라이언트 쪽은 인사로, 서버 쪽은 `pg_stat_ssl`로 확인한다). `TestAgentLogsInWithAToken`(internal/proxy)과 `TestCustodyEndToEnd`(cmd/stet)가 pgx의 연결이 `*tls.Conn`이고 `pg_stat_ssl.ssl`이 참인 것을 본다. 따라 하기: Stet의 로그 `tls=true`, `pg_stat_ssl`의 `TLSv1.3`.
- [x] 보관을 켠 세션에서 섀도의 기록, enforce의 차단, 보류와 승인, 낡은 쓰기 탐지, 되돌리기가 지금과 같이 돈다. `login_role`이 그 계정이다.
  - 섀도의 기록과 `login_role`: `TestCustodyEndToEnd`(cmd/stet). 따라 하기: 두 놓는 법 모두 판단 줄과 `stet.decisions`의 `login_role`이 그 계정이었다(바이너리: `stet.decisions`에서 `commit`, `hold`, `hold`. Docker: `stet.decisions`에서 `commit`, `commit`을 봤다. 그 조회는 세 번째 쓰기 전에 했고, 토큰을 바꾼 뒤의 세 번째 쓰기는 판단 줄의 `would hold`로만 봤다).
  - enforce의 차단(`ST001`), 보류와 승인(검토자가 승인한 쓰기가 실행된다), 낡은 쓰기 탐지(`fresh`): `TestCustodyEnforce`(cmd/stet). 게이트가 로그인한 세션을 처음부터 인증된 세션으로 보는 것(첫 쓰기에 계정과 서버 프로세스, enforce에서 서버가 알린 설정): `TestWatchedSessionKnowsItsAccount`. 게이트가 보는 세션의 시나리오: `TestWatchedScenariosThroughALogin`(internal/proxy).
  - 되돌리기: `TestUndoThroughACustodySession`(cmd/stet. 마지막 검토 뒤에 더했다). 토큰으로 붙은 세션이 `undo.tables`의 테이블에서 한 행을 고치면 `stet.history`에 `agent_role`과 `login_role`이 그 계정인 변경이 그 판단과 이어져 남고, `stet.undo_plan`이 `would_restore`, `stet.undo`가 `restored`를 돌려주고 행이 돌아온다. 그 전에 손으로도 확인했다(2026-11-23). 따라 하기의 바이너리 설정에 `undo.tables.contacts`를 더하고 `stet undo-sql`의 출력을 테이블 소유자로 실행한 뒤, 토큰으로 붙은 `psql`이 한 행을 고쳤다. `stet.history`에 `agent_role`이 그 계정인 변경이 남았고, `stet.undo_plan`이 `would_restore`, `stet.undo`가 `restored`를 돌려주고 행이 돌아왔다.
- [x] 첫 부분의 프로토콜 시나리오가 보관을 켠 세션에서 통과한다. 취소가 전달된다. `TestScenariosThroughALogin`(취소 요청이 서버에 닿는다), `TestWatchedScenariosThroughALogin`, 보관 없이 클라이언트 쪽 TLS만 켠 `TestScenariosOverClientTLS`(internal/proxy).
- [x] `stet agent add`, `token`, `password`, `remove`, `list`가 7장대로 돈다. 토큰과 비밀번호 바꾸기는 다시 띄우지 않고 적용된다.
  - 명령마다: `TestAgentAdd`, `TestAgentAddRefusesAWrongPassword`, `TestAgentAddPasswordEnv`, `TestAgentToken`, `TestAgentPassword`, `TestAgentRemove`, `TestAgentList`, `TestAgentPromptsWithoutEcho`, `TestAgentStops`, `TestAgentKeepsTheFileMode`, `TestAgentKeepsChangesMadeWhileItAsks`, `TestAgentTouchesOnlyItsAccount`, `TestAgentChecksAgainWhenItWrites`, `TestAgentWritesNothingWhenTheWriteFails`, `TestAgentGoesOnWhenTheDirectoryIsNotSynced`, `TestAgentSaysWhatStopsTheProxy`, `TestOnInterruptRestoresTheTerminal`(cmd/stet). 잠금: `TestUpdatesDoNotInterleave`, `TestUpdateLocksAcrossProcesses`(internal/custody).
  - 다시 띄우지 않고: `TestTokenRotationNeedsNoRestart`(cmd/stet. 예전 토큰은 거절, 열린 세션은 계속, 띄운 뒤에 더한 계정은 거절과 경고 한 번), `TestStoreFollowsTheFile`(internal/custody. 새 토큰과 새 비밀번호, 지운 계정).
  - 따라 하기: 두 놓는 법 모두 `stet agent token` 뒤에 예전 토큰은 거절되고 새 토큰은 받아졌고, 예전 토큰으로 열어 둔 세션은 계속 갔다. Stet의 프로세스(컨테이너의 시작 시각과 재시작 수)는 그대로였다. 바이너리에서 DB의 비밀번호를 바꾼 뒤 `stet agent password` 전의 로그인은 `08001`, 뒤의 로그인은 성공했다.
- [x] 권한이 풀린 자격 증명 파일, 확인되지 않는 원격 업스트림, TLS 없는 원격 `listen`으로는 시작하지 않는다(뒤의 둘은 보관을 켰을 때). `TestRolesMustMatchTheFile`(cmd/stet. 다른 사용자가 읽을 수 있는 파일, 계정이 없는 파일, 한쪽에만 있는 계정), `TestCheckMode`, `TestStoreOpenRefusesABadFile`(internal/custody), `TestCustodyNeedsAVerifiedUpstream`, `TestCustodyNeedsTLSOnARemoteListen`(internal/config). 따라 하기 전에 빌드한 `stet proxy`로 `0640` 파일과 `listen: 0.0.0.0:…`에 `listen_tls`가 없는 설정을 띄워 봤고, 둘 다 4장·8장의 문구로 시작하지 않았다.
- [x] 토큰, 검증값, 진짜 비밀번호가 로그와 에러에 없다. `TestNoSecretInAnyLog`(cmd/stet. 디버그 로그, 판단 줄, `stet.decisions`), `TestAgentOutputShowsTheTokenOnce`(cmd/stet), `TestNoSecretInTheProxyLog`(internal/proxy), `TestLoginErrorsCarryNoSecrets`(internal/custody), `TestVerifierNeverFormatsItsSecret`(internal/scram). 따라 하기: 두 놓는 법의 Stet 로그와 출력(`docker logs` 포함)에서 쓴 토큰 넷과 비밀번호 넷을 찾았고 없었다.
- [x] 기존 테스트가 단언을 고치지 않고 통과한다. 보관을 켜지 않은 Stet의 동작은 이 단계 전과 같다(`listen_tls`를 적지 않았을 때 클라이언트가 받는 바이트 포함). 전체 실행(`STET_TEST_PG=127.0.0.1:55432 go test -race ./...`)에서 테스트가 있는 22개 패키지가 모두 `ok`다. `TestWithoutCustodyNothingChanges`(cmd/stet. 시작 줄의 키와 값, TLS 요청에 `N`), `TestClientTLSDeclinedWithoutACertificate`(internal/proxy). 이 단계 전부터 있던 테스트 파일에서 고친 것은 헬퍼뿐이고 단언은 그대로다: `proxy.New`의 서버를 따로 띄우는 헬퍼와 TLS·토큰으로 붙는 헬퍼(`proxy_test.go`, `upstream_test.go`), 옮겨 간 함수의 이름(`printable` → `safetext.Printable`, `init_test.go`), `loopbackHost`와 그 테스트를 `internal/config`로 옮긴 것(`TestLoopbackHost`, Task 4). `latency_test.go`에는 함수를 더하기만 했다.
- [x] `latency_test.go`의 예산을 지킨다. 로그인에 드는 시간을 적는다. `TestLoginLatencyBudget`(internal/proxy, `-race`): 보관을 켠 세션의 `select 1` 2,000회 왕복에서 더해진 p95가 45.6µs(Task 6), 44.1µs(2026-11-23 다시 잼)다. 예산은 2ms다. 로그인 한 번(21번의 중앙값)은 직접 TLS로 8.5ms, 양쪽이 TLS인 Stet을 거쳐 17.8ms(Task 6), 다시 잰 값은 8.7ms와 18.1ms다.
- [x] `docs/deploy.md`의 두 가지 놓는 법(바이너리, Docker 이미지)을 처음부터 끝까지 그대로 따라 해서 토큰으로 붙은 에이전트의 쓰기가 판단되는 것을 본다. 2026-11-23, macOS에서 `go build`한 바이너리와 이 기기에서 빌드한 이미지(Docker Desktop 28.0.1)로 했다. 이 기기에 맞춰 바꾼 것(서비스 계정·`sudo`·systemd 없이 이 기기의 사용자로, 빈 디렉터리, 포트, 인증서의 이름, 같은 기기의 시험 서버)은 deploy 11장·12장의 "확인한 것"에 적었다. 처음 따라 했을 때 여러 행의 쓰기가 `unsupported`(`not_single_row`)로 기록됐다. Stet의 계정이 에이전트 역할의 구성원이 아니었다(5장). 문서의 "필요한 것"에 `GRANT`를 더하고 처음부터 다시 했다. 이미지와 컨테이너, 데이터베이스와 역할은 지웠다.
- [ ] (확인하지 못한다) `psql`과 pgx 밖의 클라이언트(JDBC, psycopg, node-postgres)의 로그인. 이 기기에 있는 것만 해 보고, 못 한 것은 적는다. 이 기기에 있던 pgjdbc 42.7.14(Java 17.0.21)와 node-postgres 8.17.2(Node 24.22.2)는 `sslmode=verify-full`과 Stet의 인증서로, 토큰만으로 그 계정의 세션을 얻었고(`pg_stat_ssl.ssl` 참), 틀린 토큰에는 `28P01`을 받았다. psycopg(`psycopg`, `psycopg2`)는 이 기기에 없어 해 보지 않았다. 그래서 체크하지 않는다.
- [ ] (계정이 있어야 한다) Neon, Supabase, RDS 가운데 하나에서 `stet agent add`부터 판단까지 된다. 하지 않았다. 소유자가 로컬 TLS 서버로 먼저 확인하고 매니지드 프로젝트는 뒤에 시험하기로 했다(2026-11-22). 원격 호스트로 가는 길(`sslrootcert`를 준 `verify-ca`로 서버를 확인하는 업스트림)은 컨테이너에서 `host.docker.internal`로 로컬 시험 서버에 붙어서만 봤다.

## 15. 정한 것과 남긴 것

**정한 것**

- 매니지드 Postgres부터 한다. 에이전트는 토큰으로 Stet에 로그인하고 Stet이 진짜 자격 증명으로 DB에 로그인한다.
- 진짜 비밀번호는 운영자가 주고 Stet 서버의 파일이나 환경 변수에 둔다.
- 신원은 DB 계정 그대로다. 계정 하나에 토큰 하나다.
- 에이전트는 SCRAM으로 Stet에 로그인한다. 클라이언트를 고치지 않는다.
- 보관을 켠 Stet은 파일에 있는 계정만 받는다.
- 보관을 켜면 원격 업스트림의 서버 확인은 필수다(`verify-full`, 또는 `sslrootcert`를 준 `verify-ca`나 `require`). 루프백이 아닌 `listen`의 TLS도 필수다.
- 자격 증명 파일의 권한은 PostgreSQL의 키 파일 규칙을 따른다. `stet agent`는 바꿔 쓸 때 소유자와 권한을 그대로 둔다.
- 환경 변수에 둔 비밀번호는 다시 띄워야 바뀐다. 다시 읽는 파일 자리(`password_file`)는 만들지 않는다.
- 클라이언트 쪽 인증서는 운영자가 준다.
- 가용성은 셋째 부분으로 뗀다.

**구현하면서 정한 것** (계획의 스펙 보정과 진행 중의 판단. 해당 장에 옮겨 적었다)

- V1(8장): 자격 증명 파일의 권한과 모양, `agents.roles`와 파일의 계정 비교, 인증서와 키의 짝은 `config.Load`가 아니라 `stet proxy`가 시작할 때 본다. 설정 검증은 파일을 열지 않는다.
- V2(3·6·8장): `agents.credentials`, `listen_tls.cert`, `listen_tls.key`의 상대 경로는 설정 파일이 있는 디렉터리에 대해 읽는다(`config.Load`). `config.Parse`는 적힌 그대로 둔다.
- V3(5장): 프록시가 로그인 끝의 메시지들을 만들어 업스트림 연결의 앞머리로 붙이고, 세션은 그것을 서버가 보낸 것으로 읽는다. 세션의 코드는 바꾸지 않았다.
- V4(4장): 인증 동안 클라이언트의 메시지는 본문 16KiB까지 받고, 넘으면 `28000`이다. 버퍼 없이 읽는다.
- V5(4장): `Server.Close`가 진행 중인 업스트림 로그인을 취소한다.
- V6(4·6장): 가짜 검증값의 비밀값은 자격 증명 파일의 `mock_secret`에 둔다. 없으면 프로세스마다의 무작위 값과 경고.
- V7(9장): 검증값은 `Text()`로만 글자가 되고, 그 밖의 모든 출력은 가린 글자다.
- 계획이 정한 자리: 서버 쪽 SCRAM은 `internal/scram`, 자격 증명 파일과 업스트림 로그인은 `internal/custody`, 바깥 글자의 거르기는 `internal/safetext` 하나에 둔다(`internal/custody`, `cmd/stet`의 `init`, `internal/lines`가 같은 것을 쓴다). 프록시는 `Server`의 필드 둘(`ClientTLS`, `Login`)로 받는다. `loopbackHost`는 `internal/config`(`LoopbackHost`)로 옮겼다.
- 6장: 파일의 권한은 PostgreSQL의 규칙 그대로다(소유자가 실효 uid면 그룹·다른 사용자 권한 없음, root면 그룹 읽기까지). `stet agent`는 파일의 소유자나 root로, 너무 열린 파일에는 쓰지 않고, 디렉터리의 `flock` 안에서 그때의 파일을 바꾼다. 다시 읽기에 실패하는 동안은 로그인 때마다(200ms에 한 번까지) 다시 시도한다. 일반 파일이 아니거나 1 MiB를 넘으면 받지 않는다.
- 5장: 업스트림 로그인은 프로토콜 3.0으로 고정한다. 주소는 `pgxpool.ParseConfig`로 읽어 풀의 키를 시작 파라미터로 보내지 않는다. 루프백 업스트림의 `prefer`에서 이 로그인은 TLS 시도가 실패하면 평문으로 다시 붙는다(남긴다).
- 4장: 3.0보다 높은 버전이나 프로토콜 옵션을 청한 클라이언트에는 NegotiateProtocolVersion(3.0)을 보내고 옵션은 전하지 않는다. 첫 SCRAM 메시지가 없는 SASLInitialResponse는 `28000`. 교환의 10초는 시작의 10초와 따로 잰다. 시간을 넘긴 교환에는 `FATAL`을 보내지 않는다.
- 7장: `password -password-env NAME`은 그 계정을 그 변수로 옮긴다. `-password-env`는 변수 이름의 모양만 받고 아닌 것은 다시 보이지 않는다. 묻는 글은 표준 에러로 가고, Ctrl-C는 터미널을 되돌린다. 알리는 명령에는 준 `-config`가 들어간다. `agents.roles`와 마지막 계정에 대해 알린다. 디렉터리의 sync만 실패하면 토큰을 보이고 경고한다.
- 8장: Stet 자신의 계정(pgconn이 정하는 `upstream`의 사용자)은 `stet proxy`가 시작할 때와 `stet agent add`가 받지 않는다. 계정마다의 시작 로그인은 게이트를 연 뒤에 하고, 시간이 다 되면 확인하지 못한 계정들을 한 경고에 적는다. 시작 줄에는 `listen_tls`와 `custody`만 더한다(`agents=<수>`는 뺐다).
- 3장: 바뀐 인증서를 다시 읽지 못할 때마다 남기고, 반쯤 쓰인 짝은 쓰던 인증서를 쓴다. 보관 없이 양쪽이 TLS일 때의 경고는 `sslmode`가 `disable`이 아니면 나온다. `listen_tls`가 있으면 루프백이 아닌 `listen`의 "암호화되지 않는다" 경고는 없다. 설계의 "돌려 보지 않았다"는 돌려서 확인했다(`TestRelayWithTLSOnBothSides`).
- 9장: 거절의 갈래에 `exchange failed`를 더했다. 교환 도중에 끊은 클라이언트와 복제 접속의 거절은 디버그에만 남는다.
- 5장·deploy: 보관을 켜면 Stet의 계정과 에이전트 계정이 다르므로, 여러 행의 쓰기를 재려면 `GRANT <에이전트> TO <Stet 계정>`이 섀도에서도 필요하다(지금의 동작이다. deploy를 따라 하다 문서에 더했다).

**마지막 검토 뒤에 고친 것** (2026-11-23. 해당 장에 옮겨 적었다)

- 8장: 보관을 켜면 원격 업스트림은 `verify-full`이거나 `sslrootcert`를 준 `verify-ca`나 `require`여야 한다. `sslrootcert` 없는 `verify-ca`는 받지 않는다. 거부의 문구도 그렇게 바꿨다.
- 8장: 키만 있고 값이 빈 `listen_tls`와 `agents.credentials`(공백뿐인 값 포함)는 설정 검증에서 거부한다. 없는 것으로 읽지 않는다.
- 8장: 보관을 켜면 섀도에서도 시작할 때 Stet의 계정이 에이전트 계정마다 그 역할의 구성원인지 보고 경고한다(enforce와 같은 문구). 보관이 없는 섀도는 그대로다.

**남긴 것**

- 가용성: 상태 확인, 세션을 기다리는 종료, 장애 넘김, 여러 인스턴스.
- Stet이 DB 계정의 비밀번호를 만들고 돌리기(2장의 2). 바깥 비밀 보관소.
- 계정 하나에 토큰 여럿(겹쳐 쓰는 교체), 다시 띄우지 않고 계정 더하기.
- 클라이언트 쪽 채널 바인딩, 클라이언트 인증서로 하는 에이전트 인증.
- 로그인 기록 테이블, 로그인 횟수 제한.
- 업스트림 연결 풀.
- Stet을 거치지 않고 게이트 대상 테이블에 쓰는 접속을 찾아 보여 주기(concept-v0.2 11장).
- 확인하지 못한 것: 매니지드 서비스(Neon, Supabase, RDS)에서의 처음부터 끝까지, psycopg의 로그인, Linux의 Docker Engine에서의 파일 소유자, systemd 단위 파일, Kubernetes의 비밀 볼륨(deploy 11~13장).
- 첫 부분이 남긴 것(공개, 무료와 유료의 경계, `search_path`, 연결 풀러, PostgreSQL 14 미만, Windows).
