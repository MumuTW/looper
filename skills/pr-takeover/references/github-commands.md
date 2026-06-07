# GitHub commands for PR takeover

Exact `gh` and GraphQL recipes used by the takeover loop. All assume `gh` is
authenticated with push/merge access. Replace `<owner>`, `<repo>`, `<num>`.

## Snapshot PR state

```bash
gh pr view <num> \
  --json number,state,isDraft,mergeable,mergeStateStatus,reviewDecision,headRefOid,statusCheckRollup \
  --jq '{state,isDraft,mergeable,mergeStateStatus,reviewDecision,head:.headRefOid}'
```

- `reviewDecision`: `APPROVED` | `CHANGES_REQUESTED` | `REVIEW_REQUIRED` | `null`.
- `mergeable`: `MERGEABLE` | `CONFLICTING` | `UNKNOWN`.
- `mergeStateStatus`: `CLEAN`, `BLOCKED`, `BEHIND`, `DIRTY`, `UNSTABLE`, … (`BLOCKED` usually = checks/approvals still required).

## Check status of required checks

```bash
gh pr checks <num>                      # human table; exit code non-zero if any failing
gh pr view <num> --json statusCheckRollup \
  --jq '[.statusCheckRollup[] | {name: (.name // .context), status: (.status // .state), conclusion}]'
```

Merge only when every required check is `SUCCESS` (none `PENDING`/`IN_PROGRESS`/`FAILURE`).

## List review threads

Unresolved threads with their comments and the file/line they target:

```bash
gh api graphql -f query='
query($owner:String!,$repo:String!,$num:Int!){
  repository(owner:$owner,name:$repo){
    pullRequest(number:$num){
      reviewDecision
      reviewThreads(first:100){
        nodes{
          id isResolved isOutdated
          comments(first:20){ nodes{ author{login} body path line } }
        }
      }
    }
  }
}' -F owner=<owner> -F repo=<repo> -F num=<num> \
  --jq '.data.repository.pullRequest.reviewThreads.nodes[] | select(.isResolved==false) | {id, file: .comments.nodes[0].path, line: .comments.nodes[0].line, body: .comments.nodes[-1].body, author: .comments.nodes[0].author.login}'
```

Each `id` is the `threadId` used to resolve. Skip threads authored by your own
bot account that are just status/disclosure notes.

## Reply to a thread

```bash
gh api graphql -f query='
mutation($threadId:ID!,$body:String!){
  addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$threadId, body:$body}){
    comment{ id }
  }
}' -F threadId=<threadId> -F body="Fixed in <sha>: <one-line summary>."
```

## Resolve a thread

```bash
gh api graphql -f query='
mutation($threadId:ID!){
  resolveReviewThread(input:{threadId:$threadId}){ thread{ isResolved } }
}' -F threadId=<threadId>
```

## Dismiss a review (for an unreasonable change request)

First get the review id (the `CHANGES_REQUESTED` review you want to dismiss):

```bash
gh api repos/<owner>/<repo>/pulls/<num>/reviews \
  --jq '.[] | select(.state=="CHANGES_REQUESTED") | {id, user: .user.login, body}'
```

Then dismiss it **with a mandatory justification message**:

```bash
gh api graphql -f query='
mutation($reviewId:ID!,$message:String!){
  dismissPullRequestReview(input:{pullRequestReviewId:$reviewId, message:$message}){
    pullRequestReview{ state }
  }
}' -F reviewId=<reviewNodeId> -F message="Dismissing: <clear reason this change request is incorrect / out of scope>."
```

Note: `dismissPullRequestReview` takes the review's **node id** (`PRR_…`), which
the GraphQL reviews query returns; the REST `id` above is numeric. To get the
node id directly:

```bash
gh api graphql -f query='
query($owner:String!,$repo:String!,$num:Int!){
  repository(owner:$owner,name:$repo){ pullRequest(number:$num){
    reviews(first:50){ nodes{ id state author{login} } } } }
}' -F owner=<owner> -F repo=<repo> -F num=<num> \
  --jq '.data.repository.pullRequest.reviews.nodes[] | select(.state=="CHANGES_REQUESTED")'
```

Dismissing requires write access and does not delete the review — it marks it
non-blocking and records your message. Never dismiss without a reason.

## Push fixes

```bash
git add -A && git commit -m "fix: <addresses review comment>"
git push
```

If the push is rejected as non-fast-forward, `git pull --rebase` first, then push
again. Never `git push --force` to a shared PR branch.

## Re-request review

```bash
gh pr edit <num> --add-reviewer <login>
```

## Merge

```bash
# merge now (requirements already met):
gh pr merge <num> --squash --delete-branch

# or queue auto-merge (GitHub merges as soon as approvals + checks pass):
gh pr merge <num> --squash --auto
```

Use `--merge` or `--rebase` instead of `--squash` if that is the repo's
convention. Do **not** use `--admin` to bypass failing checks.

## Unattended alternative

If the user wants takeover to keep running after they close their terminal,
Looper's daemon can own the PR instead — it runs the reviewer + fixer loops in
the background:

```bash
looper takeover <owner>/<repo>#<num> --merge
looper takeover list
looper takeover stop <owner>/<repo>#<num>
```

This requires installing Looper (`looperd`) and is the heavier, unattended path.
The agent-driven loop in `SKILL.md` needs nothing but `gh` + `git` and the user's
own running agent.
