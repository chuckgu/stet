# Stet을 서버에 놓기

에이전트 여럿이 Stet을 거쳐야만 DB에 닿게 하는 순서다. Stet을 서버 한 대에 띄우고, 에이전트에게는 DB의 비밀번호 대신 Stet이 내준 토큰을 준다. 진짜 비밀번호는 Stet만 쥔다.

[quickstart](quickstart.md)의 길은 같은 기기에서 실수를 보여 주는 데까지였다(그 문서 10장의 "통제가 아니다"). 이 문서는 그다음이다. 설계는 [spec-v0.2-server](spec-v0.2-server.md)에 있다. 판정, 기록, 승인, 되돌리기는 바뀌지 않는다. enforce로 돌릴 때 DB에서 해 둘 일은 [spec-v0.2-enforce](spec-v0.2-enforce.md) 8장 그대로다.

필요한 것:

- Stet을 띄울 Linux 서버 한 대. 에이전트가 거기에 닿고, 거기서 DB에 닿는다.
- 에이전트마다 따로 있는 DB 계정과 그 비밀번호. Stet 자신의 계정(`upstream`의 계정)은 에이전트 계정과 달라야 한다.
- Stet 자신의 계정이 에이전트 계정마다 그 역할의 구성원일 것. DB에서 에이전트 계정마다 한 번 실행한다.

  ```sql
  GRANT agent_enricher TO stet;
  ```

  여러 행에 쓰는 쓰기의 영향 측정은 섀도에서도 에이전트의 역할로 돈다. 빠지면 그 쓰기는 `unsupported`(`not_single_row`)로 기록되고 로그에 `impact not measured: Stet's account is not a member of the client's role`이 남는다. enforce에서는 승인된 쓰기도 그 역할로 실행한다(spec-v0.2-enforce 8장). quickstart의 길(계정 하나)에서는 필요 없던 것이다.
- 에이전트가 Stet의 이름으로 확인할 수 있는 TLS 인증서와 키(3장).

## 1. 무엇이 달라지는가

```
에이전트 ──TLS, 계정 + 토큰──▶ Stet ──TLS(확인한 서버), 계정 + 진짜 비밀번호──▶ DB
```

- **에이전트는 토큰만 안다.** 토큰은 `stet_`로 시작하는 48글자다(무작위 32바이트). Stet은 토큰을 저장하지 않고 검증값만 둔다.
- **Stet이 에이전트를 직접 인증한다.** 에이전트가 계정 이름과 토큰으로 SCRAM-SHA-256 로그인을 하면, Stet은 그 계정의 진짜 비밀번호로 DB에 로그인하고 그 세션을 에이전트에게 잇는다. 그 뒤는 지금처럼 중계하고 판단한다. 판단 기록의 `login_role`은 그 계정이다.
- **에이전트와 Stet 사이가 TLS다.** 에이전트는 Stet의 인증서를 `sslmode=verify-full`로 확인한다.
- **에이전트 쪽에서 바꾸는 것은 접속 문자열뿐이다.** 호스트와 포트를 Stet의 것으로, 비밀번호를 토큰으로 바꾸고, `sslmode=verify-full`과 `sslrootcert`를 적는다. 계정 이름과 데이터베이스는 그대로다.
- **Stet은 자격 증명 파일에 있는 계정만 받는다.** 그 밖의 계정은 Stet을 거쳐 로그인하지 못한다. 서비스와 사람은 지금처럼 DB에 직접 붙는다.

자격 증명 보관은 설정에 `agents.credentials`를 적으면 켜진다. 적지 않으면 Stet은 이 단계 전과 같다.

## 2. 놓는 자리와 네트워크

- Stet은 에이전트가 닿을 수 있는 주소에서 받는다(`listen`, 보통 `0.0.0.0:6432`나 서버의 사설 주소). 루프백이 아닌 주소로 받으면 `listen_tls`가 있어야 한다. 없으면 시작하지 않는다(4장).
- `listen_tls`가 있으면, 루프백이 아닌 주소에서 TLS 없이 세션을 시작하려는 클라이언트는 `FATAL`(SQLSTATE `28000`, `stet: this server requires TLS; connect with sslmode=require or stronger`)을 받는다. 루프백(`127.0.0.1`, `::1`)에서 온 접속은 평문도 받는다. 취소 요청은 평문으로도 받는다(libpq 16까지는 취소를 늘 평문으로 보낸다).
- **Stet 앞에 같은 기기의 TCP 전달자를 두지 않는다.** 사이드카 프록시, `ssh -L`, `kubectl port-forward`처럼 같은 기기에서 연결을 넘겨주는 것을 Stet 앞에 두면, 모든 클라이언트가 루프백에서 온 것으로 보여 평문도 받는다.
- Stet은 DB에 TLS로 붙고 서버를 확인해야 한다. `upstream`의 호스트가 루프백이 아니면 `sslmode`가 `verify-full`, `verify-ca`, 또는 `sslrootcert`가 있는 `require`여야 한다. 아니면 시작하지 않는다(4장). 확인하지 않는 연결에서는 중간에 선 누군가가 비밀번호를 평문으로 달라고 할 수 있고, Stet의 DB 클라이언트(pgx)는 그것을 따른다.
- 에이전트가 DB에 직접 닿지 못하게 막을 수 있으면 막는다(방화벽, `pg_hba.conf`). 매니지드 Postgres처럼 막을 수 없으면, 에이전트가 진짜 비밀번호를 모르게 하는 것(8장)이 막는 길이다.
- 연결 풀러를 거친 DB 주소는 지금처럼 받지 않는다(quickstart 10장).

## 3. 인증서

```yaml
listen_tls:
  cert: tls/server.crt
  key: tls/server.key
```

- PEM 파일 둘이다. 인증서 파일에는 중간 인증서를 이어 넣을 수 있다. TLS 1.2 이상만 받는다.
- 인증서는 공개 CA의 것이어도, 사설 CA나 자기 서명의 것이어도 된다. Stet은 받은 파일을 낼 뿐이다. 이 문서는 사설 CA나 자기 서명을 쓰는 경우를 적는다. 그때 에이전트는 그 CA의 인증서를 `sslrootcert`로 받는다.

자기 서명 인증서는 이렇게 만든다. `stet.internal`을 에이전트가 Stet에 붙을 때 쓰는 이름으로 바꾼다.

