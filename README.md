## 说明

此项目用来初始化qdrant中的集合数据，从ivanka_content数据库中的article_entries表中读取文章内容，转化为纯文本之后，chunk并存入qdrant数据库中。

### 分支 `pg-to-vector-stores`

从 **PostgreSQL** 数据库 `vectors_db` 的 **ivanka_content** 表读取历史数据，经相同处理（HTML→Markdown、chunk、向量化）后写入同库的 **vector_stores** 向量表。

- **数据源**：`vectors_db.ivanka_content`（字段需含：id, title, content_short, content, created_at）
- **目标表**：`vectors_db.vector_stores`（含 embedding vector(1536) 及元数据，由程序自动建表/迁移）
- **环境变量**：`OPENROUTER_API_KEY`、`OPENROUTER_API_BASE_URL`；可选 `PG_DSN`（默认 `host=localhost user=postgres password=postgres dbname=vectors_db port=5432 sslmode=disable`）
- **断点续跑**：通过 `offset.txt` 记录已处理的最大 `id`，与 main 分支逻辑一致。

## TODO

#### 检索阶段的权限过滤（Access Controls）
- 为了防止敏感数据泄露（Data Leaks），护栏在检索时刻就开始介入 。
- 基于元数据的过滤：在文档入库（Ingest）时，为每个数据分片（Chunk）添加权限元数据（如角色、部门 ID） 。
查询时过滤：在执行向量搜索时，系统强制加入过滤条件，确保检索到的内容仅限于当前用户有权访问的范围 。