# Lightweight Local Memory Retrieval Plan

## 背景 / Background

Crush 的本地 memory backend 已经具备 EventStore、Extractor、Consolidator、Materializer、Mental Models、Rollout Summaries、Compaction Recall 和 FTS5 检索。下一步的核心不是重做架构，而是在不引入外部依赖、不强制下载 GB 级模型、不让 CPU 持续满载的前提下，提高中英文混合场景的召回质量和物化/合并质量。

The goal is a better bilingual (Chinese + English) local recall path that remains cheap by default. Embeddings are explicitly optional and not part of the default path.

## 设计原则 / Principles

1. **Zero-download by default.** 默认不下载 embedding 模型，不增加外部服务依赖。
2. **Cheap first-stage recall.** 先用 SQLite FTS5 / BM25 / scope / kind / tags 获取候选。
3. **Bilingual lexical intelligence.** 在查询侧做轻量中英文同义词扩展、CJK token / bigram 处理，而不是全库昂贵计算。
4. **Rerank only candidates.** 重排只处理几十个候选，不扫描全库。
5. **Fail-soft.** FTS 严格查询 miss 时自动回退到宽松 OR 查询，再回退到内存 keyword ranking。
6. **Embeddings are opt-in.** 未来 embedding 只作为可选 reranker，并且必须增量、缓存、后台低优先级。

## P0: 无模型检索增强 / No-model retrieval improvements

### 1. Query expansion

在 query 进入 FTS5 和 heuristic reranker 前做规范化扩展：

- 中文 / English 双语同义词：
  - `记忆`, `memory`, `recall`, `remember`, `memories`
  - `压缩`, `compaction`, `compact`, `summary`, `summarize`
  - `物化`, `materialize`, `materialization`, `artifact`
  - `合并`, `consolidate`, `consolidation`, `merge`
  - `提取`, `extract`, `extraction`
  - `偏好`, `preference`, `preferences`
  - `决策`, `decision`, `decisions`
  - `陷阱`, `pitfall`, `pitfalls`, `gotcha`
  - `流程`, `procedure`, `workflow`
- camelCase / snake_case / kebab-case 拆分。
- 路径、函数名、错误码保留原 token。

### 2. CJK-aware tokenizer

当前 tokenization 主要依赖 `unicode.IsLetter/IsNumber` 分词，对没有空格的中文短语帮助有限。改进：

- 对连续 CJK 文本生成 unigram + bigram。
- 保留 Latin/digit token。
- 对 mixed query，例如 `记忆 recall 准确率`，同时保留中文 bigram 和 English token。

### 3. FTS strict-then-loose search

FTS5 查询采用两段策略：

1. Strict: token 使用 `AND` 组合，精确优先。
2. Loose: strict 无结果时，用扩展 token 的 `OR` 组合。

这样可以避免自然语言 query 中某个词 miss 导致整体无结果。

### 4. Better heuristic reranking

Heuristic reranker 增强：

- exact query phrase 命中高权重；
- expanded token 命中加权；
- kind priority: decision / preference / pitfall / procedure；
- scope priority: project / user / global；
- importance + confidence；
- recency 轻微加权，不压过稳定知识。

## P1: 物化和合并质量 / Materialization and consolidation quality

### Mental Models quality

- 对 `user_preferences`, `project_conventions`, `decisions`, `pitfalls`, `procedures` 分层保持稳定顺序。
- 每个 model 文件限制 budget，但优先保留高 importance / high confidence / latest non-superseded events。
- 在 materialized markdown 中保留来源 metadata：kind、scope、source session、updated time。

### Consolidation quality

- Consolidator 输出必须更严格地区分：
  - short-lived task state;
  - durable decision;
  - user/project preference;
  - reusable procedure;
  - pitfall / gotcha.
- Supersedes 只用于明确替代关系，避免过度删除历史决策。
- 对 bilingual content 保留原文关键词，不强制翻译掉关键 token。

## P2: Optional lightweight embeddings

Embedding 不作为默认实现。当前实现先提供 `hashing` backend：零下载、零模型文件、确定性 signed feature hashing，特征来自 expanded lexical tokens + CJK char n-grams。它不是神经 embedding，但能在中英混合候选集上提供更稳的向量式相似度信号，而且 CPU 成本只和 top-N candidates 成正比。

Implemented local backend:

- `hashing`: default local embedding backend, 384 dimensions, no model download.
- Scope: only wired for `memory.backend=local`.
- Hindsight: not wired, because Hindsight already owns remote recall/vector behavior.
- Query path: FTS/BM25 candidate recall first, then embedding rerank over candidates only.

Future neural backend requirements:

- 默认关闭；
- 模型文件几十 MB 级别，不允许 GB 级默认模型；
- 只对 consolidated events / mental model chunks 建索引；
- content hash 缓存，内容不变不重算；
- 单 worker、低优先级、限 batch、可取消；
- 查询时只对 top-N lexical candidates 做 vector rerank。

Recommended future models:

| Model | Size target | Notes |
|---|---:|---|
| all-MiniLM-L6-v2 int8 ONNX | ~20-35MB | Good English/code baseline |
| bge-small-zh/en int8 ONNX | ~30-100MB | Better bilingual support |
| bge-m3 | 500MB+ | Not default |

## 配置建议 / Configuration

Default:

```json
{
  "options": {
    "memory": {
      "reranker": {
        "enabled": true,
        "type": "heuristic",
        "max_candidates": 60
      }
    }
  }
}
```

Opt-in local hashing embedding:

```json
{
  "options": {
    "memory": {
      "backend": "local",
      "embeddings": {
        "enabled": true,
        "backend": "hashing",
        "dimensions": 384
      },
      "reranker": {
        "max_candidates": 60
      }
    }
  }
}
```

Future opt-in neural embedding:

```json
{
  "options": {
    "memory": {
      "backend": "local",
      "embeddings": {
        "enabled": true,
        "backend": "bge-small-zh-int8",
        "max_batch": 20,
        "idle_delay_seconds": 60
      }
    }
  }
}
```

## 验证指标 / Metrics

- Query: `为什么选择 sqlite 而不是 bbolt` should retrieve decision memories containing `SQLite` even if the exact Chinese words are absent.
- Query: `compaction 压缩前召回` should retrieve compaction recall memories across mixed Chinese/English wording.
- Query: `用户偏好 concise summary` should retrieve preference memories across bilingual tokens.
- No embedding model is downloaded or loaded by default.
- Retrieval latency remains dominated by SQLite query + sorting over <=100 candidates.
