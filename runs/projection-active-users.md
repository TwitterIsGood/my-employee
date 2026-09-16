# 当前状态

- 阶段：本地开发
- 上一个已确认的完成点：无

## 卡点

- **活跃度数据源定不下来，后续实现无法开始**
  - on: audit_log 加索引需要停机；不加索引则查询超时
  - 证据: 同上 EXPLAIN ANALYZE

## 需要你决定

- **活跃度数据源要你定一个方向**
  1. A 只统计登录时间（users.last_login_at 已有索引，今天就能出）
  2. B 统计登录+业务操作（需要 audit_log 加索引，要一次停机窗口，约 20 分钟）
  - 证据: A 的可行性：users 表 last_login_at 有 btree 索引，实测 0.8ms；B 的代价：audit_log 4.2 亿行，建索引预估 20 分钟停机

## 已确认的失败

- **从 audit_log 推导活跃度不可行：单用户 7 天窗口全表扫描超时**
  - where: audit_log 无 (user_id, created_at) 复合索引，查询退化为顺序扫描
  - cause: 索引缺失，不是查询写法问题。加索引可解，但 audit_log 已 4.2 亿行，加索引需要停机窗口
  - 证据: EXPLAIN ANALYZE 输出见 runs/2026-09-16_audit-explain.txt；42.7s，Seq Scan on audit_log

## 观测到的数字

- **users 表没有 department 字段**
  - metric: information_schema.columns 匹配数
  - window: 2026-09-16T05:07Z 单次查询
  - value: 0
  - 证据: psql -c "select column_name from information_schema.columns where table_name='users' and column_name='department'" -> 0 rows