```sh
(umask 077 && openssl req -x509 -newkey rsa:2048 -nodes -days 365 \
  -subj "/CN=stet.internal" -addext "subjectAltName=DNS:stet.internal" \
  -keyout server.key -out server.crt)
```

- `-addext`는 OpenSSL 1.1.1 이상에 있다. `umask 077`이라 두 파일 모두 `0600`으로 생긴다.
- 에이전트가 IP 주소로 붙는다면 SAN에 그 주소를 `IP:`와 함께 같은 글자의 `DNS:` 항목으로도 넣는다(`subjectAltName=DNS:stet.internal,IP:10.0.0.5,DNS:10.0.0.5`). libpq 14는 IP 항목을 보지 않는다.
- 자기 서명이면 `server.crt`가 곧 에이전트의 `sslrootcert`다. 에이전트에게 그 파일(비밀이 아니다)을 준다. 인증서를 갈 때마다 에이전트에게 새 파일을 줘야 한다.
- 사설 CA가 있으면 그 CA로 서명한 인증서를 쓰고, 에이전트에게는 CA의 인증서를 준다. Stet의 인증서를 갈아도 에이전트 쪽은 그대로다.

Stet이 하는 것:

- 시작할 때 두 파일을 읽지 못하거나 짝이 맞지 않으면 시작하지 않는다(`stet: listen_tls: …`).
- 키 파일을 소유자 말고 다른 사용자가 읽을 수 있으면 경고한다: `listen_tls.key <경로> gives users other than its owner access (mode 0640): whoever can read it can pose as stet to the agents; fix: chmod 600 <경로>`
- 두 파일 가운데 하나라도 바뀌면(바뀐 시각, 크기, 권한, 파일 자체) 다음 TLS 인사에서 다시 읽고, 그 인사부터 새 인증서를 쓴다. 로그에 `read listen_tls again; serving the new certificate from the next handshake`가 남는다. 다시 읽지 못하면(반쯤 쓰인 파일, 짝이 맞지 않는 두 파일) 쓰던 인증서를 계속 쓰고 `could not read listen_tls again; serving the certificate in use`를 에러로 남긴다. 인증서를 갈 때 Stet을 다시 띄우지 않아도 된다. 두 파일을 함께 바꾼다.

`listen_tls`는 자격 증명 보관(`agents.credentials`) 없이도 켤 수 있다. 그때 인증은 지금처럼 중계한다. `upstream`도 TLS면 SCRAM 계정은 클라이언트가 `channel_binding=disable`을 적어야 로그인한다. 클라이언트가 SCRAM을 Stet의 인증서에 묶고, 서버는 자기 인증서로 확인하기 때문이다. md5와 `password` 계정은 그대로 로그인한다. Stet은 시작할 때 이 조합을 경고한다(`listen_tls is set without agents.credentials and the upstream's sslmode asks for TLS: …`). 이 문서의 길은 보관을 켠다.

## 4. 설정

보관을 켠 설정의 예다. 다른 기기의 매니지드 Postgres에 붙고, 모든 주소에서 받는다.

```yaml
mode: shadow
listen: 0.0.0.0:6432
listen_tls:
  cert: tls/server.crt
  key: tls/server.key
upstream: postgres://stet:<password>@db.example.com:5432/crm?sslmode=verify-full&sslrootcert=/etc/stet/db-ca.crt
agents:
  credentials: agents.yaml
model:
  adapter: none
tables:
  contacts:
    pk: id
    impact:
      max_rows: 100
```

- **`agents.credentials`**: 자격 증명 파일의 경로다(5장). 이 키가 있으면 보관이 켜진다.
- **상대 경로.** `agents.credentials`, `listen_tls.cert`, `listen_tls.key`의 상대 경로는 설정 파일이 있는 디렉터리에 대해 읽는다. `upstream`의 `sslrootcert`는 그렇지 않다. Stet이 도는 작업 디렉터리에 대해 읽으므로 절대 경로로 적는다. `~`는 풀지 않는다.
- **`upstream`.** 주소의 계정은 Stet 자신의 연결(상태 조회, 판단 기록, 승인된 쓰기의 실행)에 쓴다. 지금과 같다. 설정 파일에 그 비밀번호가 있으면 파일을 `0600`으로 둔다. 서비스의 CA가 공개 CA가 아니면 그 CA 파일을 `sslrootcert`로 준다.
- **`agents.roles`** 는 적지 않아도 된다. 에이전트 계정은 자격 증명 파일의 계정들이다. enforce에서도 그렇다. 적었으면 파일의 계정들과 같아야 하고, 다르면 시작하지 않는다: `agents.roles and the credentials file name different accounts: only in agents.roles: ["…"]; only in the file: ["…"]`

**보관이 요구하는 것 둘.** 설정을 읽을 때 본다. 어기면 시작하지 않는다.

- `upstream`의 호스트가 루프백이 아니면 서버를 확인해야 한다(`verify-full`, `verify-ca`, `sslrootcert`가 있는 `require`): `upstream: with agents.credentials the server must be verified, or stet could hand a database password to whoever answers; use sslmode=verify-full (and sslrootcert=<CA file> when the server's CA is not a public one)`
- `listen`이 루프백이 아니면 `listen_tls`가 있어야 한다: `listen: with agents.credentials a listen address that is not loopback needs listen_tls: on a plaintext connection whoever sits in between can take over a session once the agent has logged in`

`verify-full`은 서버 인증서의 발급자와 호스트 이름을 확인한다. `verify-ca`(그리고 `sslrootcert`가 있는 `require`)는 발급자만 확인한다. 인증서가 접속하는 호스트의 이름을 담고 있으면 `verify-full`을 쓴다. 이름이 맞지 않는 경우(예: 같은 기기의 DB에 컨테이너에서 `host.docker.internal`로 붙는데 인증서에는 `localhost`만 있을 때)에만 `verify-ca`를 쓴다.

**시작할 때**(`stet proxy`) 보관에 대해 하는 것:

