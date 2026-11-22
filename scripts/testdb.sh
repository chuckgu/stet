#!/usr/bin/env bash
# 통합 테스트용 일회용 Postgres. TCP(127.0.0.1)로만 받고, 인증 방식별 테스트 계정을 만든다.
#
#   scripts/testdb.sh start <data-dir> [port]   # 기본 포트 55432
#   scripts/testdb.sh stop  <data-dir>
#
# TLS: start는 <data-dir>/server.crt와 server.key(자기 서명, CN localhost, SAN localhost와 127.0.0.1,
# 3650일, 키 0600)가 없으면 만들고 ssl=on으로 띄운다. pg_hba와 계정은 그대로다.
# (libpq 14는 IP SAN을 보지 않아 verify-full host=127.0.0.1이 DNS:127.0.0.1 항목을 요구한다.)
# start 후 출력되는 STET_TEST_PG를 export하면 go test가 통합 테스트를 돌린다.
# 계정과 비밀번호는 테스트 전용 값이며 internal/proxy/proxy_test.go와 맞춰야 한다.
set -euo pipefail

cmd=${1:?usage: testdb.sh start|stop <data-dir> [port]}
dir=${2:?usage: testdb.sh start|stop <data-dir> [port]}
port=${3:-55432}

case "$cmd" in
start)
  if [ ! -f "$dir/PG_VERSION" ]; then
    pwfile=$(mktemp)
    echo 'postgres-pw' >"$pwfile"
    initdb -D "$dir" -U postgres --auth=scram-sha-256 --pwfile="$pwfile" >/dev/null
    rm -f "$pwfile"
    cat >"$dir/pg_hba.conf" <<'EOF'
host all stet_trust 127.0.0.1/32 trust
host all stet_md5   127.0.0.1/32 md5
host all stet_clear 127.0.0.1/32 password
host all all        127.0.0.1/32 scram-sha-256
EOF
    fresh=1
  fi
  if [ ! -f "$dir/server.crt" ] || [ ! -f "$dir/server.key" ]; then
    sslconf=$(mktemp)
    cat >"$sslconf" <<'EOF'
[req]
distinguished_name = dn
prompt = no
x509_extensions = ext
[dn]
CN = localhost
[ext]
subjectAltName = DNS:localhost,DNS:127.0.0.1,IP:127.0.0.1
EOF
    rm -f "$dir/server.crt" "$dir/server.key"
    (umask 077 && openssl req -x509 -newkey rsa:2048 -nodes -days 3650 -config "$sslconf" \
      -keyout "$dir/server.key" -out "$dir/server.crt" 2>/dev/null)
    rm -f "$sslconf"
    chmod 600 "$dir/server.key"
  fi
  pg_ctl -D "$dir" -l "$dir/server.log" -w \
    -o "-c listen_addresses=127.0.0.1 -c port=$port -c ssl=on -c unix_socket_directories=''" start >/dev/null
  if [ "${fresh:-}" = 1 ]; then
    PGPASSWORD=postgres-pw psql -q -h 127.0.0.1 -p "$port" -U postgres -d postgres -v ON_ERROR_STOP=1 <<'EOF'
CREATE ROLE stet_scram LOGIN PASSWORD 'scram-pw';
SET password_encryption = 'md5';
CREATE ROLE stet_md5 LOGIN PASSWORD 'md5-pw';
RESET password_encryption;
CREATE ROLE stet_clear LOGIN PASSWORD 'clear-pw';
CREATE ROLE stet_trust LOGIN;
-- Role the experiment's agents connect as (docs/lab.md): granted CRM tables only by stet-lab prepare.
CREATE ROLE stet_agent LOGIN PASSWORD 'agent-pw';
CREATE DATABASE stet_test OWNER stet_scram;
GRANT ALL ON DATABASE stet_test TO stet_md5, stet_clear, stet_trust;
EOF
  fi
  echo "export STET_TEST_PG=127.0.0.1:$port"
  ;;
stop)
  pg_ctl -D "$dir" -m fast stop >/dev/null
  ;;
*)
  echo "unknown command: $cmd" >&2
  exit 2
  ;;
esac
