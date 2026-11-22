# Stet 써 보기

한 사람이 자기 PC에서 자기 DB에 Stet을 붙여 보는 순서다. 코딩 에이전트(Claude Code 등)가 개발 DB나 스테이징 DB에 보낸 쓰기가 실제로 몇 행에 닿았고 어디로 번졌는지를, DB가 답한 숫자로 터미널에서 본다.

이 길의 Stet은 섀도 모드, 모델 없음, DB 계정 하나로 돈다. 아무것도 막지 않는다. API 키가 필요 없고, 데이터가 DB 밖으로 나가지 않는다. 통제가 아니라 보는 도구다(10장). 설계는 [spec-v0.2-tryout.md](spec-v0.2-tryout.md)에 있다.

필요한 것:

- macOS나 Linux.
- PostgreSQL 14 이상. 연결 풀러를 거치지 않는 직접 접속 주소.
- 그 데이터베이스에 `stet` 스키마를 만들 수 있는 계정. 스키마가 이미 있으면 그것을 쓸 수 있는 계정.

## 1. 무엇을 보게 되는가

에이전트가 쓰기를 보낼 때마다 `stet proxy`가 터미널에 한 줄을 낸다. 7장의 빈 DB에서 쓰기 셋을 보내면 이렇게 나온다.

```
stet: watching 2 tables on 127.0.0.1:6432 (shadow: nothing is blocked)
20:39:42  ok            UPDATE contacts  1 row, fresh read  update contacts set status = 'customer' where id = 7
20:39:45  would hold    UPDATE contacts  300 rows, ~30% of table  [impact:rows]  update contacts set status = 'churned' where company_id < 3
20:39:48  would hold    DELETE contacts  200 rows, ~20% of table, cascades into deals  [impact:rows]  delete from contacts where id between 1 and 200
```

- 첫 줄: 한 행을 고쳤다. 에이전트가 그 행을 읽은 뒤로 행이 바뀌지 않았다. 걸리는 것이 없다(`ok`).
- 둘째 줄: 300행, 테이블의 약 30%를 고쳤다. 한 문장의 행 수 한도(100)를 넘었으므로 enforce였다면 보류했을 쓰기다(`would hold`).
- 셋째 줄: 200행을 지웠고, 그 삭제는 외래 키를 따라 `deals`로 번진다.

섀도 모드라 셋 다 이미 실행됐다. Stet은 기록하고 보여 줄 뿐이다.

## 2. 설치

게시된 릴리스, brew 포뮬러, 레지스트리에 올린 이미지는 없다. 아래 셋 가운데 하나로 얻는다. 셋 다 이 저장소에서 시작한다.

### 묶음

`scripts/release.sh`가 이 기기의 OS와 아키텍처용 묶음을 `dist/`에 만든다. Go 1.27.3 이상, C 컴파일러, git이 있어야 한다. SQL 파서가 C 코드라 cgo가 필요하고, 그래서 다른 OS용 묶음은 그 OS에서 만든다.

```sh
scripts/release.sh 0.2.0
```

`dist/stet_0.2.0_<os>_<arch>.tar.gz`(예: `stet_0.2.0_darwin_arm64.tar.gz`)를 만들고 `dist/checksums.txt`에 SHA-256을 더한 뒤, 만든 파일의 경로를 한 줄로 낸다. 묶음 안에는 `stet_0.2.0_<os>_<arch>/` 디렉터리 하나가 있고, 그 안에 `stet`, `LICENSE`, `README.md`, `stet.example.yaml`이 있다.

묶음과 `checksums.txt`가 있는 디렉터리(`dist/`, 또는 묶음을 만든 사람에게서 둘을 받은 곳)에서 확인하고 푼다.

```sh
shasum -a 256 --ignore-missing -c checksums.txt    # Linux에서는 sha256sum --ignore-missing -c checksums.txt
tar -xzf stet_0.2.0_darwin_arm64.tar.gz
export PATH="$PWD/stet_0.2.0_darwin_arm64:$PATH"
stet version
```

```
stet_0.2.0_darwin_arm64.tar.gz: OK
stet 0.2.0 (commit 23b4da6d9e99, go1.27.3)
```

`export`는 그 셸에서만 효력이 있다. 늘 쓰려면 `stet`을 `PATH`에 있는 디렉터리(예: `/usr/local/bin`)로 옮긴다.

### Docker

```sh
docker build -t stet:local .
```

빌드 단계는 `golang:1.27-bookworm`, 실행 이미지는 `gcr.io/distroless/base-debian12:nonroot`이다. 셸이 없고, root가 아닌 사용자(65532)로 돌고, 진입점은 `stet`이다. `--build-arg VERSION=0.2.0`을 주면 `stet version`에 그 버전이 찍히고, 주지 않으면 `dev`다. 띄우는 법은 4장 끝에 있다.

