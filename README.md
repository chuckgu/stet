# Stet

> 에이전트 네이티브 데이터베이스 레이어: **클라이언트가 틀릴 수 있다고 가정하는 DB**
>
> *Agents propose. Stet decides what stands.*

상태: v0.1 구현 3주차 (2026-10-31). 섀도 모드가 동작한다. 게이트 대상 쓰기의 상태를 조립하고 `stet.decisions`에 판단을 기록하며, 원래 쓰기는 바꾸지 않는다. 모델 어댑터는 Jev(`jev`)와 기록만 하는 `none`이 있고, 실험용으로 Claude(`claude`)가 있다. Jev 단건 지연은 p95 약 200ms다. 실험은 합성 CRM에서 쓰기 1,461건을 녹화했고, arm A0·A1·B·B'·C로 재생해 채점했다. 실험 전에 고정한 기준으로는 go다(docs/lab.md). enforce 모드는 아직 없다.

## 정의

기존 DB는 클라이언트를 사람이 작성한 결정론적 코드로 가정한다. 그 코드는 스키마의 의미를 이해하고, 검증을 거친 뒤에 쓰며, 오류는 드문 버그라고 본다.

AI 에이전트는 다르다.

- 권한은 있지만 판단은 확률적이다.
- 스키마의 업무적 의미를 모른다.
- 낡은 상태로 행동하고, 재시도하고, 병렬로 같은 데이터를 건드린다.
- 누군가를 대신해서 행동한다.

Stet은 이 전제를 DB 계층에서 받아들인다. 결정론적 제약은 형식 오류만 잡는다. 형식은 맞지만 의미가 틀린 쓰기는 판단이 있어야 잡힌다.

## 구조

Postgres는 그대로 두고 그 앞에 얹는다. Postgres가 이미 하는 것(ACID, RLS, WAL, 복제)은 다시 만들지 않는다.

| 계층 | 하는 일 | v0.1 |
|---|---|---|
| 에이전트 인터페이스 | 의미 카탈로그, 권한 위임, 멱등 쓰기 | 에이전트 식별·기록만 |
| **판단 계층** | 판단형 쓰기 게이트, 판단 기록 | **핵심 범위** |
| 상태 계층 | 버전·시점·출처, 의존성 기반 무효화·재평가 | 행 버전 확인만 |
| 학습 루프 | 임계값 보정, 자체 판단 모델 | 판단 기록 축적만 |
| Postgres | 저장, 트랜잭션, 권한, 복제 | 그대로 사용 |

## 오픈 코어 경계

| 오픈소스 (Apache 2.0) | 매니지드 (유료) |
|---|---|
| pgwire 프록시 | 추론 최적화: 캐싱, 배치, 버전 고정, 판단 단위 과금 |
| 판단형 쓰기 게이트 | 고객별 confidence 보정 |
| 판단 기록 스키마 | 판단–결과 데이터로 학습한 자체 판단 모델 |
| 섀도 모드 | 감사 대시보드, 보류 건 검토 UI, 팀 권한 |
| 모델 어댑터 인터페이스 | 컴플라이언스 리포트 |
| (v0.2) 무효화·재평가 | |

오픈소스 사용자는 자기 모델 API 키를 쓴다. 라이선스는 처음 정한 대로 유지한다.

## 문서

- [docs/spec-v0.1.md](docs/spec-v0.1.md): v0.1 기술 스펙
- [docs/experiment.md](docs/experiment.md): 검증 실험 설계와 go/kill 기준
- [docs/decisions.md](docs/decisions.md): 결정 기록과 열린 질문
- [docs/landscape.md](docs/landscape.md): 주변 지형

## 개발

필요한 것: Go 1.27 이상. 통합 테스트에는 Postgres(`initdb`, `pg_ctl`, `psql`)가 PATH에 있어야 한다.

```bash
go build -o bin/stet ./cmd/stet
bin/stet proxy -config stet.yaml   # 설정 형식은 stet.example.yaml
```

통합 테스트는 일회용 Postgres를 띄워서 돌린다. `STET_TEST_PG`가 없으면 통합 테스트는 건너뛴다.

```bash
scripts/testdb.sh start /tmp/stet-pg   # 포트 55432, 인증 방식별 테스트 계정 생성
export STET_TEST_PG=127.0.0.1:55432
go test -race ./...
scripts/testdb.sh stop /tmp/stet-pg
```

| 패키지 | 역할 |
|---|---|
| `cmd/stet` | CLI (`stet proxy`, `stet bench-jev`) |
| `internal/config` | `stet.yaml` 로드와 검증. 모델 버전 별칭 거부 |
| `internal/proxy` | pgwire 프록시. 세션별로 문장·실행 결과·트랜잭션 상태를 추적하고 `Gate`에 알린다 |
| `internal/sqlparse` | 게이트 대상 쓰기 분류 (pass / gated / unsupported), `stet.on_behalf_of` |
| `internal/gate` | 상태 조립, 판단 호출, 기록 (섀도 모드) |
| `internal/decide` | 모델 중립 `Decider` 인터페이스, 질문 세트, verdict 정책 |
| `internal/decide/jev` | TypeSafe Jev 어댑터 (`TYPESAFE_API_KEY` 필요) |
| `internal/decide/claude` | Claude 어댑터: 같은 질문을 JSON으로 묻는 LLM 게이트, 실험 arm B·B'용 (`ANTHROPIC_API_KEY` 필요, 지금은 `stet-lab`에만 등록) |
| `internal/store` | `stet.decisions` 스키마와 기록 |
| `cmd/stet-lab`, `internal/lab/*` | 실험 도구: 합성 CRM, 업무·주입·라벨, LLM 에이전트, 재생과 채점 (docs/lab.md) |

섀도 모드로 쓰기를 기록해 보려면 `stet.example.yaml`을 복사해 `upstream`, 테이블, `send_fields`를 맞춘다. Jev로 판단하려면 `TYPESAFE_API_KEY`를 환경 변수로 주고, 모델 없이 기록만 하려면 `model: {adapter: none}`으로 둔다. 기록은 `select * from stet.decisions order by created_at desc`로 본다.

Jev 단건 지연은 `TYPESAFE_API_KEY`를 준 상태에서 `bin/stet bench-jev -n 60 -version jev-1.14.0`으로 잰다.