1. 자격 증명 파일의 권한과 모양을 본다(5장). 읽지 못하면 시작하지 않는다(`stet: agents.credentials: …`). 계정이 하나도 없으면 시작하지 않는다(`the credentials file has no account; add one with stet agent add`).
2. Stet 자신의 계정(`upstream`의 계정. 주소에 없으면 pgx가 정하는 대로 `PGUSER`나 Stet을 돌리는 사용자)이 파일에 있으면 시작하지 않는다: `agents.credentials: "<계정>" is Stet's own account (the upstream user)`. 그 토큰을 가진 쪽은 게이트가 보지 않는 세션을 얻기 때문이다.
3. `agents.roles`가 있으면 파일의 계정들과 비교한다.
4. Stet 자신의 연결로 DB를 확인한 뒤, 계정마다 DB에 한 번 로그인해 본다. 계정마다 5초, 합쳐 30초까지다. 되지 않는 계정은 경고하고 시작은 한다. 그 계정의 로그인은 거절되고, 다른 계정은 일한다(10장).
5. 받기 시작하는 로그 줄에 `listen_tls=on custody=on`이 붙는다. `agent_roles`는 파일의 계정들이다.

## 5. 자격 증명 파일

`stet agent`가 쓴다. 손으로 고쳐도 된다.

```yaml
# Written by stet agent. It holds database passwords: keep it private.
mock_secret: <무작위 32바이트, base64>
agents:
    agent_enricher:
        token: SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>
        password: <진짜 비밀번호>
        added: 2026-11-23T02:10:41Z
        token_issued: 2026-11-23T02:10:41Z
    agent_support:
        token: SCRAM-SHA-256$4096:<salt>$<StoredKey>:<ServerKey>
        password_env: AGENT_SUPPORT_PASSWORD
        added: 2026-11-23T02:12:05Z
        token_issued: 2026-11-23T02:12:05Z
```

- 계정마다 토큰의 검증값(`token`)과 진짜 비밀번호의 자리 하나가 있다. `password`(값 그대로)나 `password_env`(`stet proxy`의 환경 변수 이름)다. 둘 다 있거나 둘 다 없으면 오류다. 계정 이름은 비어 있지 않고 63바이트 이하다. 대소문자까지 바이트 그대로 비교한다.
- `password_env`의 값은 `stet proxy`가 시작할 때 한 번 읽는다. 비어 있으면 경고한다. 그 비밀번호를 바꾸려면 Stet을 다시 띄워야 한다. 다시 띄우지 않고 바꾸려면 `password`에 둔다.
- **`mock_secret`** 은 비밀번호가 아니다. 없는 계정에 대한 로그인도 같은 모양으로 끝까지 하는데, 그때 쓰는 가짜 검증값을 이 값에서 만든다. 파일에 두는 것은 다시 띄운 뒤에도 같은 가짜 값이 나오게 하기 위해서다. 그래야 다시 띄우기 전후로 같은 이름을 대 봐서 계정이 있는지 알 수 없다. `stet agent`가 파일을 바꿀 때 없으면 만든다. 손으로 쓴 파일에 없으면 Stet은 프로세스마다 무작위 값을 쓰고 경고한다: `the credentials file has no mock_secret: after a restart a login attempt can tell an unknown account from a known one; run a stet agent command that changes the file (add, token, password or remove) to add one`

**권한.** `stet proxy`는 PostgreSQL이 서버 키 파일에 두는 규칙 그대로 본다.

| 파일의 소유자 | 받는 권한 |
|---|---|
| Stet을 돌리는 사용자(실효 uid. root로 돌리면 root) | 그룹과 다른 사용자에게 아무 권한이 없어야 한다(`0600`이나 `0400`) |
| root(Stet은 다른 사용자로 돈다) | 그룹 읽기까지(`0640`, `0440`). 컨테이너에 비밀을 넣는 흔한 방식이다 |
| 그 밖의 사용자 | 받지 않는다 |

어기면 시작하지 않는다. 예: `stet: agents.credentials: credentials file agents.yaml: mode 0640 is too open: allow no access for group or others (0600)`. 일반 파일이 아니거나 1 MiB보다 크면 받지 않는다.

**`stet agent`가 파일을 바꾸는 방식.**

- 파일의 소유자나 root가 돌린다. 그 밖의 사용자는 `the file belongs to uid <uid>: only that user or root may change it (you are uid <uid>)`로 멈춘다. root가 바꾸면 파일의 소유자, 그룹, 권한은 그대로 남는다. 그래서 다른 사용자로 도는 Stet이 바꾼 뒤에도 그 파일을 읽는다.
- 새로 만드는 파일은 돌린 사용자의 `0600`이다. 처음 `add`는 Stet을 돌릴 사용자로 한다(11장, 12장).
- 너무 열린 파일(소유자가 root가 아니면 그룹이나 다른 사용자에게 권한이 있는 것, root면 그룹 읽기보다 넓은 것)은 바꾸지 않고 멈춘다.
- 같은 디렉터리의 임시 파일에 다 쓰고 이름을 바꾼다. 반쯤 쓰인 파일은 남지 않는다. 그래서 **컨테이너에는 파일 하나가 아니라 그 디렉터리를 마운트한다.** 파일 하나를 마운트하면 이름 바꾸기가 되지 않거나 바깥에서 바꾼 것이 안에서 보이지 않는다.
- 바꾸는 동안 파일이 있는 디렉터리에 잠금(`flock`)을 건다. 두 `stet agent`가 동시에 돌아도 서로의 변경을 지우지 않는다. 이 잠금은 한 기기 안에서만 통한다. NFS 같은 네트워크 파일 시스템의 파일을 두 기기에서 바꾸면 서로 막지 못한다.
- 디렉터리를 디스크에 쓰는(sync) 마지막 단계만 실패하면, 파일은 바뀌었으므로 명령은 성공하고 `warning: … the change may not survive a crash …`를 덧붙인다.

**다시 읽기.** `stet proxy`는 로그인마다 파일이 바뀌었는지(바뀐 시각, 크기, 권한, 파일 자체가 갈렸는지) 보고, 바뀌었으면 다시 읽는다.

- 토큰과 `password`를 바꾼 것, 계정을 지운 것은 다음 로그인부터 적용된다. 열려 있는 세션은 그대로 간다.
- 다시 읽지 못하면(모양이 깨짐, 권한이 풀림, 파일이 사라짐) 쓰던 계정들을 그대로 쓰고 `could not read the credentials file again; keeping the accounts as they were`를 에러로 한 번 남긴다. 실패하는 동안은 파일이 그대로여도 로그인 때마다(200ms에 한 번까지) 다시 읽어 본다. 다시 읽히면 `read the credentials file again; following it once more`를 남긴다.
- 띄운 뒤에 파일에 생긴 계정은 받지 않는다. 처음 볼 때 경고한다: `account "<계정>" was added after stet started; restart stet proxy to accept it`

## 6. `stet agent`