### 소스에서

```sh
go build -o bin/stet ./cmd/stet
```

필요한 것은 묶음과 같다(Go, C 컴파일러). 버전은 `dev`로 찍힌다.

## 3. `stet init`

`stet init`은 DB를 읽어 설정 파일 `stet.yaml`을 쓰고, 에이전트에게 줄 접속 주소를 알려 준다. DB에는 아무것도 쓰지 않는다. 읽기 전용 트랜잭션에서 묻기만 한다.

DB 주소는 환경 변수 `DATABASE_URL`에서 읽는다. 셸에 이미 있으면 첫 줄은 건너뛴다.

```sh
read -rs DATABASE_URL && export DATABASE_URL    # 주소를 붙여 넣고 Enter. 화면에 보이지 않는다
stet init
```

주소를 마지막 인자로 줄 수도 있다(`stet init 'postgres://…'`). 권하지 않는다. 인자는 실행되는 동안 프로세스 목록(`ps`)에 보이고 셸 기록에 남는다. 비밀번호도 함께다. `export DATABASE_URL='postgres://…'`로 적어도 그 줄이 셸 기록에 남는다. `read -rs`로 받으면 화면과 셸 기록에 남지 않는다.

주소의 모양은 `postgres://계정:비밀번호@호스트:포트/데이터베이스`다(`postgresql://`도 받는다). 비밀번호의 `@ : / ? # %`는 퍼센트 인코딩한다.

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

**묻는 것.** 데이터베이스에 `stet` 스키마가 없으면, `stet proxy`를 처음 띄울 때 그 스키마가 어느 계정으로 생기는지, 그 밖에는 아무것도 바꾸지 않는다는 것, 지우는 문장을 보이고 계속할지 묻는다. `y`나 `yes`(대소문자 무관)면 계속하고, 그 밖의 답은 `stopped; nothing was written`으로 멈춘다. 스키마가 이미 있으면 묻지 않는다.

**`-yes`.** 묻지 않고 쓴다. 묻는 것은 `stet` 스키마가 없을 때뿐이다. 그때 `-yes`가 없으면, 사람이 답할 수 없는 표준 입력에서는 둘 가운데 하나로 멈춘다.

- 표준 입력이 파이프나 파일이면 묻지 못하고 `this is not a terminal, so stet init cannot ask; read the lines above and pass -yes to continue`로 멈춘다.
- 표준 입력이 `/dev/null`이면(CI 러너, `-i` 없는 `docker run`에서 흔하다) Stet은 문자 장치를 터미널로 보므로 묻는다. 그리고 입력의 끝(EOF)을 "아니오"로 읽어 `stopped; nothing was written`으로 멈춘다.

어느 쪽이든 아무것도 쓰지 않는다. 위의 안내를 읽고 `-yes`를 준다.

**`-tables a,b,c`.** 그 테이블만 넣는다. 그 가운데 아래 조건에 맞지 않는 것이 있으면 테이블마다 이유를 대고 멈춘다(예: `table "nosuch": not found`). 이때는 100개 상한이 없다.

그 밖의 플래그: `-config`(쓸 파일, 기본 `stet.yaml`), `-listen`(`stet proxy`가 받을 주소, 기본 `127.0.0.1:6432`), `-force`(파일이 있으면 덮어쓴다). 플래그는 주소 앞에 둔다. 주소 뒤에 오면 `flags go before the address`로 멈춘다. `stet init -h`는 플래그를 모두 보인다.

### 하는 일

1. **주소를 정한다.**
   - 주소에 `sslmode`가 적혀 있으면 그대로 쓴다. `sslmode=allow`는 받지 않는다.
   - 적혀 있지 않고 호스트가 이 기기(`localhost`나 루프백 IP)면 주소를 그대로 둔다.
   - 적혀 있지 않고 호스트가 다른 기기면 `sslmode=verify-full`(시스템의 루트 인증서)로 붙어 본다. 인증서를 확인하지 못하면 `sslmode=require`로 낮춰 붙고 이렇게 알린다: `the server's certificate could not be checked (…), so upstream uses sslmode=require: the connection is encrypted, but the server is not verified. To verify it, add sslrootcert=<the service's CA file> and sslmode=verify-full to upstream in the config file.` 서버가 TLS를 받지 않으면 `the server does not accept TLS; to connect in plaintext anyway, write sslmode=disable in the address yourself`로 멈춘다. 다른 기기로 평문을 쓰는 것은 사용자가 직접 적어야 한다.
   - 붙기 전에 거부하는 주소: 호스트가 없는 것, 공백이 든 것, 질의 파라미터에 `host`·`hostaddr`·`port`가 있는 것, 호스트가 여럿인 것, 그 밖에 Stet 자신의 연결과 프록시가 서로 다른 서버로 읽는 것(10장), 사용자 이름이나 비밀번호에 인코딩하지 않은 `?`·`#`가 있는 것(`the database address has a ? or # in its user or password; percent-encode it (%3F, %23)`). 마지막 것은 에이전트에게 알려 줄 주소에 비밀번호의 일부가 보일 수 있어서다. 에러는 주소를 인용하지 않는다. 비밀번호가 들어 있기 때문이다.
