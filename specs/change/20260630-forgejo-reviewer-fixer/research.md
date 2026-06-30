# Forgejo PR Review 能力实测

## 测试范围

本次调研在真实 Forgejo 仓库上验证 PR reviewer/fixer 相关能力，包括：

- PR 顶层评论的创建、列表、编辑、删除。
- PR label 的添加、列表、删除。
- PR review 的创建、pending review、提交 review。
- inline review comment 的创建和读取。
- review comment reply/thread 相关表现。
- review comment resolve/unresolve 能力。
- 推送新 commit 后 stale/outdated 行为。
- requested reviewers 的添加和取消。
- API 版本、权限、字段形状。

本文件只记录实测结果，不给出实现方案。

## 测试环境

- Forgejo 仓库：`https://code.powerformer.net/core/looper-sandbox`
- 临时 PR：`core/looper-sandbox#12`
- 临时分支：`forgejo-capability-20260630`
- API/CLI 登录：`tea` login `powerformer`
- 操作用户：`nettee`
- 实例版本：`14.0.2+gitea-1.22.0`
- OpenAPI：`https://code.powerformer.net/swagger.v1.json`
- 测试时间：`2026-06-30`

清理状态：

- PR `#12` 已关闭。
- 临时远端分支 `forgejo-capability-20260630` 已删除。
- 临时顶层 PR comment 已删除。
- 临时 label 已移除。
- 临时 requested reviewer 已移除。
- review/comment 历史仍保留在已关闭 PR 中。

## PR 与基础元数据

通过 `POST /repos/core/looper-sandbox/pulls` 创建临时 PR 成功。

创建响应中的关键字段：

- `number: 12`
- `state: "open"`
- `html_url: "https://code.powerformer.net/core/looper-sandbox/pulls/12"`
- `base.ref: "main"`
- `base.sha: "58c76a21af6ad9ee70d9d5c03c60806e4814fee4"`
- 初始 `head.ref: "forgejo-capability-20260630"`
- 初始 `head.sha: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`

仓库权限字段返回：

- `permissions.admin: true`
- `permissions.push: true`
- `permissions.pull: true`

关闭 PR 后再次读取：

- `state: "closed"`
- `closed_at: "2026-06-30T08:33:36Z"`
- `labels: []`
- `requested_reviewers: []`
- `requested_reviewers_teams: []`

删除临时分支后，`git ls-remote --heads origin forgejo-capability-20260630` 无输出。

## 顶层 PR Comment

Forgejo PR 顶层评论通过 issue comment API 操作。

实测端点：

- 创建：`POST /repos/core/looper-sandbox/issues/12/comments`
- 列表：`GET /repos/core/looper-sandbox/issues/12/comments`
- 编辑：`PATCH /repos/core/looper-sandbox/issues/comments/382`
- 删除：`DELETE /repos/core/looper-sandbox/issues/comments/382`

创建 comment `382` 成功，响应包含：

- `id`
- `html_url`
- `pull_request_url`
- `issue_url`
- `user`
- `body`
- `created_at`
- `updated_at`

创建响应中：

- `html_url: "https://code.powerformer.net/core/looper-sandbox/pulls/12#issuecomment-382"`
- `pull_request_url: "https://code.powerformer.net/core/looper-sandbox/pulls/12"`
- `issue_url: ""`
- `body: "looper capability probe: top-level PR comment v1"`

编辑后响应：

- `body: "looper capability probe: top-level PR comment v2 edited"`
- `updated_at: "2026-06-30T08:29:20Z"`

删除返回：

- HTTP `204 No Content`

删除后再次列表：

- `[]`

## Labels

仓库已有 label：

- `looper-e2e`
- `looper:review`
- `looper:worker-ready`

实测端点：

- 仓库 labels：`GET /repos/core/looper-sandbox/labels`
- PR labels：`GET /repos/core/looper-sandbox/issues/12/labels`
- 添加 label：`POST /repos/core/looper-sandbox/issues/12/labels`
- 删除 label：`DELETE /repos/core/looper-sandbox/issues/12/labels/looper:review`

添加 `looper:review` 时 payload：

```json
{"labels":[3]}
```

添加成功响应：

```json
[{"id":3,"name":"looper:review","exclusive":false,"is_archived":false,"color":"bfdadc","description":"","url":"https://code.powerformer.net/api/v1/repos/core/looper-sandbox/labels/3"}]
```

删除 label 返回：

- HTTP `204 No Content`

最终 PR 元数据中：

- `labels: []`

## Review 创建与提交

OpenAPI 中相关端点：