```
stet agent add      [-config stet.yaml] [-password-env NAME] <계정>
stet agent token    [-config stet.yaml] <계정>
stet agent password [-config stet.yaml] [-password-env NAME] <계정>
stet agent remove   [-config stet.yaml] <계정>
stet agent list     [-config stet.yaml]
```

설정 파일에서 `upstream`과 `agents.credentials`를 읽는다. `agents.credentials`가 없으면 `the config has no agents.credentials; add it first`로 멈춘다. `stet agent`는 DB의 계정을 만들거나 고치지 않는다. 계정은 DB에 먼저 있어야 한다.

### `add`

```sh
stet agent add -config /etc/stet/stet.yaml agent_enricher
```

```
Database password for "agent_enricher":
token for "agent_enricher" (shown once; stet keeps only what it needs to check it):
  stet_…

next:
  - in the agent's connection string, put this server's address as the host and the token as the password; the user stays "agent_enricher"
  - if anything else knew this account's database password, change it in the database, then run: stet agent password -config /etc/stet/stet.yaml agent_enricher
  - restart stet proxy: an account added while it runs is not accepted
```

1. 묻기 전에 멈추는 경우: Stet 자신의 계정(`"<계정>" is the account stet itself connects as; an agent needs its own`), 이미 있는 계정(`"<계정>" is already there; use stet agent token or stet agent password`), 파일이 들어갈 디렉터리가 없을 때(`agents.credentials: the directory <디렉터리> is not there; create it first`. 디렉터리는 만들지 않는다).
2. 그 계정의 진짜 비밀번호를 받는다. 표준 입력이 터미널이면 화면에 보이지 않게 묻는다. 묻는 글(`Database password for "agent_enricher": `)은 표준 에러로 간다. 터미널이 아니면 표준 입력의 첫 줄을 읽는다. 스크립트에서는 이렇게 준다. `read -rs`로 받으면 화면과 셸 기록에 남지 않는다.

   ```sh
   read -rs PW    # 비밀번호를 치고 Enter
   printf '%s\n' "$PW" | stet agent add -config /etc/stet/stet.yaml agent_enricher
   ```

3. 그 계정과 비밀번호로 DB에 한 번 로그인해 본다. `stet proxy`가 로그인하는 것과 같은 방식(같은 호스트, TLS, 서버 확인)이고, 데이터베이스는 `upstream`의 것이다. 5초를 넘기면 실패다. 되지 않으면 아무것도 쓰지 않는다.

   ```
   stet: the database refused that password for "agent_enricher", so nothing was written: upstream login: wrong password: password authentication failed for user "agent_enricher" (SQLSTATE 28P01)
   ```

   서버에 닿지 못하거나 인증서가 맞지 않으면 `could not log in to the database as "agent_enricher" to check the password, so nothing was written: upstream login: <갈래>`다. 갈래는 `cannot reach the server`, `certificate`, `refused TLS`, `other`이고, 서버가 에러를 보냈으면 그 문구와 SQLSTATE가 붙는다. 주소와 비밀번호는 적지 않는다.
4. 토큰을 만들고, 파일에 검증값과 비밀번호를 쓴다. 파일이 없으면 만든다.
5. 토큰을 표준 출력에 한 번 보인다. 다시 볼 수 없다. 잃어버리면 `stet agent token`으로 새로 만든다. 이어서 할 일을 알린다. 설정에 `agents.roles`가 있으면 거기에도 계정을 더하라고 알린다.

**`add`는 Stet을 다시 띄워야 적용된다.** 띄운 뒤에 더한 계정은 받지 않는다(5장).

`-password-env NAME`이면 비밀번호를 묻지 않는다. 그 환경 변수의 값을 이 명령의 환경에서 읽어 3의 확인에만 쓰고, 파일에는 변수의 이름만 적는다. `stet proxy`의 환경에도 같은 변수가 있어야 한다(출력이 알린다). 변수의 이름이 아닌 것을 주면 `-password-env takes the name of an environment variable, not its value`로 멈추고, 준 글자는 다시 보이지 않는다.

### `token`, `password`, `remove`, `list`

- **`token`**: 토큰을 새로 만들어 한 번 보인다. 예전 토큰은 다음 로그인부터 통하지 않는다. 다시 띄우지 않는다.

  ```
  token for "agent_enricher" (shown once; stet keeps only what it needs to check it):
    stet_…

  the old token stops working at the next login; no restart is needed
  ```

- **`password`**: DB에서 바꾼 새 비밀번호를 받아 `add`의 2, 3을 그대로 한 뒤 적는다. 파일에 둔 비밀번호는 다시 띄우지 않아도 다음 로그인부터 쓰인다(`password for "agent_enricher" updated; no restart is needed`). 비밀번호가 `password_env`인 계정은 `"agent_support" takes its password from AGENT_SUPPORT_PASSWORD; change it in stet proxy's environment and restart stet proxy`라고 알리고 아무것도 바꾸지 않는다. `-password-env NAME`을 주면 그 계정의 비밀번호 자리를 그 변수로 옮긴다(다시 띄워야 한다).
- **`remove`**: 계정을 지운다. 다음 로그인부터 거절한다(`"agent_enricher" removed; it is refused from the next login`). 마지막 계정을 지우면 `the credentials file has no account left: stet proxy will not start until one is added`를 덧붙인다.
- **`list`**: 계정마다 비밀번호의 자리(`file`이나 `env <이름>`), 더한 때, 토큰을 만든 때(UTC, 분까지)를 보인다. 토큰과 비밀번호는 보이지 않는다.

  ```
  ACCOUNT         PASSWORD                    ADDED             TOKEN ISSUED
  agent_enricher  file                        2026-11-23 02:10  2026-11-23 02:10
  agent_support   env AGENT_SUPPORT_PASSWORD  2026-11-23 02:12  2026-11-23 02:12
  ```

비밀번호를 묻는 중에 Ctrl-C를 누르면 터미널을 되돌리고 130으로 끝난다. 토큰은 그것을 만든 `add`나 `token`의 표준 출력에 한 번만 나오고, 토큰의 검증값과 비밀번호는 어느 명령의 출력과 에러에도 나오지 않는다.

## 7. 에이전트의 접속 문자열

`add`가 보인 토큰을 비밀번호 자리에 넣는다. 호스트와 포트는 Stet의 것이다.

```
postgres://agent_enricher:<token>@stet.internal:6432/crm?sslmode=verify-full&sslrootcert=/path/to/stet.crt
```