2. **붙어서 확인한다.** `stet proxy`와 같은 방식으로 붙는다. 붙지 못하면 서버의 에러를 보이고, 그 아래에 `if this address goes through a connection pooler, use the direct connection address instead`를 붙이고 멈춘다. pgconn이 읽지 못하는 주소(예: `sslmode=foo`)는 pgconn의 에러(``cannot parse `postgres://app:xxxxx@…`: …``)가 나오는데, 이 에러는 비밀번호를 `xxxxx`로 가린 채 주소를 보인다. pgx는 이 가림을 best effort라고 적는다. 한 번 붙어 보는 데 15초, 붙어서 읽는 데 30초를 넘기면 멈춘다. PostgreSQL 14보다 낮으면 멈춘다. `stet` 스키마가 없고 그 계정이 만들 수도 없으면 데이터베이스 소유자가 실행할 문장(`GRANT CREATE ON DATABASE … TO …;`)을 보이고 멈춘다. 서버가 보낸 글자는 제어 문자를 공백으로 바꿔 보인다.
3. **테이블을 고른다.** 다음을 모두 채우는 테이블이다.
   - 일반 테이블이거나 파티션을 나눈 테이블이다. 파티션 하나하나는 고르지 않는다(부모를 고른다). 시스템 스키마와 `stet` 스키마의 것이 아니다.
   - 스키마 없이 적은 이름이 그 계정의 `search_path`에서 그 테이블로 풀린다.
   - 컬럼 하나짜리 기본 키가 있다.
   - 그 계정이 읽을 수 있다.
   - 테이블 이름과 키 컬럼 이름이 소문자, 숫자, 밑줄만으로 되어 있고 숫자로 시작하지 않는다.

   뺀 테이블은 이유별로 센다(예: `left out 3 tables: no single-column primary key`). 100개를 넘으면 행 수 추정이 큰 100개만 넣고, 뺀 수와 `-tables`로 고르는 법을 알린다. PostgreSQL 14에서 파티션을 나눈 테이블의 행 수 추정은 손으로 `ANALYZE`하기 전까지 -1이라 맨 뒤로 가고, 100개 상한에서 빠질 수 있다. 그때는 `-tables`로 고른다. 하나도 남지 않으면 `no table can be watched`로 멈춘다.
4. **묻는다.** 위의 "묻는 것"이다.
5. **설정 파일을 쓴다.** 권한은 `0600`이다. 비밀번호가 들어 있다. 파일은 다 쓰이거나 아예 없다. 파일이 이미 있으면 `stet.yaml already exists; pass -force to overwrite it`로 멈춘다. 쓰기 전에 `stet proxy`가 그 파일을 받는지 확인한다.
6. **다음 할 일을 알린다.** 띄우는 명령과 에이전트에게 줄 주소다(5장).

쓰는 파일은 이렇다. 다른 기기의 DB에 `verify-full`로 붙은 경우다.

```yaml
# Written by stet init (stet 0.2.0) on 2026-11-22.
# Shadow mode: stet records what it would do and blocks nothing.
# Every key is described in stet.example.yaml and in docs/spec-v0.1.md, chapter 9.
# This file holds the database password: keep it private.

mode: shadow
listen: 127.0.0.1:6432
upstream: postgres://app:secret@db.example.com:5432/crm?sslmode=verify-full
model:
  adapter: none
tables:
  contacts:
    pk: id
    impact:
      max_rows: 100
    reads:
    stale:
  deals:
    pk: id
    impact:
      max_rows: 100
    reads:
    stale:
```

- `impact.max_rows: 100`: 한 문장이 100행보다 많이 건드리면 `hold`로 판정한다(섀도라 `would hold`로 보인다).
- `reads:`: 이 테이블의 읽기를 기록한다. 값 없이 적은 키가 그 기능을 켠다.
- `stale:`: 에이전트가 읽은 행의 버전을 기억해 두었다가, 그 행을 고치거나 지울 때 그사이 바뀌었는지 본다. 키 컬럼의 타입이 그 기능이 추적하는 타입일 때만 적는다.
- `send_fields`가 없다. 모델이 없으면 밖으로 보내는 것이 없으므로 적지 않아도 된다(11장).