- `GET /repos/{owner}/{repo}/pulls/{index}/reviews`
- `POST /repos/{owner}/{repo}/pulls/{index}/reviews`
- `GET /repos/{owner}/{repo}/pulls/{index}/reviews/{id}`
- `POST /repos/{owner}/{repo}/pulls/{index}/reviews/{id}`
- `DELETE /repos/{owner}/{repo}/pulls/{index}/reviews/{id}`

OpenAPI 中 `CreatePullReviewOptions` 字段：

- `body`
- `comments`
- `commit_id`
- `event`

OpenAPI 中 `CreatePullReviewComment` 字段：

- `body`
- `new_position`
- `old_position`
- `path`

OpenAPI 中 `SubmitPullReviewOptions` 字段：

- `body`
- `event`

### COMMENT Review

使用 `event: "COMMENT"` 创建 review 成功。

请求要点：

```json
{
  "body": "looper capability probe: review with inline comment",
  "commit_id": "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a",
  "event": "COMMENT",
  "comments": [
    {
      "path": "README.md",
      "new_position": 3,
      "old_position": 0,
      "body": "looper capability probe: inline review comment on added line"
    }
  ]
}
```

响应关键字段：

- `id: 1`
- `state: "COMMENT"`
- `body: "looper capability probe: review with inline comment"`
- `commit_id: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`
- `stale: false`
- `official: false`
- `dismissed: false`
- `comments_count: 1`
- `submitted_at: "2026-06-30T08:30:50Z"`
- `html_url: "https://code.powerformer.net/core/looper-sandbox/pulls/12#issuecomment-385"`

### REQUEST_CHANGES Review

同一用户对自己创建的 PR 提交 `REQUEST_CHANGES` 被拒绝。

错误响应：

```json
{"message":"reject your own pull is not allowed","url":"https://code.powerformer.net/api/swagger"}
```

本次没有使用非 PR 作者账号继续验证 `REQUEST_CHANGES` 成功路径。

### Pending Review

不传 `event` 创建 pending review 成功。

请求要点：

```json
{
  "body": "looper capability probe: pending review",
  "commit_id": "b897285adc24fc1b482ffe1212d60cb7f8e97d1a",
  "comments": [
    {
      "path": "README.md",
      "new_position": 3,
      "old_position": 0,
      "body": "looper capability probe: pending inline comment on current head"
    }
  ]
}
```

响应关键字段：

- `id: 2`
- `state: "PENDING"`
- `commit_id: "b897285adc24fc1b482ffe1212d60cb7f8e97d1a"`
- `stale: false`
- `comments_count: 1`

提交 pending review 成功：

- 端点：`POST /repos/core/looper-sandbox/pulls/12/reviews/2`
- payload：`{"event":"COMMENT","body":"looper capability probe: submit pending review as comment"}`

提交后响应：

- `id: 2`
- `state: "COMMENT"`
- `body: "looper capability probe: submit pending review as comment"`
- `updated_at: "2026-06-30T08:33:08Z"`

## Inline Review Comments

按 review id 列出 comments 成功：

- `GET /repos/core/looper-sandbox/pulls/12/reviews/1/comments`
- `GET /repos/core/looper-sandbox/pulls/12/reviews/2/comments`

`review 1` 的 inline comment `384` 响应字段：

- `id: 384`
- `body: "looper capability probe: inline review comment on added line"`
- `resolver: null`
- `pull_request_review_id: 1`
- `path: "README.md"`
- `commit_id: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`
- `original_commit_id: ""`
- `diff_hunk`
- `position: 3`
- `original_position: 0`
- `html_url: "https://code.powerformer.net/core/looper-sandbox/pulls/12#issuecomment-384"`

`review 2` 的 inline comment `415` 响应字段：

- `id: 415`
- `body: "looper capability probe: pending inline comment on current head"`
- `resolver: null`
- `pull_request_review_id: 2`
- `path: "README.md"`
- `commit_id: "b897285adc24fc1b482ffe1212d60cb7f8e97d1a"`
- `position: 3`
- `original_position: 0`

`tea pulls review-comments --login powerformer --repo core/looper-sandbox 12 --output json` 能聚合列出 review comments。示例字段：

- `id`
- `path`
- `line`
- `body`
- `reviewer`
- `resolver`

直接请求 `GET /repos/core/looper-sandbox/pulls/12/comments` 返回 404：

```json
{"message":"The target couldn't be found.","url":"https://code.powerformer.net/api/swagger","errors":[]}
```

本实例 OpenAPI 没有列出 `GET /repos/{owner}/{repo}/pulls/{index}/comments` 聚合端点。

## Reply / Thread 表现

OpenAPI 中存在：

- `POST /repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments`

对已存在的 review `1` 再次调用该端点创建第二条 inline comment 成功，comment id 为 `400`。

请求要点：

