## 说明

此项目用来初始化qdrant中的集合数据，从ivanka_content数据库中的article_entries表中读取文章内容，转化为纯文本之后，chunk并存入qdrant数据库中。

## TODO

#### 检索阶段的权限过滤（Access Controls）
- 为了防止敏感数据泄露（Data Leaks），护栏在检索时刻就开始介入 。
- 基于元数据的过滤：在文档入库（Ingest）时，为每个数据分片（Chunk）添加权限元数据（如角色、部门 ID） 。
查询时过滤：在执行向量搜索时，系统强制加入过滤条件，确保检索到的内容仅限于当前用户有权访问的范围 。