## 4. `stet proxy`

`init`이 알려 준 명령으로 띄운다.

```sh
stet proxy -config stet.yaml
```

```
stet: watching 2 tables on 127.0.0.1:6432 (shadow: nothing is blocked)
time=2026-11-22T20:39:39.590+09:00 level=INFO msg="stet proxy listening" listen=127.0.0.1:6432 upstream=db.example.com:5432 upstream_tls=verified mode=shadow agent_roles=[] gated_tables=2 model=none
```

- 처음 띄울 때 그 데이터베이스에 `stet` 스키마가 생긴다. 판단 기록(`stet.decisions`)이 거기에 남는다.
- 판단한 쓰기마다 한 줄을 표준 출력에 낸다(6장). 첫 줄 `stet: watching …`은 받기 시작했다는 표시다. 로그(`time=… level=…`)는 표준 에러로 간다.
- 로그의 `upstream_tls`는 Stet과 DB 사이 구간의 보호다. `off`(`sslmode=disable`), `preferred`(`prefer`나 적지 않음: 서버가 TLS를 받으면 TLS, 인증서는 확인하지 않는다), `unverified`(`sslrootcert` 없는 `require`), `verified`(`verify-ca`, `verify-full`, `sslrootcert`가 있는 `require`)다.
- 줄은 표준 출력이 터미널일 때만 낸다(`-decisions auto`, 기본). 파일이나 파이프로 보내면 내지 않는다. 문장에는 값이 들어 있어서, 로그 수집기로 흘러가는 출력에는 기본으로 내지 않는다. `-decisions on`이면 늘 내고, `off`면 내지 않는다.
- 줄을 받는 쪽이 먼저 끝나도(`stet proxy -config stet.yaml -decisions on | head -5`) Stet은 계속 돈다. 그 뒤의 줄은 버려진다. 표준 에러를 받는 쪽이 끝나면 로그가 같은 식으로 버려진다.
- 끌 때는 Ctrl-C다. 판단 중인 쓰기를 기록하고 끝난다. 아직 쓰지 못한 줄은 2초까지만 기다린다.

시작할 때 다음 경우에는 경고를 남긴다. 막지는 않는다.

- `upstream`의 호스트가 이 기기가 아닌데 평문으로 갈 수 있는 설정(`sslmode=disable`, `prefer`, 적지 않음): `upstream db.example.com is not this machine and its sslmode allows plaintext: … fix: add sslmode=verify-full to upstream (and sslrootcert=<CA file> when the server's CA is not a public one)`
- `upstream`의 호스트가 이 기기가 아닌데 인증서를 확인하지 않는 설정(`sslmode=require`에 `sslrootcert` 없음): `upstream db.example.com is not this machine and its certificate is not checked (sslmode=require without sslrootcert): …`
- `listen`이 루프백 주소가 아닐 때: `listen 0.0.0.0:6432 is not a loopback address: the connection between clients and stet is not encrypted; …`

### Docker로 띄울 때

`init`도 컨테이너에서 돌린다. 지금 디렉터리에 `stet.yaml`을 쓴다.

```sh
docker run --rm -it --user "$(id -u):$(id -g)" -e DATABASE_URL -v "$PWD:/work" -w /work stet:local init -listen 0.0.0.0:6432
```

- `-e DATABASE_URL`은 이 셸의 값을 명령줄에 적지 않고 넘긴다.
- `-listen 0.0.0.0:6432`: 컨테이너 안의 Stet은 컨테이너의 모든 주소에서 받아야 바깥에 연 포트가 닿는다. 에이전트에게 알려 주는 주소는 그래도 `127.0.0.1:6432`로 적는다.
- `--user`: 이미지는 root가 아닌 사용자(65532)로 돈다. 설정 파일은 소유자만 읽을 수 있으므로(`0600`), 컨테이너를 이 기기의 사용자로 돌려야 그 파일을 쓰고 나중에 읽는다.
- DB가 이 기기에 있으면 Docker Desktop에서는 주소의 호스트를 `host.docker.internal`로 쓴다. Linux의 Docker Engine에서는 그 이름이 그냥은 풀리지 않으므로, 두 `docker run`(`init`과 `proxy`)에 `--add-host=host.docker.internal:host-gateway`를 더한다. 이 Linux 경우는 돌려 보지 않았다(확인한 기기는 macOS의 Docker Desktop뿐이다). `init`에게 `host.docker.internal`은 다른 기기다. `verify-full`로 붙어 보고, 인증서가 그 이름의 것이 아니면 `require`로 낮추고 알린다. DB가 TLS를 받지 않으면 주소에 `sslmode=disable`을 직접 적는다.