```json
{
  "path": "README.md",
  "new_position": 3,
  "old_position": 0,
  "body": "looper capability probe: second review comment via review comments POST"
}
```

响应关键字段：

- `id: 400`
- `pull_request_review_id: 1`
- `path: "README.md"`
- `commit_id: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`
- `position: 3`
- `resolver: null`

该响应没有出现以下字段：

- `parent`
- `parent_id`
- `in_reply_to_id`
- `thread_id`

本次实测只能确认“同一 review 下追加 inline comment”可用；没有 API 证据表明 Forgejo 在该版本暴露 GitHub 式 parent/child reply thread 结构。

## Resolve / Unresolve

OpenAPI 中没有列出以下路径：

- `/repos/{owner}/{repo}/pulls/comments/{id}/resolve`
- `/repos/{owner}/{repo}/pulls/comments/{id}/unresolve`

`tea` CLI 中存在相关命令：

- `tea pulls resolve`
- `tea pulls unresolve`

但实测目标实例不可用。

实测 `GET`：

- `GET /repos/core/looper-sandbox/pulls/comments/384/resolve` 返回 HTTP `404`
- `GET /repos/core/looper-sandbox/pulls/comments/384/unresolve` 返回 HTTP `404`

404 body：

```json
{"message":"The target couldn't be found.","url":"https://code.powerformer.net/api/swagger","errors":[]}
```

实测 `POST`：

- `POST /repos/core/looper-sandbox/pulls/comments/384/resolve` 返回 HTTP `405 Method Not Allowed`
- `POST /repos/core/looper-sandbox/pulls/comments/384/unresolve` 返回 HTTP `405 Method Not Allowed`

405 响应头包含：

- `Allow: GET`
- `Content-Length: 0`

`tea pulls resolve --login powerformer --repo core/looper-sandbox 384 --output json` 失败：

```text
Error: unknown API error: 405
Request: '/api/v1/repos/core/looper-sandbox/pulls/comments/384/resolve' with 'POST' method and '' body
```

resolve/unresolve 尝试后再次读取 comment `384`：

- `resolver: null`

本次没有观察到任何 comment 被标记为 resolved。

## Stale / Outdated 行为

创建 review `1` 后，向同一个 PR 分支推送第二个 commit：

- 新 head sha：`b897285adc24fc1b482ffe1212d60cb7f8e97d1a`
- 修改了原 inline comment 所在的新增行。

推送前，review `1`：

- `commit_id: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`
- `stale: false`
- `comments_count: 1`

推送后，review `1`：

- `commit_id: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`
- `stale: true`
- `comments_count: 2`

推送后，旧 comments `384` 和 `400`：

- 仍保留旧 `commit_id: "5de5fb2bcc3e8a4e2393d7c2492f1b21d899696a"`
- 仍保留旧 `diff_hunk`
- 仍保留 `position: 3`
- `resolver: null`
- 没有单独的 `stale` 字段
- `updated_at` 被刷新为 `"2026-06-30T08:32:17Z"`

推送后，当前 head 上创建的 review `2`：

- `commit_id: "b897285adc24fc1b482ffe1212d60cb7f8e97d1a"`
- `stale: false`

当前 head 上的 comment `415`：

- `commit_id: "b897285adc24fc1b482ffe1212d60cb7f8e97d1a"`
- `diff_hunk` 包含更新后的新增行
- 没有单独的 `stale` 字段

当前 PR diff：

```diff
diff --git a/README.md b/README.md
index 951e2fe..9086052 100644
--- a/README.md
+++ b/README.md
@@ -1,2 +1,3 @@
 # looper-sandbox
 
+Forgejo capability validation line updated after inline comment.
```

## Requested Reviewers

OpenAPI 中相关端点：

- `GET /repos/{owner}/{repo}/reviewers`
- `POST /repos/{owner}/{repo}/pulls/{index}/requested_reviewers`
- `DELETE /repos/{owner}/{repo}/pulls/{index}/requested_reviewers`

`GET /repos/core/looper-sandbox/reviewers` 成功，返回可请求 review 的用户列表，包括：

- `Amy`
- `eli`
- `hanyuanxi`
- `Joey`
- `lefarcen`
- `masonjin`
- `mrcfps`
- `nettee`
- `RuiXi`
- `scarlettt_moon`
- `Tuola`
- `xiaoche`
- `zhangchi`

请求 PR 作者自己作为 reviewer 失败：

```json
{"message":"poster of pr can't be reviewer [user_id: 5, repo_id: 12]","url":"https://code.powerformer.net/api/swagger"}
```

请求 `Amy` 作为 reviewer 成功：