libpq의 키워드 형식이면 이렇다. 토큰을 `PGPASSWORD`로 줘도 된다.

```
host=stet.internal port=6432 dbname=crm user=agent_enricher password=<token> sslmode=verify-full sslrootcert=/path/to/stet.crt
```

- 토큰의 글자는 영문자, 숫자, `-`, `_`뿐이라 URL에 그대로 넣는다.
- `sslrootcert`는 Stet의 인증서를 확인할 CA의 인증서다(3장). 자기 서명이면 Stet의 `server.crt` 사본이다.
- **`sslmode=verify-full`을 쓴다.** `require`는 TLS로 붙지만 상대가 Stet인지 확인하지 않는다. 그러면 중간에 선 누군가가 Stet인 척하며 로그인을 진짜 Stet에 그대로 흘려보낸 뒤 로그인이 끝난 세션을 차지할 수 있다. 토큰은 SCRAM이라 드러나지 않지만 세션은 넘어간다. Stet은 클라이언트가 어느 `sslmode`로 붙었는지 알 수 없어 이것을 막지 못한다.
- `channel_binding=require`를 적은 클라이언트는 붙지 못한다. Stet은 채널 바인딩 방식(`SCRAM-SHA-256-PLUS`)을 제안하지 않는다. `psql`은 `channel binding is required, but server did not offer an authentication method that supports channel binding`으로 끝난다. 그 파라미터를 뺀다.
- Stet은 프로토콜 3.0으로 답한다. 더 높은 버전을 청한 클라이언트는 3.0으로 내려 받는다.
- 클라이언트의 시작 파라미터(`application_name`, `options` 등)는 DB의 세션에 그대로 간다.

거절될 때 에이전트가 받는 것:

| 경우 | 받는 것 |
|---|---|
| 모르는 계정, 틀린 토큰, 지운 계정, 진짜 비밀번호를 토큰 자리에 넣은 것, 다른 계정의 토큰 | `FATAL` `28P01` `password authentication failed for user "<계정>"` |
| 루프백이 아닌 주소에서 TLS 없이 | `FATAL` `28000` `stet: this server requires TLS; connect with sslmode=require or stronger` |
| `replication` 파라미터가 있는 접속 | `FATAL` `0A000` `stet: replication connections are not supported` |
| 깨진 인증 메시지, SCRAM-SHA-256이 아닌 방식 | `FATAL` `28000` `stet: authentication exchange failed` |
| 토큰은 맞는데 Stet이 DB에 로그인하지 못함 | `FATAL` `08001` `stet: could not open a database session for "<계정>"` |

모르는 계정과 틀린 토큰은 메시지의 수와 모양까지 같다. 왜 DB에 로그인하지 못했는지는 Stet의 로그에만 있다(10장).

**확인한 클라이언트.** `psql`(libpq 14)과 pgx는 테스트와 11장·12장의 따라 하기에서, pgjdbc 42.7.14(Java 17)와 node-postgres 8.17.2(Node 24)는 이 기기에 있던 것으로 `sslmode=verify-full`의 로그인만 해 봤다(2026-11-23). 모두 토큰으로 그 계정의 세션을 얻었고, 틀린 토큰에는 `28P01`을 받았다. psycopg는 이 기기에 없어 해 보지 않았다.

## 8. 예전 비밀번호 바꾸기

계정을 Stet 뒤로 옮겨도, 그 계정의 예전 비밀번호를 아는 에이전트는 DB에 직접 붙어 Stet을 건너뛸 수 있다. 그래서 옮긴 뒤에 DB에서 비밀번호를 바꾼다. Stet은 이것이 됐는지 알 수 없다.

1. `stet agent add`로 계정을 더하고, Stet을 다시 띄우고, 에이전트의 접속 문자열을 토큰으로 바꾼다(6장, 7장).
2. DB에서 그 계정의 비밀번호를 바꾼다. psql이면 관리 계정으로 `\password agent_enricher`다(입력이 화면에 보이지 않고, 서버에는 비밀번호 대신 검증값을 보낸다). 매니지드 서비스면 그 콘솔에서 바꿔도 된다.
3. 새 비밀번호를 Stet에 넣는다. Stet을 다시 띄우지 않는다.

   ```sh
   stet agent password -config /etc/stet/stet.yaml agent_enricher
   ```

2와 3 사이에는 그 계정의 새 로그인이 `08001`(`stet: could not open a database session for "agent_enricher"`)로 거절된다. Stet이 쥔 비밀번호가 아직 예전 것이기 때문이다. 열려 있는 세션은 그대로 간다. 로그에는 `reason="upstream login failed"`와 `wrong password`가 남는다.

새 비밀번호는 운영자와 Stet만 안다. 에이전트가 볼 수 있는 곳(에이전트의 환경 변수, 같은 기기의 파일)에 두지 않는다.

## 9. 돌리기와 다시 띄우기

| 바꾸는 것 | 하는 일 | 다시 띄우기 |
|---|---|---|
| 토큰 | `stet agent token` | 필요 없다. 예전 토큰은 다음 로그인부터 거절된다 |
| 파일에 둔 진짜 비밀번호 | DB에서 바꾸고 `stet agent password` | 필요 없다 |
| `password_env`인 진짜 비밀번호 | DB에서 바꾸고 `stet proxy`의 환경을 고친다 | 필요하다 |
| 계정 지우기 | `stet agent remove` | 필요 없다. 다음 로그인부터 거절된다 |
| 계정 더하기 | `stet agent add` | 필요하다 |
| `listen_tls`의 인증서와 키 | 두 파일을 바꾼다 | 필요 없다. 다음 TLS 인사부터 쓴다 |
| 설정 파일 | 고친다 | 필요하다 |

- 계정 하나에 토큰 하나다. 토큰을 바꾸면 예전 토큰을 쓰던 에이전트는 다음 로그인에서 거절된다. 겹쳐 쓰는 기간이 없다. 새 토큰을 에이전트에게 주는 시점에 맞춰 돌린다.
- 다시 띄우는 동안 에이전트의 접속은 끊긴다. 끌 때는 판단 중인 쓰기를 기록하고 끝난다(SIGTERM이나 Ctrl-C).
- 다시 띄우지 않고 적용되는 것들은 열려 있는 세션에 손대지 않는다. 지운 계정의 열린 세션도 끝날 때까지 간다.

## 10. 로그에서 보는 것