```sh
docker run --rm --user "$(id -u):$(id -g)" -v "$PWD/stet.yaml:/stet.yaml:ro" -p 127.0.0.1:6432:6432 stet:local proxy -config /stet.yaml -decisions on
```

- `-decisions on`: `-t` 없이 띄우면 표준 출력이 터미널이 아니라서 기본(`auto`)으로는 줄을 내지 않는다.
- `-p 127.0.0.1:6432:6432`: 포트를 이 기기의 루프백에만 연다. 에이전트와 Stet 사이는 평문이다. `-p 6432:6432`로 열면 이 기기의 모든 주소에서 받는다.
- 시작할 때 `listen 0.0.0.0:6432 is not a loopback address` 경고가 나온다. 컨테이너 안의 주소에 대한 경고다. 포트를 `127.0.0.1`에만 열었으면 이 기기 밖에서는 닿지 않는다. 시작 줄은 `stet: watching 2 tables on [::]:6432 (shadow: nothing is blocked)`로 나온다.
- 컨테이너 안의 시각은 UTC다. 이 기기의 시간대로 보려면 `-e TZ=Asia/Seoul`처럼 준다.
- Ctrl-C로 끄면 컨테이너도 지워진다(`--rm`).

## 5. 에이전트의 접속 주소 바꾸기

에이전트가 DB 주소를 읽는 곳(에이전트를 띄우는 환경의 `DATABASE_URL`, MCP 서버 설정의 접속 문자열 등)에서, 주소를 `init`이 알려 준 것으로 바꾼다. `<password>` 자리에는 원래 비밀번호를 넣는다. 계정과 비밀번호는 전과 같다.

```
postgres://app:<password>@127.0.0.1:6432/crm?sslmode=disable
```

- 호스트와 포트는 Stet의 것(`listen`)이다. `listen`의 호스트가 `0.0.0.0`이나 `::`면 `127.0.0.1`로 적는다.
- `sslmode=disable`: 에이전트와 Stet 사이는 같은 기기 안이라 평문이다. Stet과 DB 사이는 설정의 `sslmode`를 따른다.
- 원래 주소의 `ssl`로 시작하는 파라미터, `channel_binding`, `gssencmode`, `requiressl`, `host`, `hostaddr`, `port`는 뺀다. 남은 파라미터는 이름 순서로 적고, `password` 파라미터는 `password=<password>`로 가린다.
- 클라이언트가 TLS(`sslmode=require` 등)나 채널 바인딩(`channel_binding=require`)을 요구하면 Stet에 붙지 못한다. 알려 준 주소를 그대로 쓴다.
- 인증은 중계한다. md5와 SCRAM의 비밀번호를 Stet은 보지 않는다. DB와의 구간이 TLS면, 서버가 제안하는 방식 가운데 채널 바인딩 방식(`SCRAM-SHA-256-PLUS`)을 빼고 넘긴다. 클라이언트는 `SCRAM-SHA-256`으로 로그인한다. 서버가 채널 바인딩 방식만 제안하면 `stet: the server offers only channel-bound authentication, which cannot pass through stet`로 끝난다.

## 6. 줄 읽는 법

```
<시각>  <판정>  <OP> <테이블>  <사실들>  [<이유들>]  <문장>
```

칸 사이는 공백 둘이다. 판정 칸은 12글자 폭으로 채운다. 없는 칸은 빠진다.

**시각.** 쓰기가 Stet에 들어온 시각이다. 이 기기의 시간대다. 줄은 판단이 기록된 순서로 나온다. 섀도에서는 쓰기가 실행된 뒤에 판단하므로, 잇달아 보낸 쓰기의 줄이 들어온 순서와 다르게 나올 수 있다.

**판정.**

| 줄의 낱말 | 뜻 |
|---|---|
| `ok` | 걸리는 것이 없다 |
| `would hold` | enforce였다면 보류하고 사람의 검토를 기다렸을 쓰기다 |
| `would escalate` | enforce였다면 보류했을 쓰기다. 판단이 확실하지 않아 사람에게 올리는 경우다 |
| `would reject` | enforce였다면 거부했을 쓰기다. 예: 읽은 뒤에 바뀐 행에 쓴 것(`stale_read`) |
| `unsupported` | Stet이 판단하지 않는 쓰기다. 예: 명시적 트랜잭션 안의 쓰기(`in_transaction`) |

enforce로 돌면 `would`가 빠진다(`hold`, `escalate`, `reject`). 이 문서의 길은 섀도뿐이다. 기록(`stet.decisions.verdict`)에는 `commit`, `hold`, `escalate`, `reject`, `unsupported`로 남는다. `ok`는 `commit`이다.