- 端点：`POST /repos/core/looper-sandbox/pulls/12/requested_reviewers`
- payload：`{"reviewers":["Amy"]}`
- HTTP：`201 Created`

响应中创建了 review request 记录：

- `id: 3`
- `state: "REQUEST_REVIEW"`
- `official: true`
- `user.login: "Amy"`
- `pull_request_url: "https://code.powerformer.net/core/looper-sandbox/pulls/12"`

取消 `Amy` review request 成功：

- 端点：`DELETE /repos/core/looper-sandbox/pulls/12/requested_reviewers`
- payload：`{"reviewers":["Amy"]}`
- HTTP：`204 No Content`

最终 PR 元数据：

- `requested_reviewers: []`
- `requested_reviewers_teams: []`

## API 稳定性观察

目标实例 OpenAPI 明确列出 review 和 review comment 相关端点：

- `/repos/{owner}/{repo}/pulls/{index}/reviews`
- `/repos/{owner}/{repo}/pulls/{index}/reviews/{id}`
- `/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments`
- `/repos/{owner}/{repo}/pulls/{index}/reviews/{id}/comments/{comment}`
- `/repos/{owner}/{repo}/pulls/{index}/requested_reviewers`
- `/repos/{owner}/{repo}/reviewers`

目标实例 OpenAPI 未列出：

- `/repos/{owner}/{repo}/pulls/{index}/comments`
- `/repos/{owner}/{repo}/pulls/comments/{id}/resolve`
- `/repos/{owner}/{repo}/pulls/comments/{id}/unresolve`

`tea` CLI 内置了 resolve/unresolve 命令，但在目标实例上调用失败，状态码为 `405`。

## 能力汇总

| 能力 | 实测结果 | 证据 |
|---|---:|---|
| 创建 PR | 可用 | `POST /repos/core/looper-sandbox/pulls` 创建 `#12` |
| 关闭 PR | 可用 | `PATCH /repos/core/looper-sandbox/pulls/12` with `state=closed` |
| 顶层 PR comment 创建 | 可用 | `POST /issues/12/comments` 创建 `382` |
| 顶层 PR comment 列表 | 可用 | `GET /issues/12/comments` |
| 顶层 PR comment 编辑 | 可用 | `PATCH /issues/comments/382` |
| 顶层 PR comment 删除 | 可用 | `DELETE /issues/comments/382` 返回 `204` |
| PR label 添加 | 可用 | `POST /issues/12/labels` |
| PR label 删除 | 可用 | `DELETE /issues/12/labels/looper:review` 返回 `204` |
| review 列表 | 可用 | `GET /pulls/12/reviews` |
| COMMENT review 创建 | 可用 | `POST /pulls/12/reviews` 返回 `state:"COMMENT"` |
| REQUEST_CHANGES review | 未完整验证 | 自己 PR 返回 `reject your own pull is not allowed` |
| pending review 创建 | 可用 | 不传 `event` 返回 `state:"PENDING"` |
| pending review 提交 | 可用 | `POST /pulls/12/reviews/2` 返回 `state:"COMMENT"` |
| inline review comment 创建 | 可用 | `path/new_position/old_position/body` |
| 按 review id 列出 comments | 可用 | `GET /pulls/12/reviews/{id}/comments` |
| 单条 review comment 读取 | 可用 | `GET /pulls/12/reviews/1/comments/384` |
| 聚合列出 PR review comments REST | 不可用 | `GET /pulls/12/comments` 返回 `404` |
| 聚合列出 PR review comments tea | 可用 | `tea pulls review-comments ... 12` |
| reply/thread parent-child 字段 | 未观察到 | comment 响应无 `parent` / `in_reply_to_id` / `thread_id` |
| 同 review 下追加 inline comment | 可用 | `POST /pulls/12/reviews/1/comments` 创建 `400` |
| resolve review comment | 不可用 | `POST /pulls/comments/384/resolve` 返回 `405` |
| unresolve review comment | 不可用 | `POST /pulls/comments/384/unresolve` 返回 `405` |
| resolver 字段 | 存在但未变化 | comment 响应含 `resolver:null` |
| review stale 字段 | 可用 | 推新 commit 后 review `1` 变为 `stale:true` |
| comment stale 字段 | 不存在 | comment 响应无单独 `stale` |
| comment commit 归属 | 可用 | comment 响应含 `commit_id` 和 `diff_hunk` |
| requested reviewers 列表 | 可用 | `GET /repos/core/looper-sandbox/reviewers` |
| requested reviewer 添加 | 可用 | 请求 `Amy` 返回 `201` |
| requested reviewer 删除 | 可用 | 删除 `Amy` 返回 `204` |
| 请求 PR 作者自己为 reviewer | 不允许 | 返回 `422` |