로그는 표준 에러로 간다(quickstart 4장). 계정 이름은 시작 패킷에서 온 글자라 제어 문자를 걸러 적는다. 토큰, 검증값, 진짜 비밀번호는 어느 줄에도 없다(`-v`의 디버그 로그도).

| 줄(`msg`) | 수준 | 뜻 |
|---|---|---|
| `stet proxy listening` … `listen_tls=on custody=on` | INFO | 받기 시작했다. `agent_roles`는 파일의 계정들이다 |
| `agent logged in` `account=… remote=… tls=true` | INFO | 토큰으로 로그인해 DB 세션을 얻었다. `tls`는 에이전트 쪽 구간이 TLS인지다 |
| `agent login refused` `account=… remote=… reason=…` | WARN | 거절했다. `reason`은 아래 다섯 가운데 하나다 |
| `an account of the credentials file cannot log in to the database: its agent's logins are refused until it can` | WARN | 시작할 때의 확인에서 그 계정으로 DB에 로그인하지 못했다. `err`에 갈래가 있다 |
| `the start ran out of time to log in as these accounts, so they are not checked` | WARN | 시작할 때의 확인이 30초를 넘겼다. 확인하지 못한 계정들을 적는다 |
| `the environment variable for an account's password is empty or not set` | WARN | `password_env`의 변수가 비었다 |
| `account "…" was added after stet started; restart stet proxy to accept it` | WARN | 띄운 뒤에 파일에 생긴 계정이다 |
| `could not read the credentials file again; keeping the accounts as they were` | ERROR | 다시 읽지 못했다. 고쳐지면 `read the credentials file again; following it once more`(INFO) |
| `read listen_tls again; serving the new certificate from the next handshake` | INFO | 인증서를 갈았다 |
| `could not read listen_tls again; serving the certificate in use` | ERROR | 바뀐 인증서 파일을 읽지 못했다 |

`agent login refused`의 `reason`:

- `unknown account`: 파일에 없는 계정이다.
- `bad token`: 토큰이 틀렸다.
- `upstream login failed`: 토큰은 맞았고, Stet이 DB에 로그인하지 못했다. `err`에 갈래(`wrong password`, `cannot reach the server`, `certificate`, `refused TLS`, `other`)와 서버의 문구가 있다. 에이전트는 `08001`만 받는다.
- `exchange failed`: 인증 메시지가 깨졌거나, 클라이언트가 10초 안에 교환을 끝내지 않았다. 시간을 넘긴 경우에는 클라이언트에 `FATAL`을 보내지 않는다.
- `tls required`: 루프백이 아닌 주소에서 TLS 없이 시작했다.

로그인 도중에 클라이언트가 끊은 것(`psql`은 비밀번호 없이 먼저 붙어 보고 끊는다)과 복제 접속을 거절한 것은 디버그(`-v`)에만 남는다.

## 11. 놓는 법 1: 서비스 계정으로 돌리는 바이너리

Linux 서버에 서비스 계정 `stet`을 만들고, 설정과 비밀을 `/etc/stet`에 둔다. 바이너리는 quickstart 2장의 묶음이나 `go build`로 얻는다.

**서비스 계정과 디렉터리.**

```sh
sudo useradd --system --no-create-home --shell /usr/sbin/nologin stet
sudo install -d -m 0700 -o stet -g stet /etc/stet /etc/stet/tls
sudo install -m 0755 stet /usr/local/bin/stet
```

**인증서, DB의 CA, 설정.** 3장에서 만든 `server.crt`와 `server.key`, DB 서버의 CA 파일, 4장의 설정을 놓는다.

```sh
sudo install -m 0600 -o stet -g stet server.crt server.key /etc/stet/tls/
sudo install -m 0644 -o stet -g stet db-ca.crt /etc/stet/
sudo install -m 0600 -o stet -g stet stet.yaml /etc/stet/
```

**계정 더하기.** 서비스 계정으로 돌린다. 처음 `add`가 만드는 자격 증명 파일이 `stet`의 `0600`이 되어야 Stet이 읽는다.

```sh
sudo -u stet stet agent add -config /etc/stet/stet.yaml agent_enricher
```

터미널이 아니면 6장처럼 표준 입력의 첫 줄로 준다(`printf '%s\n' "$PW" | sudo -u stet stet agent add …`). 파일이 생긴 뒤에는 root(`sudo stet agent …`)로 바꿔도 소유자와 권한이 그대로 남는다.

**띄우기.** 먼저 손으로 띄워 시작 줄과 경고를 본다.

```sh
sudo -u stet stet proxy -config /etc/stet/stet.yaml
```

```
time=… level=INFO msg="stet proxy listening" listen=[::]:6432 upstream=db.example.com:5432 upstream_tls=verified mode=shadow agent_roles=[agent_enricher] gated_tables=1 model=none listen_tls=on custody=on
```

표준 출력이 터미널이면 판단한 쓰기마다 한 줄이 함께 나온다(quickstart 6장). 서비스로 돌리면 표준 출력이 터미널이 아니라 나오지 않는다. 늘 내려면 `-decisions on`을 준다. 판단은 어느 쪽이든 `stet.decisions`에 남는다.

systemd로 돌리는 단위 파일의 예다. **돌려 보지 않았다**(이 문서를 따라 한 기기는 macOS다).