**사실들.** 쉼표로 잇는다. 이 순서다.

1. 행 수. 실행된 쓰기는 서버가 답한 행 수다: `300 rows`, `1 row`. 실행되지 않은 쓰기는 Stet이 잰 값이다. 센 값은 그대로(`40 rows`), 추정은 `~`를 붙여(`~300 rows`) 적는다. 한도에서 세기를 멈췄으면 추정이 있을 때 `~300 rows`, 없을 때 `101+ rows`다.
2. `~30% of table`: 테이블 행 수 추정에 대한 비율이다. 추정은 통계(`ANALYZE`)에서 오므로 늘 `~`가 붙는다. 1% 미만은 `<1% of table`. 통계가 없으면 빠진다.
3. `cascades into deals`: 지우는 쓰기가 외래 키를 따라 번지는 테이블이다.
4. `fresh read`, `stale read`: 같은 계정이 그 행을 읽은 뒤로 행이 바뀌지 않았다, 바뀌었다. `stale:`을 켠 테이블에서 키 하나로 고치거나 지우는 쓰기만 본다. 읽은 적이 없으면 빠진다.
5. `not executed`: 서버가 거부했거나 결과를 모르는 쓰기다. enforce에서 Stet이 보내지 않은 쓰기는 `refused ST001`처럼 답한 SQLSTATE가 나온다.

**이유들.** 대괄호 안에 쉼표로 잇는다. 모델 없이 이 길에서 나오는 것은 대개 이렇다.

- `impact:rows`: 행 수가 `impact.max_rows`(100)를 넘었다.
- `impact:uncertain`: 한도 안이지만 행 수를 추정으로만 쟀다.
- `stale_read`: 읽은 뒤에 바뀐 행에 썼다.
- `in_transaction`: 명시적 트랜잭션 안의 쓰기다.

나머지는 [spec-v0.2-impact](spec-v0.2-impact.md), [spec-v0.2-stale](spec-v0.2-stale.md), [spec-v0.1](spec-v0.1.md) 6장에 있다.

**문장.** 줄바꿈, 연속된 공백, 제어 문자를 공백 하나로 바꾸고, 80자를 넘으면 79자와 `…`로 자른다. 아주 긴 문장은 앞 4 KiB만 본다. 문장, 테이블 이름, 이유는 에이전트나 DB가 쓴 글자라서 터미널에 그대로 내지 않는다.

**버린 줄.** 줄은 1,024줄짜리 대기열을 거쳐 쓴다. 터미널이 따라오지 못해 대기열이 차면, 대기열이 빌 때까지 새 줄을 버리고 버린 수를 한 줄로 알린다: `… 37 lines dropped`. 쓰기와 판단은 출력을 기다리지 않는다. 버린 줄의 판단도 `stet.decisions`에는 있다.

## 7. 빈 DB에서 해 보기

에이전트 없이, 빈 데이터베이스와 `psql`로 1장의 줄을 본다.

빈 데이터베이스 하나와 그 소유 계정을 준비하고(로컬 Postgres면 `createdb`, 매니지드 서비스면 그 콘솔에서), 그 주소를 `DATABASE_URL`에 둔다.

```sh
read -rs DATABASE_URL && export DATABASE_URL    # 빈 데이터베이스의 주소
```

테이블 둘을 만든다. `contacts` 1,000행과, 그 행마다 하나씩 딸린 `deals`다. `deals`는 `contacts`를 `ON DELETE CASCADE`로 가리킨다.

```sh
psql "$DATABASE_URL" <<'SQL'
create table contacts (
  id         int primary key,
  name       text not null,
  status     text not null,
  company_id int not null
);
create table deals (
  id         serial primary key,
  contact_id int not null references contacts on delete cascade
);
insert into contacts select g, 'contact ' || g, 'lead', g % 10 from generate_series(1, 1000) g;
insert into deals (contact_id) select g from generate_series(1, 1000) g;
analyze contacts, deals;
SQL
```

`analyze`가 없으면 자동 통계가 돌기 전까지 줄에 `~30% of table`이 빠진다.

새 디렉터리에서 `init`을 돌리고 `y`로 답한 뒤, 같은 터미널에서 Stet을 띄운다.

```sh
mkdir stet-try && cd stet-try
stet init
stet proxy -config stet.yaml
```

다른 터미널에서 에이전트 노릇을 한다. `init`이 알려 준 주소의 `<password>`를 비밀번호로 바꿔 `AGENT`에 둔다. 그리고 읽기 하나와 쓰기 셋을 보낸다. 쓰기마다 첫 터미널에 줄이 나온 것을 보고 다음을 보낸다.

