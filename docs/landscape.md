# 주변 지형 (2026-10-28 기준)

"에이전트 네이티브 DB"를 각자 자기 강점에 맞춰 정의하고 있다. 에이전트가 틀린다는 사실을 DB의 전제로 삼은 곳은 아직 보이지 않는다.

| 플레이어 | 정의하는 "에이전트 네이티브" | 기반 | Stet과의 관계 |
|---|---|---|---|
| Neon / Databricks Lakebase | 생성: 즉시 생성, 브랜칭, scale-to-zero | Postgres | 위에 얹을 수 있는 기반. 기능으로 흡수할 수 있는 가장 큰 위험 |
| Supabase | 생성, 개발자 경험 | Postgres | 위와 같음 |
| CedarDB | 신선도: OLTP와 분석을 한 엔진에서. 에이전트 메모리 협업 | 자체 엔진(TUM Umbra 계열), PG 와이어 호환, PG 확장 불가 | 프록시 방식이면 위에 얹을 수 있음. "비즈니스 규칙은 DB가 강제한다"는 논지가 겹치지만 결정론 쪽 |
| Keewano | 맥락: 엔티티별 이벤트 이력 | 자체 이벤트 DB, 기존 DB 옆에 배치 | 상태 계층과 일부 겹침 |
| MotherDuck `prompt_jev` | 쿼리 안 판단 (분류·필터) | DuckDB | 읽기 측, 사용자가 부르는 방식(pull) |
| pg-jev (오픈소스) | 쿼리 안 판단 (필터·랭킹·분류) | Postgres 확장 | 읽기 측 같은 개념. 쓰기 게이트·판단 기록 없음 |
| Builder.io Agent-Native | 사람과 에이전트가 같은 DB·액션·권한 공유 | 앱 프레임워크(Postgres 사용) | 상위 계층, 잠재 통합처 |
| Mem0, Zep, Letta | 에이전트 메모리 | 각자 저장소 | 메모리 무효화 판단에서 겹침 |

## 비어 있는 자리

- 쓰기 경로의 판단 게이트
- 판단 객체의 생명주기: 근거 버전, 의존성, 무효화, 재평가, 결과

## 근거가 되는 데이터 포인트

- Databricks: 새 DB의 80%, 테스트·개발 환경의 97%를 에이전트가 생성. Neon에서 에이전트 생성 비중은 2년 만에 0.1%에서 80%로 증가. 기업들의 에이전트 대규모 배포는 아직 적고, 성공은 거버넌스 프로세스와 강하게 상관된다 (State of AI Agents, 2026-01)
- Databricks, Neon을 약 10억 달러에 인수 (2025-05)
- Keewano, 에이전트용 이벤트 DB로 1,200만 달러 투자 유치 (2026-09)
- MotherDuck `prompt_jev`: 10만 행 분류 40초·$0.50, 같은 작업 LLM은 32분·$37
- Jev 한계 연구: 낡은 상태 요약을 따르고, 입력에서 물려받은 오류는 confidence로 잡지 못함 (2026-10 프리프린트)

## 출처

- https://siliconangle.com/2026/09/15/startup-keewano-launches-agent-focused-database-and-pulls-in-12m-in-funding/
- https://www.databricks.com/blog/databricks-neon
- https://www.forbes.com/sites/victordey/2026/02/04/databricks-says-ai-agents-now-build-80-of-enterprise-databases/
- https://cedardb.com/ai/
- https://cedardb.com/docs/compatibility/
- https://motherduck.com/docs/integrations/data-science-ai/typesafe/
- https://pgxn.org/dist/jev/
- https://www.opentrain.ai/papers/what-can-t-jev-decide-probing-the-limits-of-one-pass-decision-models--arxiv-2610.24929/