```ini
# /etc/systemd/system/stet.service
[Unit]
Description=Stet
After=network-online.target
Wants=network-online.target

[Service]
User=stet
Group=stet
ExecStart=/usr/local/bin/stet proxy -config /etc/stet/stet.yaml
Restart=on-failure
# password_env를 쓰는 계정이 있으면 그 변수를 root만 읽는 파일에 두고 넘긴다
# EnvironmentFile=/etc/stet/env

[Install]
WantedBy=multi-user.target
```

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now stet
sudo journalctl -u stet -f
```

**토큰 돌리기.** 다시 띄우지 않는다.

```sh
sudo -u stet stet agent token -config /etc/stet/stet.yaml agent_enricher
```

계정을 더한 뒤에는 다시 띄운다(`sudo systemctl restart stet`).

**확인한 것** (2026-11-23, macOS, PostgreSQL 14.18의 시험 서버). "필요한 것"의 `GRANT`부터 이 장의 순서를 그대로 쳤다. 달랐던 것: 서비스 계정, `sudo`, systemd 없이 이 기기의 사용자로 돌렸고, `/etc/stet` 대신 빈 디렉터리와 `go build`한 바이너리를 썼다. `listen`은 `127.0.0.1:16432`, 인증서의 이름은 `localhost`와 `127.0.0.1`, `upstream`은 같은 기기의 시험 서버(`verify-full`)였고, `stet proxy`는 `-decisions on`으로 띄웠다. 비밀번호는 `printf … |`로 줬다. 토큰만 아는 `psql`이 `verify-full`로 붙어 보낸 쓰기 셋이 `commit`, `hold`, `hold`로 그 계정(`login_role`)으로 기록됐다. `stet agent token` 뒤에 예전 토큰은 다시 띄우지 않고 거절됐고, 예전 토큰으로 열어 둔 세션은 계속 갔다. 8장의 비밀번호 바꾸기도 적은 대로였다(`\password` 대신 `ALTER ROLE`로 바꿨다). 처음 따라 했을 때는 "필요한 것"에 `GRANT`가 없어 여러 행의 쓰기가 `unsupported`로 기록됐고, 그 줄을 더한 뒤 처음부터 다시 했다.

## 12. 놓는 법 2: 디렉터리를 마운트한 Docker 이미지

이미지는 quickstart 2장의 `docker build -t stet:local .`로 만든다. 게시된 이미지는 없다. 이미지는 root가 아닌 사용자(uid 65532)로 돌고 셸이 없다.

**누가 파일을 갖는가.** 컨테이너 안의 Stet은 uid 65532다. 그래서 마운트할 디렉터리와 그 안의 파일을 호스트에서 uid 65532의 것으로 둔다. Linux의 Docker Engine에서 바인드 마운트는 호스트의 uid를 그대로 보인다. 자격 증명 파일은 `0600`이어야 하고(5장), `stet agent`는 같은 uid로 같은 디렉터리를 마운트한 컨테이너에서 돌린다. 그래야 새로 만드는 파일도 65532의 것이 된다.

```sh
sudo install -d -m 0700 -o 65532 -g 65532 /srv/stet /srv/stet/tls
sudo install -m 0600 -o 65532 -g 65532 server.crt server.key /srv/stet/tls/
sudo install -m 0644 -o 65532 -g 65532 db-ca.crt /srv/stet/
sudo install -m 0600 -o 65532 -g 65532 stet.yaml /srv/stet/
```

`stet.yaml`의 경로는 컨테이너 안의 것으로 적는다. `/srv/stet`을 `/etc/stet`에 마운트하므로 4장의 예가 그대로 맞는다(`sslrootcert=/etc/stet/db-ca.crt`). 컨테이너 안에서는 모든 주소(`0.0.0.0:6432`)로 받아야 바깥에 연 포트가 닿으므로 `listen_tls`가 반드시 있다.

**계정 더하기.** 같은 이미지로 `stet agent`를 돌린다. `-it`면 비밀번호를 화면에 보이지 않게 묻는다. 스크립트에서는 `-t` 없이 `-i`로 표준 입력의 첫 줄을 준다.

```sh
docker run --rm -it -v /srv/stet:/etc/stet stet:local agent add -config /etc/stet/stet.yaml agent_enricher
```

```sh
read -rs PW
printf '%s\n' "$PW" | docker run --rm -i -v /srv/stet:/etc/stet stet:local agent add -config /etc/stet/stet.yaml agent_enricher
```

`add`는 DB에 한 번 로그인해 보므로 이 컨테이너에서도 DB에 닿아야 한다.

**띄우기.** Stet은 파일을 읽기만 하므로 읽기 전용(`:ro`)으로 마운트한다. 디렉터리째 마운트해야 `stet agent`가 바꾼 파일이 다시 띄우지 않고 보인다(5장).

```sh
docker run -d --name stet --restart unless-stopped -v /srv/stet:/etc/stet:ro -p 6432:6432 stet:local proxy -config /etc/stet/stet.yaml
docker logs -f stet
```

- `-p 6432:6432`는 서버의 모든 주소에서 받는다. 에이전트가 다른 기기에서 오므로 그렇게 연다. 컨테이너 밖에서 온 접속은 컨테이너 안에서 루프백이 아니므로, TLS 없이 오면 `28000`으로 거절된다(2장).
- 판단 줄을 `docker logs`에서 보려면 끝에 `-decisions on`을 더한다. 컨테이너 안의 시각은 UTC다(`-e TZ=Asia/Seoul`로 바꾼다).
- `password_env`를 쓰면 그 변수를 `docker run -e`나 `--env-file`로 준다. 바꾸면 컨테이너를 다시 만든다.

**토큰 돌리기.** 도는 컨테이너는 그대로 둔다.

```sh
docker run --rm -v /srv/stet:/etc/stet stet:local agent token -config /etc/stet/stet.yaml agent_enricher
```

계정을 더한 뒤에는 `docker restart stet`.

**확인한 것** (2026-11-23, macOS의 Docker Desktop 28.0.1, 이 기기에서 빌드한 이미지). "필요한 것"의 `GRANT`부터 이 장의 명령을 그대로 쳤다(`add`는 `printf … | docker run --rm -i …`의 형식으로). 달랐던 것: 태그는 `stet:deploy-test`, 컨테이너 이름은 `dp-stet`, 디렉터리는 `/srv/stet` 대신 빈 디렉터리였고, `sudo install -o 65532 -g 65532` 대신 이 기기의 사용자로 파일을 놓았다. 포트는 `127.0.0.1:16433`에만 열었고, `-decisions on`을 더했다. `upstream`은 같은 기기의 시험 서버를 `host.docker.internal`로 가리켜 `verify-ca`로 확인했다(그 서버의 인증서는 `localhost`와 `127.0.0.1`만 담는다). 토큰으로 붙은 `psql`의 쓰기가 판단되어 그 계정으로 기록됐고, 평문 접속은 `28000`으로 거절됐다(`reason="tls required"`). 다른 컨테이너에서 돌린 `stet agent token` 뒤에, 읽기 전용으로 마운트한 채 도는 컨테이너가 다시 띄우지 않고(시작 시각과 재시작 수가 그대로) 예전 토큰을 거절하고 새 토큰을 받았다.

**확인하지 못한 것.** Docker Desktop(macOS)에서는 바인드 마운트한 파일이 컨테이너 안에서 컨테이너를 돌린 사용자의 것으로 보였다(호스트에서 이 기기의 사용자 파일이 컨테이너 안에서는 uid 65532의 것). 그래서 위의 소유자 설정이 맞는지, 틀리면 Stet이 거부하는지는 이 기기에서 시험되지 않는다. Linux의 Docker Engine에서는 돌려 보지 않았다. `-it`로 묻는 형식도 돌려 보지 않았다(묻는 동작은 테스트로만 확인했다).

## 13. Kubernetes의 비밀 볼륨

**돌려 보지 않았다.** 놓는 모양만 적는다.

- 설정, 인증서와 키, DB의 CA, 자격 증명 파일을 비밀(Secret) 하나에 넣고, 그 비밀을 디렉터리로 마운트한다(`/etc/stet`). `subPath`로 파일 하나씩 마운트하지 않는다. `subPath`는 비밀이 바뀌어도 갱신되지 않는다.
- 비밀의 파일은 서로 다른 디렉터리에 둘 수 없으므로 설정의 경로도 평평하게 적는다(`cert: server.crt`, `key: server.key`, `credentials: agents.yaml`, `sslrootcert=/etc/stet/db-ca.crt`).
- 파드는 uid 65532로 돌리고 `fsGroup: 65532`를 준다. 비밀 볼륨의 파일은 root가 소유하고 `fsGroup`의 그룹이 된다(Kubernetes의 동작이다). Stet은 root가 소유한 그룹 읽기 파일(`0640`, `0440`)을 받는다(5장). 그래서 `defaultMode: 0440`을 준다. 이 모양에서는 키 파일을 그룹이 읽을 수 있으므로 시작할 때 `listen_tls.key … gives users other than its owner access (mode 0440)` 경고가 나온다. 키를 소유자만 읽게 하면 uid 65532가 읽지 못한다.
- 비밀을 바꾸면 kubelet이 마운트한 파일을 바꾼다(곧바로는 아니다). Stet은 다음 로그인에서 바뀐 것을 본다. 토큰, 파일에 둔 비밀번호, 지운 계정, 인증서는 다시 띄우지 않고 적용된다. 계정을 더한 뒤에는 `kubectl rollout restart`로 다시 띄운다.
- 비밀 볼륨은 읽기 전용이라 `stet agent`가 그 자리에서 쓰지 못한다. `stet agent`는 DB에 닿는 곳에서 자격 증명 파일의 사본에 돌리고, 그 사본으로 비밀을 바꾼다. 사본에는 모든 진짜 비밀번호가 있다. 다 쓰면 지운다.
- `replicas: 1`로 둔다. 여러 인스턴스와 가용성은 이 단계의 범위가 아니다.
- 서비스(Service)로 받으면 에이전트의 주소는 파드 밖의 것이라 TLS가 필요하다. `kubectl port-forward`로 붙으면 Stet에는 루프백으로 보여 평문도 받는다(2장).

```yaml
spec:
  replicas: 1
  template:
    spec:
      securityContext:
        runAsUser: 65532
        runAsGroup: 65532
        fsGroup: 65532
      containers:
        - name: stet
          image: <레지스트리>/stet:<버전>
          args: ["proxy", "-config", "/etc/stet/stet.yaml"]
          ports:
            - containerPort: 6432
          volumeMounts:
            - name: stet
              mountPath: /etc/stet
              readOnly: true
      volumes:
        - name: stet
          secret:
            secretName: stet
            defaultMode: 0440