```sh
read -rs AGENT
psql "$AGENT" -c "select id, status from contacts where id = 7"
psql "$AGENT" -c "update contacts set status = 'customer' where id = 7"
psql "$AGENT" -c "update contacts set status = 'churned' where company_id < 3"
psql "$AGENT" -c "delete from contacts where id between 1 and 200"
```

`psql`은 `UPDATE 1`, `UPDATE 300`, `DELETE 200`을 답하고, 첫 터미널에는 1장의 세 줄이 나온다(시각만 다르다).

- 첫 줄의 `fresh read`는 바로 앞의 읽기에서 온다. 같은 계정이 7번 행을 읽었고, 고칠 때 그 행이 그대로였다. 읽기를 빼면 `1 row`만 나온다.
- `company_id < 3`은 1,000행 가운데 300행이다. 한도 100을 넘는다.
- 1번부터 200번까지 지우면 외래 키를 따라 `deals`의 행도 지워진다.

다 봤으면 첫 터미널에서 Ctrl-C로 Stet을 끈다.

## 8. 기록 보기

판단은 DB의 `stet.decisions`에 남는다. DB에 바로 묻는다.

```sh
psql "$DATABASE_URL" -c "select created_at, verdict, reasons, op, target_table, rows_affected, statement from stet.decisions order by created_at"
```

```
          created_at           | verdict |    reasons    |   op   | target_table | rows_affected |                          statement
-------------------------------+---------+---------------+--------+--------------+---------------+-------------------------------------------------------------
 2026-11-22 20:39:42.725081+09 | commit  | {}            | update | contacts     |             1 | update contacts set status = 'customer' where id = 7
 2026-11-22 20:39:45.783141+09 | hold    | {impact:rows} | update | contacts     |           300 | update contacts set status = 'churned' where company_id < 3
 2026-11-22 20:39:48.844723+09 | hold    | {impact:rows} | delete | contacts     |           200 | delete from contacts where id between 1 and 200
(3 rows)
```

- `created_at`은 쓰기가 들어온 시각이다. 줄의 시각과 같다.
- `impact`(jsonb)에 행 수 측정이, `read_check`(jsonb)에 읽기 확인이 있다. 줄의 사실들은 이 둘과 `rows_affected`에서 나온다.
- 컬럼 전체는 [`internal/store/schema.sql`](../internal/store/schema.sql)에 있다.

## 9. 지우는 법

새 명령은 없다. Stet은 아래 문장을 스스로 실행하지 않는다.

1. 에이전트의 접속 주소를 원래 DB 주소로 되돌린다.
2. `stet proxy`를 끈다(Ctrl-C).
3. `stet` 스키마를 지운다. 판단 기록도 함께 지워진다.

   ```sh
   psql "$DATABASE_URL" -c 'DROP SCHEMA stet CASCADE;'
   ```

4. 설정 파일을 지운다. 비밀번호가 들어 있다.

   ```sh
   rm stet.yaml
   ```

   `stet init`이 파일을 쓰는 도중에 끊긴 적이 있으면(Ctrl-C, `kill`) 설정 파일 옆에 이름 앞에 `.`, 뒤에 `.<숫자>`가 붙은 파일이 남아 있을 수 있다(`stet.yaml`이면 `.stet.yaml.123456789`). 쓰던 설정이고 비밀번호가 들어 있다. 권한은 `0600`이다. 있으면 함께 지운다: `rm -f .stet.yaml.*`

되돌리기(`undo`)를 켠 적이 있으면 3 전에 `stet undo-sql -config stet.yaml -drop`이 내는 문장으로 캡처 트리거를 지운다. 이 문서의 길에서는 켜지 않는다.

7장에서 만든 테이블은 `drop table deals, contacts;`로 지우거나, 데이터베이스째 지운다.

## 10. 한계