```

## 14. 한계

- **진짜 비밀번호가 Stet 서버에 있다.** 파일이나 환경 변수에 평문으로 있고, 프로세스의 메모리에 있다. Stet 서버가 뚫리면 에이전트 계정의 비밀번호가 모두 나간다. 운영자도 그 값을 안다.
- **예전 비밀번호를 아는 에이전트는 건너뛸 수 있다.** 계정을 Stet 뒤로 옮길 때 DB에서 비밀번호를 바꿔야 한다(8장). Stet은 그것이 됐는지 알 수 없다.
- **운영 규칙은 그대로 전제다.** 누군가 에이전트에게 서비스나 사람의 자격 증명을 주면 Stet은 그 쓰기를 보지 못한다.
- **채널 바인딩을 요구하는 클라이언트는 붙지 못한다.** 에이전트는 Stet의 인증서를 `sslmode=verify-full`로 확인한다.
- **에이전트가 `sslmode=require`로 붙으면 Stet이 진짜인지 확인하지 않는다.** 중간에 선 누군가가 로그인을 그대로 흘려보낸 뒤 세션을 차지할 수 있다. Stet은 클라이언트의 `sslmode`를 알 수 없어 막지 못한다. `verify-full`을 쓴다(7장).
- **루프백은 평문도 받는다.** 같은 기기의 TCP 전달자를 Stet 앞에 두면 모든 클라이언트가 루프백으로 보인다(2장).
- **`password_env`인 계정의 진짜 비밀번호를 바꾸려면 다시 띄워야 한다.**
- **계정 하나에 토큰 하나다.** 토큰을 바꾸면 예전 토큰을 쓰던 에이전트는 다음 로그인에서 거절된다. 겹쳐 쓰는 기간이 없다.
- **계정을 더하면 다시 띄워야 한다.** 그동안 에이전트의 접속이 끊긴다. 가용성은 다음 단계다.
- **로그인이 한 번 더 든다.** 세션을 열 때 Stet이 DB에 로그인하는 시간이 더해진다. 시험 서버에서 한 번의 로그인은 직접 TLS로 붙을 때 중앙값 8.5ms, 양쪽이 TLS인 Stet을 거쳐 17.8ms였다. 세션을 자주 여닫는 클라이언트에서 보인다. 연결 풀은 만들지 않는다. 로그인 뒤의 중계는 지연 예산(2ms) 안이다.
- **Stet은 프로토콜 3.0으로 답한다.** 더 높은 버전을 청한 클라이언트는 3.0으로 내려 받는다.
- **`stet agent`의 잠금은 한 기기 안에서만 통한다.** 네트워크 파일 시스템의 자격 증명 파일을 두 기기에서 바꾸면 서로 막지 못한다.
- **실제 매니지드 서비스에서 확인하지 못했다.** 시험과 이 문서의 따라 하기는 로컬의 TLS 서버로 했다.
- **연결 풀러, 스키마를 적은 테이블 이름 등 quickstart의 한계는 그대로다**(quickstart 10장. "통제가 아니다"와 "에이전트와 Stet 사이는 평문이다"는 이 문서의 길에는 해당하지 않는다).