- **통제가 아니다.** 같은 기기의 에이전트는 진짜 접속 정보(설정 파일, 환경 변수)를 읽어 DB에 직접 붙을 수 있다. 이 길은 실수를 보여 주는 데까지다. 에이전트가 Stet을 거쳐야만 DB에 닿게 하려면 Stet을 서버에 놓고, 에이전트에게는 DB의 비밀번호 대신 Stet이 내준 토큰만 준다. 그 순서는 [deploy.md](deploy.md)에 있다.
- **계정이 하나면 기록을 지킬 수 없다.** 에이전트가 Stet과 같은 계정을 쓰므로 `stet` 스키마를 읽고 고칠 수 있다. 섀도에서만 받아들인다. enforce가 요구하는 것(계정 분리, 역할 부여, `stet` 스키마 권한)은 [spec-v0.2-enforce](spec-v0.2-enforce.md) 8장에 있다.
- **에이전트와 Stet 사이는 평문이다.** 같은 기기 안이라는 전제다. 루프백이 아닌 주소로 받으면 경고한다. 다른 기기의 에이전트를 받을 때는 `listen_tls`로 TLS를 받는다([deploy.md](deploy.md) 3장).
- **채널 바인딩을 요구하는 클라이언트는 붙지 못한다.** `sslmode=require`를 적은 클라이언트도 같다. 접속 주소를 `init`이 알려 준 것으로 바꿔야 한다.
- **`require`와 `prefer`는 서버를 확인하지 않는다.** `prefer`는 중간에서 TLS를 거절당하면 평문으로 간다. libpq와 같은 뜻이다. 원격 DB에는 `verify-full`을 쓴다.
- **`prefer`(`sslmode`를 적지 않은 주소도)의 프록시 구간이 평문으로 가는 것은 서버가 TLS 요청을 거절(`N`)할 때뿐이다.** 서버가 TLS를 받겠다고 하고 인사에서 실패하면, Stet 자신의 연결은 평문으로 다시 붙어 Stet은 뜨지만 에이전트의 세션은 `08001`(`stet: TLS to upstream … failed: …`)로 끝난다. 그런 서버에는 `sslmode=disable`이나 `require`를 적는다.
- **연결 풀러를 거친 주소는 지원하지 않는다.** Stet 자신의 연결은 시작 파라미터와 세션 상태를 쓴다. 풀러에 따라 접속이 거부되거나, 붙더라도 Stet의 읽기와 에이전트의 세션이 어긋날 수 있다. 직접 접속 주소를 쓴다.
- **주소에는 호스트와 포트를 하나만 적는다.** 호스트 여럿(장애 조치 목록), 질의 파라미터나 `PGHOST`·`PGPORT`·서비스 파일로 정한 호스트와 포트는 받지 않는다. Stet 자신의 연결과 에이전트의 세션이 다른 서버에 닿을 수 있기 때문이다.
- **인증서로 하는 에이전트 인증은 지나가지 못한다.** 중계하는 세션에는 클라이언트 인증서를 내지 않는다.
- **스키마를 적은 이름과 `search_path` 밖의 테이블은 고르지 않는다.** 지금 Stet이 이름을 푸는 방식의 한계다.
- **실제 매니지드 서비스에서 확인하지 못했다.** 시험은 로컬의 TLS 서버로 했다. Neon·Supabase·RDS에서의 확인은 계정이 있어야 한다.

## 11. 기록에 남는 값

모델이 없으면 밖으로 보내는 것이 없다. 그래서 `send_fields`(모델이 볼 컬럼 목록)가 없어도 된다. 그때 한 행에 쓰는 쓰기(Stet이 그 행의 상태를 읽은 쓰기)의 판단 기록에서 `state` 안의 `proposed`와 `current`는 빈다(`{}`). 여러 행에 쓰는 쓰기는 상태를 읽지 않으므로 `state` 자체가 없다(NULL).

그래도 쓰려던 값은 기록에 남는다.

- `proposed` 컬럼: 한 행에 쓰는 쓰기가 넣으려던 값 전부(7장의 첫 쓰기면 `{"status": "customer"}`). Stet이 그 행의 상태를 읽은 쓰기에만 있고, 여러 행에 쓰는 쓰기(7장의 둘째, 셋째)는 비어 있다.
- `statement`: 에이전트가 보낸 문장 그대로. `psql -c`처럼 값을 문장에 적어 보내면 값이 여기에 있다.
- `params`: 값을 따로 보내는 클라이언트(확장 질의 프로토콜)의 바인딩 값. 문장에 값을 적어 보내면 비어 있다.

`send_fields`는 모델로 나가는 것을 고르는 목록이고, 사용자 자신의 DB에 남는 기록을 줄이는 목록이 아니다. 기록은 그 데이터베이스의 `stet` 스키마에 있고, 그 계정으로 읽을 수 있다. 계정이 하나면 에이전트도 읽을 수 있다. 트리거가 판단 기록의 수정과 삭제를 막는다(append-only. 라벨 컬럼 밖의 `UPDATE`, `DELETE`, `TRUNCATE`는 `ST004`로 거부된다). 그러나 계정이 하나면 그 계정이 `stet.decisions`의 소유자라서 트리거를 끄고 기록을 고치거나 지울 수 있고, 에이전트도 그 계정이다. 이 길에서 트리거는 실수를 막을 뿐 기록을 지키지 못한다(10장의 "계정이 하나면 기록을 지킬 수 없다"). 기록을 없애려면 9장대로 스키마를 지운다.
